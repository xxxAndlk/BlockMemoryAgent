package bootstrap

// memory_consolidate.go 块记忆库整理（2026-10-13，对齐经验技能库整理 C 库存治理）。
//
// 动机：SessionEvolver/saveBlockMemory 每次任务沉淀 0-5 条块记忆且只增不并，长期
// 膨胀稀释【沉淀索引】槽（实证 2026-10-13：global_knowledge 344 条激活块记忆，索引
// 超 200 行配额被截断，且大量教材改写近重复条目与当前会话无关也被注入）。索引块
// 的"请整理合并旧沉淀条目后重写索引"指令此前无任何执行路径（死结）——本文件把清理
// 闭环到系统自身：达阈值后由轻量模型整理一轮，产出合并/归档方案 → judge 复核误并 →
// 代码逐条校验应用。
//
// 纪律（同 skill_consolidate + append-only 底账约定）：
//   - 只动 block_memory 且 archived=false 的行；合并=保留者内容重写+重嵌入、来源
//     archived=true（冷备不删，SQL archived=true 可查，非物理删除）；
//   - 归档动作仅限 access_count==0（被召回过的一律不自动归档）；合并来源不受此限
//     （内容并入保留者，信息不丢）；
//   - 合并前经 judge（judgeMemoryDuplicate）逐来源复核，防整理模型误并——judge 明确
//     判"不重复"的来源跳过，judge 失败/超时保守按重复处理（与技能库整理同档策略）；
//   - 全部动作写 evolution_log（block_memory_merge / block_memory_archive /
//     memory_consolidate_run）可审计。

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// memoryConsolidateTimeout 整理单次轻量模型调用预算（与技能库整理同档）。
const memoryConsolidateTimeout = 90 * time.Second

// memoryConsolidateMaxMerges / memoryConsolidateMaxArchives 单轮动作上限（防模型输出失控）。
const (
	memoryConsolidateMaxMerges   = 10
	memoryConsolidateMaxArchives = 20
)

// memoryConsolidateMaxGroupSources 单组合并的来源数上限（每来源一次 judge 复核调用，
// 防个别超大批次把轻量模型调用次数放大）。
const memoryConsolidateMaxGroupSources = 8

// memoryConsolidateMaxEntries 送模型整理的条目数上限（超出取最新 N 条；本轮之外的
// 条目交给后续轮次——每日跑，合并会让计数持续下降）。600×~190 runes 的输入对轻量
// 模型（256K 上下文档）留足余量。
const memoryConsolidateMaxEntries = 600

// memoryConsolidateExcerptRunes / memoryConsolidateMergedRunes 单条摘要与合并后正文
// 的 rune 上限（合并后正文超限截断并标注，防个别条目滚雪球）。
const (
	memoryConsolidateExcerptRunes = 160
	memoryConsolidateMergedRunes  = 1000
)

// blockMemoryStoreOps 整理所需的最小块记忆能力（*store.KnowledgeStore 满足；接口便于测试桩）。
type blockMemoryStoreOps interface {
	ListAll(ctx context.Context) ([]*types.KnowledgeRecord, error)
	UpdateContentEmbedding(ctx context.Context, id int64, content string, embedding []float32) error
	Archive(ctx context.Context, id int64) error
	Embed(ctx context.Context, text string) ([]float32, error)
}

// evolutionLogger 进化审计流水写入（*store.LearnedSkillStore 满足，evolution_log 表通用）。
type evolutionLogger interface {
	AppendEvolutionLog(ctx context.Context, kind, target, summary, sourceSession string) error
}

// memoryConsolidator 块记忆库整理器。
type memoryConsolidator struct {
	mem       blockMemoryStoreOps
	evo       evolutionLogger
	factory   *model.ModelFactory
	threshold int // 激活块记忆数达到该值才自动整理；<=0 关闭自动（手动触发不受限）
	// judge 合并来源判重（judgeMemoryDuplicate 的注入位，单测以 fake 覆盖"拒并/
	// judge 失败"路径；nil=未接线按保守合并处理——宁可合并不灌水）。
	judge func(ctx context.Context, keep, src *types.KnowledgeRecord) (duplicateJudgeResult, error)
	mu    sync.Mutex
}

func newMemoryConsolidator(mem blockMemoryStoreOps, evo evolutionLogger, factory *model.ModelFactory, threshold int) *memoryConsolidator {
	c := &memoryConsolidator{mem: mem, evo: evo, factory: factory, threshold: threshold}
	c.judge = func(ctx context.Context, keep, src *types.KnowledgeRecord) (duplicateJudgeResult, error) {
		return judgeMemoryDuplicate(ctx, factory, keep, src)
	}
	return c
}

// memoryMergeGroup 一条合并方案：keep 保留者 id、merge 被并入者 id、merged 合并后
// 完整表述（信息并集）、note 合并理由。
type memoryMergeGroup struct {
	Keep   int64   `json:"keep"`
	Merge  []int64 `json:"merge"`
	Merged string  `json:"merged"`
	Note   string  `json:"note"`
}

// memoryArchiveItem 一条归档方案（仅限 access_count==0 的条目，code 侧硬校验）。
type memoryArchiveItem struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// memoryConsolidatePlan 轻量模型产出的整理方案（id 为 global_knowledge 行号）。
type memoryConsolidatePlan struct {
	Merges   []memoryMergeGroup  `json:"merges"`
	Archives []memoryArchiveItem `json:"archives"`
}

// Run 执行一轮整理，返回人类可读摘要（跳过原因也在此）。manual=true 时忽略阈值
//（手动端点触发）；自动（每日 tick）时低于阈值直接跳过。
func (c *memoryConsolidator) Run(ctx context.Context, manual bool) (string, error) {
	if c == nil || c.mem == nil || c.factory == nil {
		return "", fmt.Errorf("memory consolidator not wired")
	}
	if !c.mu.TryLock() {
		return "已有记忆整理任务在执行，本次跳过", nil
	}
	defer c.mu.Unlock()
	active, err := c.listMemories(ctx)
	if err != nil {
		return "", fmt.Errorf("list block memories: %w", err)
	}
	if !manual && (c.threshold <= 0 || len(active) < c.threshold) {
		return fmt.Sprintf("块记忆 %d 条未达整理阈值 %d，跳过", len(active), c.threshold), nil
	}
	if len(active) < 2 {
		return fmt.Sprintf("激活块记忆仅 %d 条，无需整理", len(active)), nil
	}
	sent := active
	if len(sent) > memoryConsolidateMaxEntries {
		sent = sent[len(sent)-memoryConsolidateMaxEntries:] // 取最新一段（ListAll 按 id 升序）
	}
	plan, err := c.plan(ctx, sent)
	if err != nil {
		return "", err
	}
	groups, sources, archived := c.apply(ctx, sent, plan)
	summary := fmt.Sprintf("记忆整理完成：合并 %d 组（归档来源 %d 条）、归档 %d 条零访问记忆；激活块记忆 %d → %d 条",
		groups, sources, archived, len(active), len(active)-sources-archived)
	_ = c.appendLog(ctx, "memory_consolidate_run", "block_memory", summary)
	log.Printf("[memory-consolidate] %s", summary)
	return summary, nil
}

// listMemories 取全部激活块记忆（ListAll 全类型后过滤 block_memory；≤ 万级一次扫完）。
func (c *memoryConsolidator) listMemories(ctx context.Context) ([]*types.KnowledgeRecord, error) {
	all, err := c.mem.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*types.KnowledgeRecord, 0, len(all))
	for _, r := range all {
		if r == nil || r.Archived || r.KnowledgeType != enums.KnowledgeTypeBlockMemory {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// plan 调轻量模型产出合并/归档方案（输入为条目 id/访问次数/创建时间/内容摘要）。
func (c *memoryConsolidator) plan(ctx context.Context, memories []*types.KnowledgeRecord) (*memoryConsolidatePlan, error) {
	var b strings.Builder
	for _, m := range memories {
		created := ""
		if !m.CreatedAt.IsZero() {
			created = m.CreatedAt.Format("2006-01-02")
		}
		excerpt := truncateForPrompt(strings.Join(strings.Fields(m.Content), " "), memoryConsolidateExcerptRunes)
		fmt.Fprintf(&b, "- id: %d | 访问: %d | 创建: %s | 内容: %s\n", m.ID, m.AccessCount, created, excerpt)
	}
	prompt := fmt.Sprintf(`你是块记忆库整理器。下面是各任务沉淀下来的块记忆条目（id/被召回次数/创建日期/内容）。
找出语义近重复的条目（同一事实的多种表述、互为子集/特例）提出合并方案，找出低价值陈旧条目提出归档方案。

规则:
- 只处理下面列出的 id，id 必须逐字来自列表
- merges: 同一事实的近重复组，keep 选信息覆盖最全的一条；merged 是合并后的一条完整表述（保留各组信息并集、去掉重复表述，可精炼，但不得丢失任何一条里的独有信息）；merge 列出被并入者（至少 1 个）
- archives: 仅限被召回次数为 0 且（已并入别处 / 一次性任务的琐碎细节无跨任务复用价值 / 被更完整条目取代）的条目
- 语义不同、各有价值的条目不要动；宁少勿多，没有可整理的输出空数组
- 输出 JSON 对象（不要 markdown 围栏）:
{"merges":[{"keep":1,"merge":[2,3],"merged":"合并后的完整表述","note":"合并理由"}],"archives":[{"id":4,"reason":"归档理由"}]}

块记忆列表:
%s

现在输出 JSON：`, b.String())
	pctx, cancel := context.WithTimeout(ctx, memoryConsolidateTimeout)
	defer cancel()
	resp, err := c.factory.CallLightweightWithRetry(pctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return parseMemoryConsolidatePlan(resp)
}

// parseMemoryConsolidatePlan 解析整理方案 JSON（parseStrictJSON 共用实现：剥围栏/严格解析）。
func parseMemoryConsolidatePlan(resp string) (*memoryConsolidatePlan, error) {
	var plan memoryConsolidatePlan
	if err := parseStrictJSON(resp, &plan); err != nil {
		return nil, fmt.Errorf("parse memory consolidate plan: %w", err)
	}
	return &plan, nil
}

// apply 校验并应用整理方案；返回（合并组数, 归档来源数, 归档数）。
func (c *memoryConsolidator) apply(ctx context.Context, memories []*types.KnowledgeRecord, plan *memoryConsolidatePlan) (int, int, int) {
	byID := make(map[int64]*types.KnowledgeRecord, len(memories))
	for _, m := range memories {
		byID[m.ID] = m
	}
	used := map[int64]bool{} // 已被方案占用的条目（保留者/合并来源/归档）
	groups, sources, archived := 0, 0, 0
	// 合并：保留者内容重写为 merged 并重嵌入，来源归档（archived=true，冷备可查）。
	for _, g := range plan.Merges {
		if groups >= memoryConsolidateMaxMerges {
			break
		}
		keep := byID[g.Keep]
		merged := strings.TrimSpace(g.Merged)
		if keep == nil || used[g.Keep] || merged == "" || len(g.Merge) > memoryConsolidateMaxGroupSources {
			continue
		}
		var srcs []*types.KnowledgeRecord
		valid := true
		for _, id := range g.Merge {
			src := byID[id]
			if src == nil || id == g.Keep || used[id] {
				valid = false
				break
			}
			srcs = append(srcs, src)
		}
		if !valid || len(srcs) == 0 {
			continue
		}
		// judge 复核（防整理模型误并，对齐技能库 B4 mergeOne）：逐来源判定，"不重复"
		// 的来源从合并集合剔除（保持激活可召回）；judge 失败/超时/未接线保守按重复处理；
		// 全部来源被拒则整组放弃（keep 不动，零副作用）。注：merged 是整理模型对整组
		// 合成的文本，被拒来源的独有信息可能仍被合成进去——故对被拒来源的保护目标是
		// "不归档不消失"，其信息最多在 keep 中复述，不丢失。
		srcs = c.confirmMergeSources(ctx, keep, srcs)
		if len(srcs) == 0 {
			log.Printf("[memory-consolidate] merge aborted: all sources rejected by judge (keep=%d)", g.Keep)
			continue
		}
		merged = truncateForPrompt(merged, memoryConsolidateMergedRunes)
		emb, err := c.mem.Embed(ctx, merged)
		if err != nil {
			log.Printf("[memory-consolidate] embed merged failed: keep=%d err=%v", g.Keep, err)
			continue
		}
		if err := c.mem.UpdateContentEmbedding(ctx, keep.ID, merged, emb); err != nil {
			log.Printf("[memory-consolidate] update keep failed: keep=%d err=%v", g.Keep, err)
			continue
		}
		used[g.Keep] = true
		mergedIDs := make([]string, 0, len(srcs))
		for _, src := range srcs {
			used[src.ID] = true
			mergedIDs = append(mergedIDs, strconv.FormatInt(src.ID, 10))
			if err := c.mem.Archive(ctx, src.ID); err != nil {
				log.Printf("[memory-consolidate] archive merged source failed: id=%d err=%v", src.ID, err)
				continue
			}
			sources++
		}
		groups++
		_ = c.appendLog(ctx, "block_memory_merge", strconv.FormatInt(keep.ID, 10),
			fmt.Sprintf("并入 id [%s]（%s）：%s", strings.Join(mergedIDs, ","), truncateForPrompt(g.Note, 80), truncateForPrompt(merged, 150)))
		log.Printf("[memory-consolidate] merged: keep=%d sources=%d", keep.ID, len(srcs))
	}
	// 归档：仅零访问条目（安全下限），冷备不删。
	for _, a := range plan.Archives {
		if archived >= memoryConsolidateMaxArchives {
			break
		}
		rec := byID[a.ID]
		if rec == nil || used[a.ID] || rec.AccessCount > 0 {
			continue
		}
		if err := c.mem.Archive(ctx, rec.ID); err != nil {
			log.Printf("[memory-consolidate] archive failed: id=%d err=%v", a.ID, err)
			continue
		}
		used[a.ID] = true
		archived++
		_ = c.appendLog(ctx, "block_memory_archive", strconv.FormatInt(rec.ID, 10),
			fmt.Sprintf("（访问=0）：%s | 内容: %s", truncateForPrompt(a.Reason, 80), truncateForPrompt(rec.Content, 150)))
	}
	return groups, sources, archived
}

// confirmMergeSources 合并前逐来源经 judge 复核（对齐技能库 B4 mergeOne）：judge 明确
// 判"不重复"的来源从合并集合剔除（保持激活）；judge 失败/超时或未接线一律保守按重复
// 处理（维持合并，宁可合并不灌水）。返回空切片的调用方必须放弃整组（keep 零副作用）。
func (c *memoryConsolidator) confirmMergeSources(ctx context.Context, keep *types.KnowledgeRecord, srcs []*types.KnowledgeRecord) []*types.KnowledgeRecord {
	if c.judge == nil {
		return srcs
	}
	confirmed := make([]*types.KnowledgeRecord, 0, len(srcs))
	for _, src := range srcs {
		res, err := c.judge(ctx, keep, src)
		if err != nil {
			log.Printf("[memory-consolidate] merge judge failed (conservative merge): keep=%d src=%d err=%v", keep.ID, src.ID, err)
			confirmed = append(confirmed, src)
			continue
		}
		if !res.Duplicate {
			log.Printf("[memory-consolidate] merge judge rejected (skip): keep=%d src=%d reason=%s", keep.ID, src.ID, res.Reason)
			continue
		}
		confirmed = append(confirmed, src)
	}
	return confirmed
}

// appendLog 写审计流水（失败仅日志，不阻断整理）。
func (c *memoryConsolidator) appendLog(ctx context.Context, kind, target, summary string) error {
	if c.evo == nil {
		return nil
	}
	if err := c.evo.AppendEvolutionLog(ctx, kind, target, summary, ""); err != nil {
		log.Printf("[memory-consolidate] evolution log failed (non-fatal): kind=%s err=%v", kind, err)
		return err
	}
	return nil
}

// judgeMemoryDuplicate 合并前逐来源复核（对齐技能库 judgeDuplicateMerge）：轻量模型
// 判断两条块记忆是否同一事实的近重复。调用失败返回 error，调用方保守按重复处理
//（维持合并，与技能库整理同档策略）。
func judgeMemoryDuplicate(ctx context.Context, factory *model.ModelFactory, keep, src *types.KnowledgeRecord) (duplicateJudgeResult, error) {
	if factory == nil {
		return duplicateJudgeResult{}, fmt.Errorf("memory judge not wired")
	}
	if keep == nil || src == nil {
		return duplicateJudgeResult{}, fmt.Errorf("judge records are nil")
	}
	prompt := fmt.Sprintf(`你是块记忆判重器。判断"保留记忆"与"候选记忆"是否为同一事实的近重复（语义相同，或一个是另一个的子集/特例）。
规则：只有描述同一事实/同一工艺口径才算重复；仅主题相近但事实不同不算重复。
只输出 JSON（不要 markdown 围栏）：{"duplicate": true, "merge_into": "keep", "reason": "判定理由"}

保留记忆：
{"id": %d, "内容": %q}

候选记忆：
{"id": %d, "内容": %q}

现在输出 JSON：`,
		keep.ID, truncateForPrompt(keep.Content, 300), src.ID, truncateForPrompt(src.Content, 300))
	jctx, cancel := context.WithTimeout(ctx, skillJudgeTimeout)
	defer cancel()
	resp, err := factory.CallLightweightWithRetry(jctx, prompt)
	if err != nil {
		return duplicateJudgeResult{}, err
	}
	return parseDuplicateJudge(resp)
}
