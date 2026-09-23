// Package decision 决策层（TODO #23，Jev 范式：类型化决策 + 置信度分级 + 影子先行）。
//
// 定位：硬规则（沙箱/配额/幂等/审批/终止条件/FailureKind errors.Is/salvage 防污染闸）
// 永远留确定性代码；本层只做灰区判断与建议。三条铁律：
//  1. 候选集由代码持有（Choice 问题的 Candidates 是闭集，模型输出集外值即判无效）——结构性防幻觉；
//  2. 按后果分级置信度阈值（per-决策点 PointPolicy.MinConfidence，高后果动作配高门槛）；
//  3. 影子先行、对拍达标才晋级强制；永远保留旁路——provider 故障/超时/解析失败一律
//     返回错误，调用方按现状行为继续（fail-open），决策层故障不阻塞链路。
//
// 三原语：Choice（闭集选项）/ Score（0-1 打分）/ Noul（是/否判断）。每答案带 confidence。
package decision

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Primitive 决策原语类型。
type Primitive string

const (
	// PrimitiveChoice 闭集选项：Answer.Value 必须命中 Question.Candidates。
	PrimitiveChoice Primitive = "choice"
	// PrimitiveScore 0-1 相关性/质量打分：Answer.Score 承载数值。
	PrimitiveScore Primitive = "score"
	// PrimitiveNoul 是/否判断：Answer.Value 取 "yes"/"no"。
	PrimitiveNoul Primitive = "noul"
)

// 决策点名（晋级开关与影子行 role 的键）。逐点独立晋级，不做全量开关。
const (
	PointIntentKind      = "intent_kind"      // ①任务性质：速答/单agent/多域编排
	PointToolFace        = "tool_face"        // ①所需工具面
	PointNeedClarify     = "need_clarify"     // ①需澄清?
	PointFailureDisp     = "failure_disposition" // ②失败处置路由
	PointDispatchGap     = "dispatch_gap"     // ③派发门灰区需求信号
	PointUptakeScore     = "uptake_score"     // ④黑板摄取相关性
	PointExtractWorth    = "extract_worth"    // ⑤沉淀提取预判
	PointSalvageWorth    = "salvage_worth"    // ⑤打捞提取预判
	PointGearHint        = "gear_hint"        // ⑥档位建议（只读，永不做自动选档）
)

// Question 单个决策点的提问。Key 是本次 Request 内的唯一标识（同一点可批量多问，
// 如 uptake_score 逐候选打分）；Point 是决策点名（晋级/影子聚合键）。
type Question struct {
	Key        string    `json:"key"`
	Point      string    `json:"point"`
	Primitive  Primitive `json:"primitive"`
	Prompt     string    `json:"prompt"`
	Candidates []string  `json:"candidates,omitempty"` // Choice 闭集（代码持有）
	Context    string    `json:"context,omitempty"`    // 状态摘要片段
}

// Answer 单个问题的答案。Confidence 恒在 [0,1]。
type Answer struct {
	Key        string    `json:"key"`
	Point      string    `json:"point"`
	Primitive  Primitive `json:"primitive"`
	Value      string    `json:"value,omitempty"` // Choice 候选之一 / Noul "yes"|"no"
	Score      float64   `json:"score,omitempty"` // Score 原语数值 [0,1]
	Confidence float64   `json:"confidence"`
}

// Request 一次决策调用：状态快照 + 待决问题集（同批一次 provider 往返）。
type Request struct {
	State     string     `json:"state"`
	Questions []Question `json:"questions"`
}

// Response 决策结果。Answers 与 Request.Questions 按 Key 对齐（可能少于问题数：
// 无效/缺失答案被丢弃——宁缺勿滥，调用方对缺失答案走现状行为）。
type Response struct {
	Answers []Answer `json:"answers"`
}

// AnswerOf 按 Key 取答案。
func (r Response) AnswerOf(key string) (Answer, bool) {
	for _, a := range r.Answers {
		if a.Key == key {
			return a, true
		}
	}
	return Answer{}, false
}

// Provider 决策提供者。实现：LLM 兜底（llm.go）与外部 HTTP（http.go，TypeSafe 兼容）。
type Provider interface {
	Decide(ctx context.Context, req Request) (Response, error)
}

// ShadowEvent 一条影子对拍行（agent_events type=decision_shadow，零 DDL——type 自由文本）。
// 字段映射：Role=Point、Content=AnswerJSON、Input=Actual、Output=Compare。
type ShadowEvent struct {
	AgentID    string // 影子行归属实例（session_id 由 agentID 派生）
	Point      string // 决策点名
	AnswerJSON string // 答案 JSON（单 Answer 序列化）
	Actual     string // 实际路径取值（代码规则/父 LLM/用户实际选择；未知留空）
	Compare    string // 对照结果：match / mismatch / pending
}

// ShadowRecorder 影子行落库接口。bootstrap 用 agent_events 实现（MemoryPipeline.Write）。
type ShadowRecorder interface {
	RecordShadow(ctx context.Context, ev ShadowEvent) error
}

// PointPolicy 单决策点的行为参数（按后果分级）。
type PointPolicy struct {
	// Mode "shadow"（默认，只记录不生效）| "enforce"（置信达标时答案生效）。
	Mode string
	// MinConfidence 强制生效的置信门槛（低于门槛即使 enforce 也回退现状行为）。
	MinConfidence float64
	// ScoreFloor Score 原语的筛选下限（uptake_score 过滤低相关候选用；<=0 取默认 0.5）。
	ScoreFloor float64
}

// ModeShadow / ModeEnforce PointPolicy.Mode 取值。
const (
	ModeShadow  = "shadow"
	ModeEnforce = "enforce"
)

// Options Layer 行为参数（config.yaml agent.decision_* 装配）。
type Options struct {
	// ShadowEnabled 全局影子开关（默认 true）。false 时仅 enforce 点运行（省成本）。
	ShadowEnabled bool
	// Timeout 单次 provider 往返超时（decision_timeout_sec，默认 5s）。
	Timeout time.Duration
	// Points per-决策点策略；缺失的点按 DefaultPolicy 兜底。
	Points map[string]PointPolicy
}

// DefaultPolicy 未配置决策点的兜底策略（影子 + 0.6 门槛）。
func DefaultPolicy() PointPolicy {
	return PointPolicy{Mode: ModeShadow, MinConfidence: 0.6, ScoreFloor: 0.5}
}

// policyFor 取决策点策略（缺失补默认；MinConfidence/ScoreFloor 零值补默认）。
func (o Options) policyFor(point string) PointPolicy {
	p, ok := o.Points[point]
	if !ok {
		return DefaultPolicy()
	}
	if p.Mode == "" {
		p.Mode = ModeShadow
	}
	if p.MinConfidence <= 0 {
		p.MinConfidence = 0.6
	}
	if p.ScoreFloor <= 0 {
		p.ScoreFloor = 0.5
	}
	return p
}

// Layer 决策层门面：超时包裹 provider + 影子记录 + per-点强制开关。
type Layer struct {
	provider Provider
	recorder ShadowRecorder // nil = 不落影子（测试场景）
	opts     Options
}

// NewLayer 构造决策层。recorder 可 nil（只决策不记录）；provider 不可 nil。
func NewLayer(provider Provider, recorder ShadowRecorder, opts Options) *Layer {
	return &Layer{provider: provider, recorder: recorder, opts: opts}
}

// Decide 调 provider 决策。语义：
//   - 过滤掉本层不该跑的问题（影子关闭且该点非 enforce 的点整条跳过）；
//   - 剩余问题为零时直接返回空 Response（零成本）；
//   - provider 错误/超时原样返回——调用方 fail-open 回退现状行为，本层不吞错不改行为。
func (l *Layer) Decide(ctx context.Context, req Request) (Response, error) {
	if l == nil || l.provider == nil {
		return Response{}, nil
	}
	active := req.filterActive(l.opts)
	if len(active.Questions) == 0 {
		return Response{}, nil
	}
	if l.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.opts.Timeout)
		defer cancel()
	}
	return l.provider.Decide(ctx, active)
}

// ShouldEnforce 报告该点答案是否应生效：点为 enforce 模式且置信达标。
// 影子期恒 false——调用方现状行为零变化。
func (l *Layer) ShouldEnforce(point string, confidence float64) bool {
	if l == nil {
		return false
	}
	p := l.opts.policyFor(point)
	return p.Mode == ModeEnforce && confidence >= p.MinConfidence
}

// EnforceMode 报告该点是否配置为 enforce（与置信无关；调用方决定同步/异步路径用）。
func (l *Layer) EnforceMode(point string) bool {
	if l == nil {
		return false
	}
	return l.opts.policyFor(point).Mode == ModeEnforce
}

// ScoreFloor 取 Score 原语筛选下限。
func (l *Layer) ScoreFloor(point string) float64 {
	if l == nil {
		return 0.5
	}
	return l.opts.policyFor(point).ScoreFloor
}

// Observe 落一条影子对拍行。actual 为实际路径取值（未知留空）；对照结果由
// AnswerCompare 计算。recorder 为 nil / 影子关闭且该点非 enforce 时零操作。
// 影子行是观测数据，写失败仅计数不惊扰主流程（与 mailbox 留痕同款 best-effort）。
func (l *Layer) Observe(ctx context.Context, agentID, actual string, ans Answer) {
	if l == nil || l.recorder == nil || ans.Point == "" {
		return
	}
	p := l.opts.policyFor(ans.Point)
	if p.Mode != ModeEnforce && !l.opts.ShadowEnabled {
		return
	}
	blob, err := json.Marshal(ans)
	if err != nil {
		return
	}
	ev := ShadowEvent{
		AgentID:    agentID,
		Point:      ans.Point,
		AnswerJSON: string(blob),
		Actual:     actual,
		Compare:    AnswerCompare(ans, actual),
	}
	// best-effort：错误仅丢行（观测数据不阻塞主流程）。
	_ = l.recorder.RecordShadow(ctx, ev)
}

// Ask 单问题同步便捷入口（enforce 路径用）：fail-open，任何失败返回 nil，
// 调用方按无建议走现状行为。Observe 由调用方按实际值自行补落（异步对拍点）。
func (l *Layer) Ask(ctx context.Context, state string, q Question) *Answer {
	if l == nil {
		return nil
	}
	resp, err := l.Decide(ctx, Request{State: state, Questions: []Question{q}})
	if err != nil {
		return nil
	}
	if a, ok := resp.AnswerOf(q.Key); ok {
		return &a
	}
	return nil
}

// GoObserve 影子异步观测：后台 Decide + Observe，绝不阻塞调用方（影子期零延迟税）。
// actualFor 在答案到达后逐条求实际值（调用方闭包捕获现场快照）。ctx 自动脱取消
// （WithoutCancel）防调用方返回后 goroutine 被连带取消丢观测。
func (l *Layer) GoObserve(ctx context.Context, agentID string, req Request, actualFor func(Answer) string) {
	if l == nil || l.recorder == nil {
		return
	}
	if len(req.filterActive(l.opts).Questions) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		resp, err := l.Decide(ctx, req)
		if err != nil {
			return
		}
		for _, ans := range resp.Answers {
			actual := ""
			if actualFor != nil {
				actual = actualFor(ans)
			}
			l.Observe(ctx, agentID, actual, ans)
		}
	}()
}

// AnswerCompare 计算单答案与实际路径的对照结果：
//   - actual 为空（实际未知/异步）→ pending（离线 SQL 关联后续事件补对拍）；
//   - Noul/Choice：actual 取首个空白分段（容忍 "yes rank=1" 附注形态）与 Value 比对；
//   - Score：actual 为 "yes"/"no" 时按 (Score>=0.5) 布尔化比对；"score=0.83" 按 0.15
//     带宽比对；其余形态 pending。
func AnswerCompare(ans Answer, actual string) string {
	tok := actual
	if fields := strings.Fields(strings.TrimSpace(actual)); len(fields) > 0 {
		tok = fields[0]
	} else {
		return "pending"
	}
	if ans.Primitive == PrimitiveScore {
		if tok == "yes" || tok == "no" {
			return boolCompare(ans.Score >= 0.5, tok == "yes")
		}
		var got float64
		if _, err := fmtSscanfScore(actual, &got); err != nil {
			return "pending"
		}
		return boolCompare(absDiff(got, ans.Score) <= 0.15, true)
	}
	return boolCompare(ans.Value == tok, true)
}

// boolCompare got/want 一致返回 match，否则 mismatch。
func boolCompare(got, want bool) string {
	if got == want {
		return "match"
	}
	return "mismatch"
}

// filterActive 过滤出本层当前应跑的问题：enforce 点恒跑；影子点仅全局影子开启时跑。
// 过滤同时丢弃 Point/Key 为空的坏问题（防御，调用方拼错不至于发进 provider）。
func (r Request) filterActive(opts Options) Request {
	out := Request{State: r.State}
	for _, q := range r.Questions {
		if q.Key == "" || q.Point == "" {
			continue
		}
		p := opts.policyFor(q.Point)
		if p.Mode != ModeEnforce && !opts.ShadowEnabled {
			continue
		}
		out.Questions = append(out.Questions, q)
	}
	return out
}

// fmtSscanfScore 解析 "score=0.83" 形态的 actual 值（保守解析，失败报错）。
func fmtSscanfScore(s string, out *float64) (int, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "score="))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	*out = v
	return 1, nil
}

// absDiff 浮点绝对差。
func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
