package memory

// context_engine.go ContextEngine 分档槽位（TODO #22①，对标 OpenClaw ownsCompaction
// 契约子集 + Hermes Phase 1 纯裁剪 + LCM 摘要 DAG 参数）。
//
// 三档装配：
//   - fast（快速档）= 纯裁剪引擎：零 LLM——旧 tool result >200 字符→stub+截断（Hermes
//     Phase 1 同款），压缩侧永不调 LLM（⑤ 零 LLM 裁剪验收的生效判据）；
//   - daily（日常档）= safeguard 单摘要引擎：现状管线原样（handoff 摘要三件套 +
//     qualityGuard 空摘要重试 + #20② 钩子），零行为变化；
//   - cluster（集群档）= LCM 式摘要 DAG：leaf 包 ~800–1,200 tok / condensed 包
//     ~1,500–2,000 tok、fanout 8/4、常驻 30–100K、大文件 >25K tok 外置换 ~200 tok
//     探查摘要（"全文请 ReadFile"指针化）。
//
// 失败隔离（QuarantineEngine）：引擎 panic/契约失败→该 Agent 隔离并降级 daily 引擎，
// 报错留痕（agent_events type=engine_quarantine + slog.Error）不静默。
//
// 五钩子（OpenClaw 子集；BMA 语义标注）：
//   - Assemble：请求视图变换（内容替换、条数不变——TailStart 下标语义不受影响）；
//   - Compact：超预算压缩推进（各档风格参数驱动共享金字塔实现）；
//   - AfterTurn：轮末微压缩（fast 的 stub 修剪在此跑，Assemble 尾部每轮调用）；
//   - PrepareSubagentSpawn：派发前子任务上下文注记（fork 预算硬顶归 #22② announce）；
//   - OnSubagentEnded：子回传摄入整形（回灌预算归 #22② announce 预算公式）。
//
// 单位约定：本包 tok/rune 混排处按 CJK 1 tok ≈ 1 rune 粗估（与 EstimateTokens 同口径），
// 截断阈值一律 rune 计。

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// ContextEngine 上下文引擎槽位契约（TODO #22①，OpenClaw 9 钩子只取五钩子子集）。
// Assemble/AfterTurn 只允许内容替换、不允许增删消息条数（TailStart 下标依赖）。
type ContextEngine interface {
	Name() string
	Assemble(agentID string, history []agent.ReactMessage) []agent.ReactMessage
	Compact(agentID string, history []agent.ReactMessage)
	AfterTurn(agentID string, history []agent.ReactMessage) []agent.ReactMessage
	PrepareSubagentSpawn(agentID, task string) string
	OnSubagentEnded(agentID, result string) string
}

// -- 快速档：纯裁剪引擎（零 LLM）--

// fastStubRunes 旧 tool result 超此长度即 stub 化（Hermes Phase 1 的 200 字符同款）。
const fastStubRunes = 200

// FastTrimEngine 快速档纯裁剪引擎：旧工具输出 stub+截断、压缩走截断包，全程零 LLM。
type FastTrimEngine struct{ p *Pipeline }

// NewFastTrimEngine 创建快速档引擎（p 可为 nil——仅测试 stub 纯函数路径）。
func NewFastTrimEngine(p *Pipeline) *FastTrimEngine { return &FastTrimEngine{p: p} }

func (e *FastTrimEngine) Name() string { return "fast_trim" }

// Threshold 压缩触发阈值：沿用全局阈值（stub 修剪已把历史压小，触发即走零 LLM 截断包）。
func (e *FastTrimEngine) Threshold(string) int { return 0 }

func (e *FastTrimEngine) Assemble(_ string, history []agent.ReactMessage) []agent.ReactMessage {
	return history
}

// AfterTurn 轮末微压缩：旧 tool result >200 runes → stub+截断；近保留段不动。
func (e *FastTrimEngine) AfterTurn(_ string, history []agent.ReactMessage) []agent.ReactMessage {
	keep := defaultKeepRecent
	if e.p != nil && e.p.compressKeepRecent > 0 {
		keep = e.p.compressKeepRecent
	}
	return stubOldToolResults(history, fastStubRunes, keep)
}

// Compact 压缩推进：截断包（truncateSegment），无 LLM、无合并烧包（fanout 用 maxBundles）。
func (e *FastTrimEngine) Compact(agentID string, history []agent.ReactMessage) {
	if e.p == nil {
		return
	}
	e.p.advanceCompressionStyled(agentID, history, compactStyle{useLLM: false})
}

// PrepareSubagentSpawn 快速档派发注记：告知子任务"上下文只留摘录"。
func (e *FastTrimEngine) PrepareSubagentSpawn(_, _ string) string {
	return "【上下文纪律】快速档执行：历史工具输出超 200 字符只留摘录，需要全文请重新 ReadFile。"
}

// OnSubagentEnded 回传摄入整形：回传正文超 stub 阈值时保留结论头 + 统计尾（预算收口归 announce）。
func (e *FastTrimEngine) OnSubagentEnded(_, result string) string { return result }

// -- 日常档：safeguard 单摘要引擎（现状管线，零行为变化）--

// SafeguardEngine 日常档引擎：handoff 摘要三件套 + qualityGuard + #20② 钩子，
// 全部走 Pipeline 默认实现（advanceCompression 默认风格）。
type SafeguardEngine struct{ p *Pipeline }

// NewSafeguardEngine 创建日常档引擎。
func NewSafeguardEngine(p *Pipeline) *SafeguardEngine { return &SafeguardEngine{p: p} }

func (e *SafeguardEngine) Name() string { return "daily_safeguard" }
func (e *SafeguardEngine) Threshold(string) int { return 0 }
func (e *SafeguardEngine) Assemble(_ string, h []agent.ReactMessage) []agent.ReactMessage {
	return h
}
func (e *SafeguardEngine) AfterTurn(_ string, h []agent.ReactMessage) []agent.ReactMessage {
	return h
}
func (e *SafeguardEngine) Compact(agentID string, history []agent.ReactMessage) {
	if e.p == nil {
		return
	}
	e.p.advanceCompressionStyled(agentID, history, defaultCompactStyle())
}
func (e *SafeguardEngine) PrepareSubagentSpawn(_, _ string) string   { return "" }
func (e *SafeguardEngine) OnSubagentEnded(_, result string) string  { return result }

// -- 集群档：LCM 式摘要 DAG 引擎 --

// DAGEngine 集群档引擎（LCM 参数见文件头）。
type DAGEngine struct{ p *Pipeline }

// NewDAGEngine 创建集群档引擎。
func NewDAGEngine(p *Pipeline) *DAGEngine { return &DAGEngine{p: p} }

func (e *DAGEngine) Name() string { return "cluster_dag" }

// Threshold 常驻上界：候选视图 ≥100K runes 即压缩（比全局 150K 更早，常驻带 30–100K）。
func (e *DAGEngine) Threshold(string) int { return dagResidentMaxRunes }

// dagCompactStyle 集群档压缩风格（leaf/condensed/fanout/常驻/探查摘要）。
func (e *DAGEngine) style() compactStyle {
	return compactStyle{
		useLLM:           true,
		leafCapRunes:     dagLeafCapRunes,
		condCapRunes:     dagCondCapRunes,
		fanout:           dagFanout,
		mergeK:           dagMergeK,
		probeBigFile:     true,
		bigFileRunes:     dagBigFileRunes,
		probeRunes:       dagProbeRunes,
		residentMinRunes: dagResidentMinRunes,
	}
}

// LCM 摘要 DAG 参数（tok≈rune 口径）。
const (
	dagLeafCapRunes     = 1200 // leaf 包上限（目标带 800–1,200）
	dagCondCapRunes     = 2000 // condensed 包上限（目标带 1,500–2,000）
	dagFanout           = 8    // 超过 8 个包触发合并（LCM fanout 8/4）
	dagMergeK           = 4    // 一次合并最老 4 个
	dagResidentMinRunes = 30000
	dagResidentMaxRunes = 100000
	dagBigFileRunes     = 25000 // 大文件阈值：>25K tok 外置换探查摘要
	dagProbeRunes       = 200   // 探查摘要目标 ~200 tok
)

func (e *DAGEngine) Assemble(agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	keep := defaultKeepRecent
	if e.p != nil && e.p.compressKeepRecent > 0 {
		keep = e.p.compressKeepRecent
	}
	// 大文件外置：旧区域 >25K runes 的工具输出换 ~200 rune 探查摘要（近保留段不动）。
	return probeBigFiles(history, dagBigFileRunes, dagProbeRunes, keep)
}

func (e *DAGEngine) AfterTurn(_ string, h []agent.ReactMessage) []agent.ReactMessage { return h }

func (e *DAGEngine) Compact(agentID string, history []agent.ReactMessage) {
	if e.p == nil {
		return
	}
	e.p.advanceCompressionStyled(agentID, history, e.style())
}

// PrepareSubagentSpawn 集群档派发注记：摘要 DAG 语义告知。
func (e *DAGEngine) PrepareSubagentSpawn(_, _ string) string {
	return "【上下文纪律】集群档执行：早期对话已折叠为摘要包，大段文件内容为探查摘要（~200 tok），全文请 ReadFile。"
}

func (e *DAGEngine) OnSubagentEnded(_, result string) string { return result }

// -- 内容替换纯函数（条数不变契约）--

// stubOldToolResults 旧 tool result 超 maxRunes → stub+截断（Hermes Phase 1）。
// 近 keep 条消息不动（Agent 需要看到最近读取的完整内容）。
// 幂等：已裁剪行跳过（防每轮重复改写破坏前缀缓存）。
func stubOldToolResults(history []agent.ReactMessage, maxRunes, keep int) []agent.ReactMessage {
	if len(history) == 0 {
		return history
	}
	protect := len(history) - keep
	if protect < 0 {
		protect = 0
	}
	out := make([]agent.ReactMessage, len(history))
	copy(out, history)
	for i := 0; i < protect; i++ {
		m := out[i]
		if m.Role != "tool" || strings.Contains(m.Content, stubMarker) {
			continue
		}
		r := []rune(m.Content)
		if len(r) <= maxRunes {
			continue
		}
		m.Content = string(r[:maxRunes]) + fmt.Sprintf("%s（原 %d 字符；全文请重新调用该工具或 ReadFile）", stubMarker, len(r))
		out[i] = m
	}
	return out
}

const stubMarker = "…（工具输出已裁剪"

// probeBigFiles 旧区域 >bigRunes 的工具输出换探查摘要（首尾各半 + 指针），近 keep 条不动。
// 幂等：已是探查摘要跳过（内容已远小于阈值，防御重入）。
func probeBigFiles(history []agent.ReactMessage, bigRunes, probeRunes, keep int) []agent.ReactMessage {
	if len(history) == 0 {
		return history
	}
	protect := len(history) - keep
	if protect < 0 {
		protect = 0
	}
	out := make([]agent.ReactMessage, len(history))
	copy(out, history)
	for i := 0; i < protect; i++ {
		m := out[i]
		if m.Role != "tool" || strings.Contains(m.Content, "【探查摘要】") {
			continue
		}
		r := []rune(m.Content)
		if len(r) <= bigRunes {
			continue
		}
		head := probeRunes / 2
		tail := probeRunes - head
		m.Content = fmt.Sprintf("【探查摘要】大文件内容已外置（原 %d 字符）：\n%s\n…（中略）…\n%s\n【全文请 ReadFile 读取源文件】",
			len(r), string(r[:head]), string(r[len(r)-tail:]))
		out[i] = m
	}
	return out
}

// trimToRunes 超长截断（包体瘦身用；无省略语义损失提示，包体本就是摘要）。
// textutil 单源；max<=0 原样返回（与 TruncateRunes 的 <=0 返空语义不同，故保留守卫）。
func trimToRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	return textutil.TruncateRunes(s, max, "")
}

// -- 失败隔离：QuarantineEngine --

// QuarantineEngine 引擎隔离壳（TODO #22① 失败隔离）：inner panic/契约失败时按 agentID
// 隔离并降级 fallback（日常档），agent_events 落 engine_quarantine 留痕 + slog.Error 不静默。
type QuarantineEngine struct {
	p        *Pipeline
	innerFor func(agentID string) ContextEngine
	fallback ContextEngine

	mu         sync.Mutex
	quarantined map[string]bool
}

// NewQuarantineEngine 创建隔离壳：innerFor 按 agentID 解析档位引擎，fallback=降级目标（日常档）。
func NewQuarantineEngine(p *Pipeline, innerFor func(string) ContextEngine, fallback ContextEngine) *QuarantineEngine {
	return &QuarantineEngine{p: p, innerFor: innerFor, fallback: fallback, quarantined: map[string]bool{}}
}

func (q *QuarantineEngine) Name() string { return "quarantine" }

// Threshold 转发已解析引擎的阈值覆盖（隔离后走 fallback 的阈值）。
func (q *QuarantineEngine) Threshold(agentID string) int {
	return engineThreshold(q.resolve(agentID), agentID, 0)
}

func (q *QuarantineEngine) isQuarantined(agentID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.quarantined[agentID]
}

func (q *QuarantineEngine) quarantine(agentID, hook, reason string) {
	q.mu.Lock()
	first := !q.quarantined[agentID]
	q.quarantined[agentID] = true
	q.mu.Unlock()
	if !first {
		return
	}
	slog.Error("context engine quarantined, degrading to daily engine",
		"agent", agentID, "hook", hook, "reason", reason)
	if q.p != nil {
		_ = q.p.Write(agentID, agent.MemoryEvent{
			Type:    "engine_quarantine",
			AgentID: agentID,
			Content: hook,
			Output:  reason,
		})
	}
}

// resolve 返回 inner（已隔离则 fallback）。
func (q *QuarantineEngine) resolve(agentID string) ContextEngine {
	if q.isQuarantined(agentID) {
		return q.fallback
	}
	if q.innerFor != nil {
		if e := q.innerFor(agentID); e != nil {
			return e
		}
	}
	return q.fallback
}

// checkContract 契约检查：内容替换钩子条数必须不变（TailStart 下标依赖）。
func checkContract(in, out []agent.ReactMessage) string {
	if out == nil && len(in) > 0 {
		return "engine returned nil view for non-empty history"
	}
	if len(out) != len(in) {
		return fmt.Sprintf("engine changed message count (%d -> %d)", len(in), len(out))
	}
	return ""
}

func (q *QuarantineEngine) Assemble(agentID string, history []agent.ReactMessage) (result []agent.ReactMessage) {
	inner := q.resolve(agentID)
	defer func() {
		if r := recover(); r != nil {
			q.quarantine(agentID, "assemble", fmt.Sprintf("panic: %v", r))
			result = q.fallback.Assemble(agentID, history)
		}
	}()
	out := inner.Assemble(agentID, history)
	if msg := checkContract(history, out); msg != "" {
		q.quarantine(agentID, "assemble", msg)
		return q.fallback.Assemble(agentID, history)
	}
	return out
}

func (q *QuarantineEngine) AfterTurn(agentID string, history []agent.ReactMessage) (result []agent.ReactMessage) {
	inner := q.resolve(agentID)
	defer func() {
		if r := recover(); r != nil {
			q.quarantine(agentID, "afterTurn", fmt.Sprintf("panic: %v", r))
			result = q.fallback.AfterTurn(agentID, history)
		}
	}()
	out := inner.AfterTurn(agentID, history)
	if msg := checkContract(history, out); msg != "" {
		q.quarantine(agentID, "afterTurn", msg)
		return q.fallback.AfterTurn(agentID, history)
	}
	return out
}

func (q *QuarantineEngine) Compact(agentID string, history []agent.ReactMessage) {
	inner := q.resolve(agentID)
	defer func() {
		if r := recover(); r != nil {
			q.quarantine(agentID, "compact", fmt.Sprintf("panic: %v", r))
			q.fallback.Compact(agentID, history)
		}
	}()
	inner.Compact(agentID, history)
}

func (q *QuarantineEngine) PrepareSubagentSpawn(agentID, task string) (result string) {
	inner := q.resolve(agentID)
	defer func() {
		if r := recover(); r != nil {
			q.quarantine(agentID, "prepareSubagentSpawn", fmt.Sprintf("panic: %v", r))
			result = q.fallback.PrepareSubagentSpawn(agentID, task)
		}
	}()
	return inner.PrepareSubagentSpawn(agentID, task)
}

func (q *QuarantineEngine) OnSubagentEnded(agentID, result string) (out string) {
	inner := q.resolve(agentID)
	defer func() {
		if r := recover(); r != nil {
			q.quarantine(agentID, "onSubagentEnded", fmt.Sprintf("panic: %v", r))
			out = q.fallback.OnSubagentEnded(agentID, result)
		}
	}()
	return inner.OnSubagentEnded(agentID, result)
}

// thresholdOverrider 可选契约：引擎自带压缩触发阈值（0=用全局阈值）。
type thresholdOverrider interface{ Threshold(agentID string) int }

// engineThreshold 取引擎阈值覆盖；无覆盖/0 返回全局 defaultThreshold。
func engineThreshold(eng ContextEngine, agentID string, defaultThreshold int) int {
	if t, ok := eng.(thresholdOverrider); ok {
		if v := t.Threshold(agentID); v > 0 {
			return v
		}
	}
	return defaultThreshold
}
