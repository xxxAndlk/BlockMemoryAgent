package memory

// pyramid.go 层级压缩（summary pyramid）：替代旧的"中段整段暴力截断"。
//
// 机制（2026-08-28 设计结论）：
//   - 每次压缩触发（token 阈值，唯一闸门；步频扳机已退役）把"新滑出保留段"的中段历史
//     （history[prev.TailStart:newTailStart]）压成一个结构化压缩包追加到列表，
//     而不是像旧实现那样每次从全量 history 重算整段摘要并覆盖；
//   - 压缩包数量超 maxBundles 时，把最老的一半合并为 1 个更粗的包（LLM merge），
//     循环往复——旧上下文以逐级变粗的形式保留（渐变式遗忘），
//     而不是 200 字符截断后等同丢弃（悬崖式丢失）；
//   - 合并取"最老的一半"而非"最老 5 个压 1 个"：后者每次合并信息损失约 80%，
//     递归几次后早期内容只剩一句话；减半合并的深度增长更平缓；
//   - LLM 摘要不可用（未注入/超时/402 关停）时降级为截断式压缩（renderTruncated），
//     与旧行为一致，主流程永不因摘要失败中断；
//   - 压缩状态随触发落库（Store.SaveCompressState），进程重启后首次 Assemble 懒加载恢复；
//     TailStart 下标语义依赖全量 history 同时被恢复（MetaAgent 走 agent_messages）。
//
// 前缀缓存：压缩包列表在两次触发之间冻结，视图前缀字节级稳定（同旧冻结视图设计）；
// 触发那一轮前缀失效是不可避免的代价，但触发频率不变（token 阈值驱动）。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// DefaultMaxBundles 是每个 agent 压缩包数量默认上限：超限合并最老的一半。
// 20 个包 × 每包覆盖一个压缩周期（约 10 步）≈ 200+ 步上下文仍有粗粒度记忆。
const DefaultMaxBundles = 20

// defaultKeepRecent 压缩保留段默认条数（compressKeepRecent<=0 时的回落值）。
const defaultKeepRecent = 10

// maxMergedBundleRunes 是合并降级路径（LLM 不可用直接拼接）的单包最大 rune 数，
// 防拼接兜底时单包无限增长。LLM 合并路径由 prompt 约束篇幅（约 400 字）。
const maxMergedBundleRunes = 3000

// compactStyle 压缩风格参数（TODO #22① 三档分化）：daily 用 defaultCompactStyle()，
// cluster（DAGEngine）传 LCM 参数，fast 传 useLLM=false（截断包零 LLM）。
type compactStyle struct {
	useLLM           bool  // 允许 LLM 摘要（false=截断包，零 LLM）
	leafCapRunes     int   // leaf 包截断上限（0=不截）
	condCapRunes     int   // 合并（condensed）包截断上限（0=不限，走 maxMergedBundleRunes 兜底）
	fanout           int   // 包数合并触发阈值（0=用 maxBundles）
	mergeK           int   // 一次合并最老几个（0=最老一半，现有语义）
	probeBigFile     bool  // 大文件外置换探查摘要（LCM）
	bigFileRunes     int   // 大文件阈值
	probeRunes       int   // 探查摘要目标长度
	residentMinRunes int   // 常驻下界：压缩后保留段不足此量时多保一条消息（LCM 常驻 30K）
}

// defaultCompactStyle 日常档风格 = 现状管线语义（LLM 摘要 + 超限合并最老一半）。
func defaultCompactStyle() compactStyle {
	return compactStyle{useLLM: true}
}

// advanceCompression 现状语义入口（日常档/未接引擎路径）。
func (p *Pipeline) advanceCompression(agentID string, history []agent.ReactMessage) {
	p.advanceCompressionStyled(agentID, history, defaultCompactStyle())
}

// advanceCompressionStyled 执行一次层级压缩推进（风格参数化，TODO #22①）：
// 计算新保留段边界，把新滑出的中段压成一个压缩包追加到金字塔，超 fanout 合并
// 最老 mergeK 个，冻结新状态并落库。LLM 调用在锁外进行；并发重复触发最坏同包重压，无害。
func (p *Pipeline) advanceCompressionStyled(agentID string, history []agent.ReactMessage, style compactStyle) {
	firstUserIdx, recentStart, ok := compressBoundary(history, p.compressKeepRecent)
	if !ok {
		return
	}
	// 常驻下界（LCM 30K）：保留段 token 估算不足 min 时向前多保消息，防压缩后活跃段过薄。
	if style.residentMinRunes > 0 && p.tokenEstimator != nil {
		for recentStart > firstUserIdx+1 &&
			p.tokenEstimator(history[recentStart:]) < style.residentMinRunes {
			recentStart--
		}
	}

	p.mu.RLock()
	prev, hasPrev := p.compressStates[agentID]
	p.mu.RUnlock()

	// 增量压缩：只压"上次覆盖点之后、新保留段起点之前"的新滑出段。
	// 旧状态越界（history 被外部重建变短）时从头重压，防御性兜底。
	segStart := firstUserIdx + 1
	bundles := []string(nil)
	if hasPrev && prev.TailStart > segStart && prev.TailStart <= recentStart {
		segStart = prev.TailStart
		bundles = prev.Bundles
	}
	segment := history[segStart:recentStart]
	if len(segment) == 0 {
		// 无新内容滑出保留段：不推进（避免产生空压缩包）。
		return
	}

	// 压缩前状态快照（TODO #20②）：账本/名册/未读指针级摘要，随压缩事件落底账——
	// 压缩后可 SQL 回答"压掉了什么、账本当时在什么状态"（CC 压缩事故同类防护）。
	// 快照是"记录"；下方 HotReinject 是"解药"，两件配套。
	preSnap := ""
	if p.preCompactSnapshot != nil {
		preSnap = p.preCompactSnapshot(agentID)
	}

	// 大文件外置（LCM，TODO #22①）：leaf 摘要前把段内 >bigRunes 的工具输出换探查摘要，
	// 防整段大文件灌爆摘要 prompt。条数不变（内容替换）。
	// 注意：probeBigFiles 的 keep=0（段内全部替换，段本身就是滑出区）。
	if style.probeBigFile {
		segment = probeBigFiles(segment, style.bigFileRunes, style.probeRunes, 0)
	}

	bundle := p.summarizeSegmentStyled(agentID, segment, style)
	// 拷贝后追加，避免与并发读取共享底层数组。
	bundles = append(append([]string(nil), bundles...), bundle)
	fanout := style.fanout
	if fanout <= 0 {
		fanout = p.maxBundles
	}
	if fanout > 0 && len(bundles) > fanout {
		bundles = p.mergeOldestBundlesStyled(agentID, bundles, style)
	}

	// 压缩后热改动重读回灌（TODO #20②）：最近改动文件（≤5 个，>5K tokens 只回路径）
	// + 项目自述从盘重读，冻结进视图——未提交热改动不随压缩蒸发（CC 压缩回退事故解药）。
	hot := ""
	if p.hotReinjectProvider != nil {
		hot = p.hotReinjectProvider(agentID, agent.FilesModifiedFromHistory(history))
	}

	p.mu.Lock()
	p.compressStates[agentID] = compressState{Bundles: bundles, TailStart: recentStart, HotReinject: hot}
	p.mu.Unlock()
	p.persistCompressState(agentID)

	// 压缩审计事件（TODO #20②+#21③）：type=compaction 独立事件类型落 agent_events——
	// before/after 体积 + 摘要本体 + 压缩前快照。落库失败仅告警（观测数据不阻塞主流程）。
	p.writeCompactionEvent(agentID, bundle, preSnap, len(history), recentStart)
}

// writeCompactionEvent 落压缩审计事件（agent_events type=compaction，零 DDL——type 自由文本）。
// 字段映射：Input=压缩前快照、Content=摘要本体、Output=before/after 体积指标。
// 直写 store 不走 Write：审计行是 SQL 取证数据（"压掉了什么、账本当时什么状态"），
// 进内存事件流会被 injectEvents 渲染进【近期事件】尾巴——既污染上下文又每轮变字节
// 打断前缀缓存（TestPipeline_CompressionFrozenView 实证）。
func (p *Pipeline) writeCompactionEvent(agentID, bundle, preSnap string, beforeMsgs, afterTailStart int) {
	if p.store == nil {
		return
	}
	out := fmt.Sprintf("before_msgs=%d after_tail_start=%d bundle_runes=%d",
		beforeMsgs, afterTailStart, len([]rune(bundle)))
	p.mu.RLock()
	if st, ok := p.compressStates[agentID]; ok {
		out = fmt.Sprintf("before_msgs=%d after_tail_start=%d bundles=%d bundle_runes=%d",
			beforeMsgs, afterTailStart, len(st.Bundles), len([]rune(bundle)))
	}
	p.mu.RUnlock()
	ev := agent.MemoryEvent{
		Type:     "compaction",
		AgentID:  agentID,
		Content:  bundle,
		Input:    preSnap,
		Output:   out,
		Occurred: time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.SaveEvent(ctx, agentID, ev); err != nil {
		slog.Warn("pipeline: write compaction event failed", "agent_id", agentID, "err", err)
	}
}

// summarizeSegment 把一段中段历史压成一个压缩包：优先 LLM 结构化摘要，
// 未注入摘要器 / 402 已关停 / 调用失败或返空时降级为截断式压缩（旧行为）。
// 质量门（TODO #21③）：响应不含摘要块（返空）即重试一次再降级——
// KC v0.3.0 同款，防偶发空响应把整段历史打成截断包。
func (p *Pipeline) summarizeSegment(agentID string, segment []agent.ReactMessage) string {
	return p.summarizeSegmentStyled(agentID, segment, defaultCompactStyle())
}

// summarizeSegmentStyled 风格化压缩包生成（TODO #22①）：
// style.useLLM=false → 截断包（快速档零 LLM）；否则 LLM 摘要 + leafCap 截断收口。
func (p *Pipeline) summarizeSegmentStyled(agentID string, segment []agent.ReactMessage, style compactStyle) string {
	if style.useLLM && p.historySummarizer != nil && !p.summarizerDisabled() {
		timeout := p.summarizeTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		for attempt := 0; attempt < 2; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			text, err := p.historySummarizer(ctx, formatSegmentText(segment), false)
			cancel()
			if err != nil {
				p.noteSummarizeError(agentID, err)
				slog.Warn("pipeline: history bundle summarize failed, fallback to truncation",
					"agent_id", agentID, "segment_messages", len(segment), "err", err)
				break
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				p.noteSummarizeSuccess()
				return trimToRunes(trimmed, style.leafCapRunes)
			}
			// 空摘要：重试一次（质量门），仍空则落截断降级。
		}
	}
	return truncateSegment(segment)
}

// mergeOldestBundles 把压缩包列表最老的一半合并为 1 个更粗的包，返回新列表。
// k 至少为 2：合并 k 个为 1 个使总数净减 k-1，保证超上限（maxBundles+1）后收敛回上限内
// （k=1 时总数不变，小上限下永不收敛）。合并优先走 LLM（merge=true）；
// 不可用/失败时降级为直接拼接并截断兜底。
func (p *Pipeline) mergeOldestBundles(agentID string, bundles []string) []string {
	return p.mergeOldestBundlesStyled(agentID, bundles, defaultCompactStyle())
}

// mergeOldestBundlesStyled 风格化合并（TODO #22①）：style.mergeK 显式指定一次合并个数
// （cluster=4，LCM fanout 8/4）；0=最老一半（现状语义）。condCap 收口合并包长度。
func (p *Pipeline) mergeOldestBundlesStyled(agentID string, bundles []string, style compactStyle) []string {
	k := style.mergeK
	if k <= 0 {
		k = len(bundles) / 2
	}
	if k < 2 {
		k = 2
	}
	if k > len(bundles) {
		k = len(bundles)
	}
	oldest := bundles[:k]
	merged := ""
	if style.useLLM && p.historySummarizer != nil && !p.summarizerDisabled() {
		timeout := p.summarizeTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		// 质量门（TODO #21③）：空摘要重试一次再降级拼接——与 summarizeSegment 同款。
		for attempt := 0; attempt < 2; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			text, err := p.historySummarizer(ctx, joinNonEmpty("\n\n", oldest), true)
			cancel()
			if err != nil {
				p.noteSummarizeError(agentID, err)
				slog.Warn("pipeline: bundle merge failed, fallback to concat",
					"agent_id", agentID, "merged_bundles", k, "err", err)
				break
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				p.noteSummarizeSuccess()
				merged = trimToRunes(trimmed, style.condCapRunes)
				break
			}
		}
	}
	if merged == "" {
		// 降级：直接拼接最老的一半并截断兜底，保证包数收敛、单包不爆炸。
		merged = joinNonEmpty("\n\n", oldest)
		capRunes := maxMergedBundleRunes
		if style.condCapRunes > 0 && style.condCapRunes < capRunes {
			capRunes = style.condCapRunes
		}
		if r := []rune(merged); len(r) > capRunes {
			merged = string(r[:capRunes]) + "…"
		}
	}
	out := make([]string, 0, len(bundles)-k+1)
	out = append(out, merged)
	out = append(out, bundles[k:]...)
	return out
}

// formatSegmentText 渲染上限：单条 user/system/assistant 1000 runes、tool 800 runes；
// 段总量 maxSegmentTextRunes，超限从最老消息开始丢弃（保留最新）并加省略标记。
// 此前不截断单条（注释称"tool 输出入史时已截断到 tool_output_history_max_runes，长度天然
// 有界"），但 20000 runes/条 × ~30 条/压缩段 = 最多 60 万字符，远超 lightweight 摘要模型上下文
// ——压缩 LLM 每次 400（"Total tokens of image and text exceed max message tokens"），重试 3 次
// 后降级 truncateSegment，压缩摘要退化成无信息残桩，Agent 失忆反复重读同一文件烧墙钟
// （2026-08-28 渲染领域 Agent 两小时事故实证）。
const (
	segmentTextMsgRunes     = 1000
	segmentTextToolMsgRunes = 800
	maxSegmentTextRunes     = 12000
)

// formatSegmentText 把一段中段历史渲染为 LLM 压缩的输入文本。
// 单条按角色截断 + 段总量兜底截断：lightweight 摘要模型上下文有限，不截断必 400
// （见上常量注释）。截断只影响压缩输入，不动 history 本体。
func formatSegmentText(segment []agent.ReactMessage) string {
	lines := make([]string, 0, len(segment))
	for _, m := range segment {
		role := m.Role
		if role == "" {
			role = "?"
		}
		content := strings.TrimSpace(m.Content)
		if content == "" && len(m.ToolCalls) > 0 {
			content = fmt.Sprintf("[tool_calls: %d]", len(m.ToolCalls))
		}
		limit := segmentTextMsgRunes
		if role == "tool" {
			limit = segmentTextToolMsgRunes
		}
		if r := []rune(content); len(r) > limit {
			content = string(r[:limit]) + "…"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", role, content))
	}
	// 段总量 cap：从最老消息开始丢弃，保留最新（近期上下文细节价值最高）。
	total := 0
	for _, l := range lines {
		total += len([]rune(l)) + 1
	}
	dropped := 0
	for total > maxSegmentTextRunes && len(lines) > 1 {
		total -= len([]rune(lines[0])) + 1
		lines = lines[1:]
		dropped++
	}
	var sb strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&sb, "[… 最早 %d 条消息因压缩输入总量超限省略 …]\n", dropped)
	}
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
}

// truncateSegment 是压缩包的截断式降级：user 消息保前 500 字符，其余 200 字符。
func truncateSegment(segment []agent.ReactMessage) string {
	var sb strings.Builder
	renderTruncated(&sb, segment)
	return strings.TrimSpace(sb.String())
}

// renderBundles 把压缩包列表渲染为一条压缩摘要消息正文（从旧到新编号）。
// 同一列表渲染结果字节级稳定（DeepSeek 前缀缓存命中）。
func renderBundles(bundles []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "【历史压缩摘要】（共 %d 段，按时间从旧到新；越旧的段落覆盖越久、粒度越粗）\n", len(bundles))
	for i, b := range bundles {
		fmt.Fprintf(&sb, "\n▶ 第 %d 段", i+1)
		if i == 0 && len(bundles) > 1 {
			sb.WriteString("（最早期）")
		}
		sb.WriteString("：\n")
		sb.WriteString(b)
		sb.WriteString("\n")
	}
	sb.WriteString("\n（以上为早期对话的分层压缩摘要；需要被压缩段落的细节时可用工具重新读取相关文件，" +
		"或查 agent_events / session_events 事件流（历史轨迹 SQL 可查），近期上下文见下方最近消息）")
	return sb.String()
}

// Bundles 返回该 agent 压缩金字塔的摘要包链（leaf→condensed，时序旧→新）副本。
// 展开式召回（TODO #22④）用：只读展开器沿链定位细节所属段再取原文。无状态返回 nil。
func (p *Pipeline) Bundles(agentID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.compressStates[agentID]
	if !ok || len(st.Bundles) == 0 {
		return nil
	}
	return append([]string(nil), st.Bundles...)
}

// persistCompressState 把当前冻结的压缩状态落库（失败仅告警，不影响主流程）。
func (p *Pipeline) persistCompressState(agentID string) {
	if p.store == nil {
		return
	}
	p.mu.RLock()
	st, ok := p.compressStates[agentID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.store.SaveCompressState(ctx, agentID, &st); err != nil {
		slog.Warn("pipeline: persist compress state failed", "agent_id", agentID, "err", err)
	}
}

// loadCompressStateOnce 从 store 懒加载压缩状态（每 agent 只试一次，无论成败）。
// 加载结果校验：无数据 / TailStart 越过当前 history 长度（全量历史未恢复，下标语义无意义）
// / 无压缩包时一律丢弃，退化为不压缩。
func (p *Pipeline) loadCompressStateOnce(agentID string, historyLen int) (compressState, bool) {
	if p.store == nil {
		return compressState{}, false
	}
	p.mu.Lock()
	if p.compressLoaded[agentID] {
		p.mu.Unlock()
		return compressState{}, false
	}
	p.compressLoaded[agentID] = true
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	st, err := p.store.LoadCompressState(ctx, agentID)
	cancel()
	if err != nil {
		slog.Warn("pipeline: load compress state failed", "agent_id", agentID, "err", err)
		return compressState{}, false
	}
	if st == nil || len(st.Bundles) == 0 || st.TailStart > historyLen {
		return compressState{}, false
	}
	p.mu.Lock()
	p.compressStates[agentID] = *st
	p.mu.Unlock()
	return *st, true
}

// loadEventsOnce 从 store 懒加载历史事件（每 agent 只试一次，无论成败）。
// 重启后内存事件为空时恢复"近期事件"连续性；Store.LoadEvents 约定返回时间正序。
func (p *Pipeline) loadEventsOnce(agentID string) []agent.MemoryEvent {
	if p.store == nil {
		return nil
	}
	p.mu.Lock()
	if p.eventsLoaded[agentID] {
		p.mu.Unlock()
		return nil
	}
	p.eventsLoaded[agentID] = true
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	events, err := p.store.LoadEvents(ctx, agentID, p.maxEventsPerAgent)
	cancel()
	if err != nil {
		slog.Warn("pipeline: load events failed", "agent_id", agentID, "err", err)
		return nil
	}
	// 审计行（type=compaction/checkpoint）是 SQL 取证数据，不是上下文事件——懒加载过滤，
	// 否则会随【近期事件】渲染进尾巴（既占上下文又每轮变字节打断前缀缓存）。
	filtered := events[:0]
	for _, ev := range events {
		if !isContextHiddenEvent(ev.Type) {
			filtered = append(filtered, ev)
		}
	}
	events = filtered
	if len(events) == 0 {
		return nil
	}
	p.mu.Lock()
	// 双检：加载期间可能有新事件写入，保留较完整的一份（正常路径 len 为 0）。
	if len(p.events[agentID]) == 0 {
		p.events[agentID] = events
	}
	events = p.events[agentID]
	p.mu.Unlock()
	return events
}

// summarizerDisabled 报告摘要路径是否已被 402 熔断（连续 2 次 Insufficient Balance）。
func (p *Pipeline) summarizerDisabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.consecutiveBalanceErrors >= 2
}

// noteSummarizeSuccess 摘要成功：重置连续 402 计数。
func (p *Pipeline) noteSummarizeSuccess() {
	p.mu.Lock()
	p.consecutiveBalanceErrors = 0
	p.mu.Unlock()
}

// noteSummarizeError 摘要失败记账：检测到 Insufficient Balance（402）时累加连续计数，
// 达到 2 次熔断摘要路径直到进程重启（实证 DeepSeek 余额耗尽持续 402，重试无意义）。
// 事件摘要与历史压缩包共用同一熔断：两者用的是同一个轻量模型。
func (p *Pipeline) noteSummarizeError(agentID string, err error) {
	if strings.Contains(err.Error(), "Insufficient Balance") || strings.Contains(err.Error(), "402") {
		p.mu.Lock()
		p.consecutiveBalanceErrors++
		count := p.consecutiveBalanceErrors
		p.mu.Unlock()
		slog.Warn("pipeline: summarize got Insufficient Balance, will disable after 2 consecutive errors",
			"agent_id", agentID, "consecutive_count", count)
		if count >= 2 {
			slog.Warn("pipeline: summarizer disabled (Insufficient Balance x2), falling back until restart")
		}
	}
}
