package team

// logical_task.go 逻辑任务合同（Harness 根治计划 WP1）。
//
// 背景（doc/计划_Harness合同事务与语义编辑根治.md）：长会话里 Goal 只是最初目标，
// 决策账本只是"用户说过什么"的流水；两者都无法回答"当前逻辑任务是什么、本轮允许
// 操作哪些资源、哪些历史约束仍然活跃"。C-06 TaskContract 服务自认领计划（InitPlan
// 全量替换投影），不能直接复用为顶层任务事实源，因此新增独立类型，保持 C-06 JSON
// 与 Claim/fencing 语义不变。
//
// 本文件只做纯类型、规范化、hash 与状态迁移判定，不 import 任何 internal 上层包：
//   - 存储接口在 logical_task_store.go（接口 + 内存实现）；
//   - PG 实现在 internal/store/logical_task_store.go；
//   - 顶层会话绑定在 internal/agent（TaskManager 经 bootstrap 注入）。
//
// 关键纪律（计划 §2.2）：
//   - 当前合同与活跃不变量永不参与有损摘要（I-STATE-1）；
//   - 事件是权威底账，投影可重建（I-STATE-2）；
//   - 空 WriteSet 不表示"全部允许"，而表示"尚未解析，不能执行生产写入"（§3.2）；
//   - Invariant 必须有至少一种机器可验证 Kind，否则任务只能 unverified（§3.2）；
//   - verified（机器证据）与 accepted（用户决定）永远分离（§3.4）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LogicalTaskSchemaVersion 当前合同 schema 版本。
const LogicalTaskSchemaVersion = 1

// 逻辑任务状态（§3.4 状态机）。
const (
	TaskStatusDraft                 = "draft"
	TaskStatusActive                = "active"
	TaskStatusBlocked               = "blocked"
	TaskStatusAwaitingClarification = "awaiting_clarification"
	TaskStatusAwaitingVerification  = "awaiting_verification"
	TaskStatusVerified              = "verified"
	TaskStatusDeliveredUnverified   = "delivered-unverified"
	TaskStatusFailed                = "failed"
	TaskStatusCancelled             = "cancelled"
	TaskStatusAccepted              = "accepted"
	TaskStatusSuperseded            = "superseded"
)

// 逻辑任务事件类型（append-only）。
const (
	LogicalEventCreated    = "created"
	LogicalEventRefined    = "refined"
	LogicalEventSuperseded = "superseded"
	LogicalEventRevoked    = "revoked"
	LogicalEventVerified   = "verified"
	LogicalEventUnverified = "delivered_unverified"
	LogicalEventAccepted   = "accepted"
	LogicalEventRejected   = "rejected"
	LogicalEventFailed     = "failed"
	LogicalEventCancelled  = "cancelled"
)

// 资源规则效果（deny 优先）。
const (
	RuleEffectAllow = "allow"
	RuleEffectAsk   = "ask"
	RuleEffectDeny  = "deny"
)

// 资源规则种类。
const (
	RuleKindPath           = "path"
	RuleKindGlob           = "glob"
	RuleKindSemanticTarget = "semantic_target"
	RuleKindTool           = "tool"
	RuleKindNetwork        = "network"
	RuleKindDatabase       = "database"
)

// 资源规则来源（审计用：谁给的权限）。
const (
	RuleSourceUser     = "user"
	RuleSourceResolver = "resolver"
	RuleSourcePolicy   = "policy"
	RuleSourceLegacy   = "legacy"
)

// 不变量状态。
const (
	InvariantActive     = "active"
	InvariantSuperseded = "superseded"
	InvariantRevoked    = "revoked"
)

// 机器可验证的不变量种类（§3.2：只存自然语言不算数）。
const (
	InvariantKindFileHash  = "file_hash"
	InvariantKindTextRange = "text_range"
	InvariantKindASTNode   = "ast_node"
	InvariantKindDOMMetric = "dom_metric"
	InvariantKindAPISchema = "api_schema"
	InvariantKindTest      = "test"
)

// verifiableInvariantKinds 是允许进入 active 的不变量种类集合。
var verifiableInvariantKinds = map[string]bool{
	InvariantKindFileHash:  true,
	InvariantKindTextRange: true,
	InvariantKindASTNode:   true,
	InvariantKindDOMMetric: true,
	InvariantKindAPISchema: true,
	InvariantKindTest:      true,
}

// SemanticTarget 语义目标：比"文件名"更细的编辑单位（符号/模板节点/CSS 声明/API 字段）。
type SemanticTarget struct {
	// ID 稳定标识（会话内唯一，跨 revision 保留以支持不变量引用）。
	ID string `json:"id"`
	// File 目标文件的工作区相对路径（正斜杠）。
	File string `json:"file,omitempty"`
	// Language 语言标记：vue/css/ts/js/go/api/...（适配器据此选解析器）。
	Language string `json:"language,omitempty"`
	// Kind 目标种类：symbol/template_node/css_declaration/api/...
	Kind string `json:"kind,omitempty"`
	// Anchor 结构锚点（适配器解析用；不是任意正则）。
	Anchor string `json:"anchor,omitempty"`
	// Label 用户可见标签/名称（如"订单号""商品名称"）：把用户原话里的中文指称映射到
	// 具体语义目标；解析器产出，用户输入匹配时使用。空=无标签。
	Label string `json:"label,omitempty"`
	// Fingerprint 目标当前内容指纹（适配器产出，用于"唯一目标 + 预期指纹"校验）。
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ResourceRule 一条资源规则。ExcludedSet 恒为 deny。
type ResourceRule struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
	Source   string `json:"source"`
	Revision int64  `json:"revision"`
}

// Invariant 用户/合同不变量（机器可验证）。
type Invariant struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Target       SemanticTarget  `json:"target"`
	Expected     json.RawMessage `json:"expected"`
	BaselineRef  string          `json:"baseline_ref,omitempty"`
	SourceEvent  int64           `json:"source_event_id,omitempty"`
	SourceTaskID string          `json:"source_task_id,omitempty"`
	Status       string          `json:"status"`
	Verification string          `json:"verification"`
	// Text 人类可读描述（注入上下文用；不参与机器校验）。
	Text string `json:"text,omitempty"`
	// NeedsConfirm 无法判断是否仍相关时置 true（§3.3 第 5 条：不得静默丢弃）。
	NeedsConfirm bool `json:"needs_confirm,omitempty"`
}

// AcceptanceRule 合同验收规则（分层：static/unit/integration/dom/visual/api/manual）。
type AcceptanceRule struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	Required     bool            `json:"required"`
	TargetIDs    []string        `json:"target_ids,omitempty"`
	InvariantIDs []string        `json:"invariant_ids,omitempty"`
	Spec         json.RawMessage `json:"spec"`
}

// LogicalTaskContract 一次逻辑任务的权威合同（某 revision 的不可变快照）。
type LogicalTaskContract struct {
	SchemaVersion  int              `json:"schema_version"`
	LogicalTaskID  string           `json:"logical_task_id"`
	SessionID      string           `json:"session_id"`
	Owner          string           `json:"owner"`
	Revision       int64            `json:"revision"`
	ParentRevision int64            `json:"parent_revision,omitempty"`
	RelatedTaskIDs []string         `json:"related_task_ids,omitempty"`
	Goal           string           `json:"goal"`
	OriginEventID  int64            `json:"origin_event_id,omitempty"`
	Targets        []SemanticTarget `json:"targets,omitempty"`
	ReadSet        []ResourceRule   `json:"read_set,omitempty"`
	WriteSet       []ResourceRule   `json:"write_set,omitempty"`
	ExcludedSet    []ResourceRule   `json:"excluded_set,omitempty"`
	Invariants     []Invariant      `json:"invariants,omitempty"`
	Acceptance     []AcceptanceRule `json:"acceptance,omitempty"`
	BaseCheckpoint string           `json:"base_checkpoint,omitempty"`
	Status         TaskStatus       `json:"status"`
	ContentHash    string           `json:"content_hash"`
	CreatedAt      time.Time        `json:"created_at,omitempty"`
}

// TaskStatus 逻辑任务状态。
type TaskStatus = string

// LogicalTaskEvent append-only 事件（投影的唯一权威来源）。
type LogicalTaskEvent struct {
	EventID        int64     `json:"event_id,omitempty"`
	SessionID      string    `json:"session_id"`
	LogicalTaskID  string    `json:"logical_task_id"`
	Revision       int64     `json:"revision"`
	Kind           string    `json:"kind"`
	Payload        string    `json:"payload,omitempty"`
	OriginEventID  int64     `json:"origin_event_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	Owner          string    `json:"owner"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// NormalizeContract 规范化合同：裁剪空白、补默认值、规则去重排序、状态默认。
// 规范化是幂等的；同一逻辑输入得到同一 ContentHash。
func NormalizeContract(c *LogicalTaskContract) error {
	if c == nil {
		return fmt.Errorf("logical task contract required")
	}
	c.SchemaVersion = LogicalTaskSchemaVersion
	c.LogicalTaskID = strings.TrimSpace(c.LogicalTaskID)
	c.SessionID = strings.TrimSpace(c.SessionID)
	c.Goal = strings.TrimSpace(c.Goal)
	c.Status = strings.TrimSpace(c.Status)
	if c.LogicalTaskID == "" {
		return fmt.Errorf("logical task: logical_task_id required")
	}
	if c.SessionID == "" {
		return fmt.Errorf("logical task: session_id required")
	}
	if c.Revision <= 0 {
		c.Revision = 1
	}
	if c.Status == "" {
		c.Status = TaskStatusDraft
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.ID = strings.TrimSpace(t.ID)
		t.File = normalizeRelPath(t.File)
		t.Kind = strings.TrimSpace(t.Kind)
		t.Language = strings.TrimSpace(t.Language)
		t.Anchor = strings.TrimSpace(t.Anchor)
		t.Label = strings.TrimSpace(t.Label)
	}
	c.ReadSet = normalizeRules(c.ReadSet, RuleEffectAllow, c.Revision)
	c.WriteSet = normalizeRules(c.WriteSet, RuleEffectAllow, c.Revision)
	c.ExcludedSet = normalizeRules(c.ExcludedSet, RuleEffectDeny, c.Revision)
	// 拒绝集恒为 deny（§3.2）。
	for i := range c.ExcludedSet {
		c.ExcludedSet[i].Effect = RuleEffectDeny
	}
	for i := range c.Invariants {
		inv := &c.Invariants[i]
		inv.ID = strings.TrimSpace(inv.ID)
		inv.Kind = strings.TrimSpace(inv.Kind)
		inv.Target.File = normalizeRelPath(inv.Target.File)
		if inv.Status == "" {
			inv.Status = InvariantActive
		}
		if inv.Verification == "" {
			inv.Verification = inv.Kind
		}
		if len(inv.Expected) == 0 {
			inv.Expected = json.RawMessage(`{}`)
		}
	}
	for i := range c.Acceptance {
		a := &c.Acceptance[i]
		a.ID = strings.TrimSpace(a.ID)
		a.Kind = strings.TrimSpace(a.Kind)
		if a.Kind == "" {
			a.Kind = "static"
		}
		if len(a.Spec) == 0 {
			a.Spec = json.RawMessage(`{}`)
		}
	}
	sort.Strings(c.RelatedTaskIDs)
	c.RelatedTaskIDs = dedupeStrings(c.RelatedTaskIDs)
	c.ContentHash = ComputeContractHash(c)
	return nil
}

// ValidateContract 校验合同可执行性（在 Normalize 之后调用）。
func ValidateContract(c *LogicalTaskContract) error {
	if c == nil {
		return fmt.Errorf("logical task contract required")
	}
	if c.ContentHash == "" {
		return fmt.Errorf("logical task: content_hash missing (call NormalizeContract first)")
	}
	if !validTaskStatus(c.Status) {
		return fmt.Errorf("logical task: illegal status %q", c.Status)
	}
	seenTarget := map[string]bool{}
	for i, t := range c.Targets {
		if t.ID == "" {
			return fmt.Errorf("logical task: target[%d] id required", i)
		}
		if seenTarget[t.ID] {
			return fmt.Errorf("logical task: duplicate target id %q", t.ID)
		}
		seenTarget[t.ID] = true
	}
	for i, inv := range c.Invariants {
		if !verifiableInvariantKinds[inv.Kind] {
			return fmt.Errorf("logical task: invariant[%d] kind %q is not machine-verifiable (allowed: %s)",
				i, inv.Kind, strings.Join(verifiableKinds(), "/"))
		}
		if cleanStatus := strings.TrimSpace(inv.Status); cleanStatus != InvariantActive && cleanStatus != InvariantSuperseded && cleanStatus != InvariantRevoked {
			return fmt.Errorf("logical task: invariant[%d] illegal status %q", i, inv.Status)
		}
	}
	for i, r := range append(append(append([]ResourceRule{}, c.ReadSet...), c.WriteSet...), c.ExcludedSet...) {
		if r.Kind == "" || r.Value == "" {
			return fmt.Errorf("logical task: resource rule[%d] kind/value required", i)
		}
		switch r.Effect {
		case RuleEffectAllow, RuleEffectAsk, RuleEffectDeny:
		default:
			return fmt.Errorf("logical task: resource rule[%d] illegal effect %q", i, r.Effect)
		}
	}
	for i, a := range c.Acceptance {
		switch a.Kind {
		case "static", "unit", "integration", "dom", "visual", "api", "manual":
		default:
			return fmt.Errorf("logical task: acceptance[%d] illegal kind %q", i, a.Kind)
		}
	}
	return nil
}

func verifiableKinds() []string {
	out := make([]string, 0, len(verifiableInvariantKinds))
	for k := range verifiableInvariantKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validTaskStatus(s string) bool {
	switch s {
	case TaskStatusDraft, TaskStatusActive, TaskStatusBlocked, TaskStatusAwaitingClarification,
		TaskStatusAwaitingVerification, TaskStatusVerified, TaskStatusDeliveredUnverified,
		TaskStatusFailed, TaskStatusCancelled, TaskStatusAccepted, TaskStatusSuperseded:
		return true
	}
	return false
}

// IsTerminalTaskStatus 终态判定（终态后才能被新任务替代/接受）。
func IsTerminalTaskStatus(s string) bool {
	switch s {
	case TaskStatusVerified, TaskStatusDeliveredUnverified, TaskStatusFailed, TaskStatusCancelled, TaskStatusAccepted, TaskStatusSuperseded:
		return true
	}
	return false
}

// ActiveInvariants 返回仍生效的不变量（active 且未需确认的按需过滤）。
func (c *LogicalTaskContract) ActiveInvariants() []Invariant {
	if c == nil {
		return nil
	}
	out := make([]Invariant, 0, len(c.Invariants))
	for _, inv := range c.Invariants {
		if inv.Status == InvariantActive {
			out = append(out, inv)
		}
	}
	return out
}

// FindInvariant 按 ID 查找不变量。
func (c *LogicalTaskContract) FindInvariant(id string) (Invariant, bool) {
	if c == nil {
		return Invariant{}, false
	}
	for _, inv := range c.Invariants {
		if inv.ID == id {
			return inv, true
		}
	}
	return Invariant{}, false
}

// contractCanonical 是 hash 用的规范化视图：只含权威字段，不含展示字段与状态。
// Status 属状态机推进（active→verified），不参与合同版本 hash——否则"验证通过"
// 会让同一合同版本的验收证据被误判为过期。
type contractCanonical struct {
	SchemaVersion  int              `json:"schema_version"`
	LogicalTaskID  string           `json:"logical_task_id"`
	SessionID      string           `json:"session_id"`
	Revision       int64            `json:"revision"`
	ParentRevision int64            `json:"parent_revision,omitempty"`
	RelatedTaskIDs []string         `json:"related_task_ids,omitempty"`
	Goal           string           `json:"goal"`
	Targets        []SemanticTarget `json:"targets,omitempty"`
	ReadSet        []ResourceRule   `json:"read_set,omitempty"`
	WriteSet       []ResourceRule   `json:"write_set,omitempty"`
	ExcludedSet    []ResourceRule   `json:"excluded_set,omitempty"`
	Invariants     []Invariant      `json:"invariants,omitempty"`
	Acceptance     []AcceptanceRule `json:"acceptance,omitempty"`
	BaseCheckpoint string           `json:"base_checkpoint,omitempty"`
}

// ComputeContractHash 计算合同内容 hash（规范化后字段的 sha256）。
func ComputeContractHash(c *LogicalTaskContract) string {
	if c == nil {
		return ""
	}
	payload := contractCanonical{
		SchemaVersion:  c.SchemaVersion,
		LogicalTaskID:  c.LogicalTaskID,
		SessionID:      c.SessionID,
		Revision:       c.Revision,
		ParentRevision: c.ParentRevision,
		RelatedTaskIDs: append([]string{}, c.RelatedTaskIDs...),
		Goal:           strings.TrimSpace(c.Goal),
		Targets:        stableTargets(c.Targets),
		ReadSet:        stableRules(c.ReadSet),
		WriteSet:       stableRules(c.WriteSet),
		ExcludedSet:    stableRules(c.ExcludedSet),
		Invariants:     stableInvariants(c.Invariants),
		Acceptance:     stableAcceptance(c.Acceptance),
		BaseCheckpoint: c.BaseCheckpoint,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// stableTargets 返回按 ID 排序的目标副本（hash 与输入顺序无关）。
func stableTargets(in []SemanticTarget) []SemanticTarget {
	out := append([]SemanticTarget{}, in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// stableRules 返回排序后的规则副本（hash 与输入顺序无关）。
func stableRules(in []ResourceRule) []ResourceRule {
	out := append([]ResourceRule{}, in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Value != out[j].Value {
			return out[i].Value < out[j].Value
		}
		return out[i].Effect < out[j].Effect
	})
	return out
}

func stableInvariants(in []Invariant) []Invariant {
	out := append([]Invariant{}, in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func stableAcceptance(in []AcceptanceRule) []AcceptanceRule {
	out := append([]AcceptanceRule{}, in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// normalizeRelPath 统一相对路径为正斜杠、去 ./ 前缀与尾部斜杠。
func normalizeRelPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p == "." {
		return ""
	}
	return strings.TrimSuffix(p, "/")
}

// normalizeRules 规范化资源规则（补齐来源/revision、去重、稳定排序）。
func normalizeRules(in []ResourceRule, defaultEffect string, rev int64) []ResourceRule {
	out := make([]ResourceRule, 0, len(in))
	seen := map[string]bool{}
	for _, r := range in {
		r.Kind = strings.TrimSpace(strings.ToLower(r.Kind))
		r.Value = strings.TrimSpace(r.Value)
		if r.Kind == "" || r.Value == "" {
			continue
		}
		if r.Kind == RuleKindPath || r.Kind == RuleKindGlob {
			r.Value = normalizeRelPath(r.Value)
			if r.Value == "" {
				continue
			}
		}
		if r.Effect == "" {
			r.Effect = defaultEffect
		}
		r.Effect = strings.ToLower(strings.TrimSpace(r.Effect))
		if r.Source == "" {
			r.Source = RuleSourceResolver
		}
		if r.Revision <= 0 {
			r.Revision = rev
		}
		key := r.Kind + "\x00" + r.Value + "\x00" + r.Effect
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return stableRules(out)
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	var last string
	for i, s := range in {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

// --- 状态迁移 -------------------------------------------------------------

// SupersedeInvariant 把不变量标记为 superseded（不物理删除）。
func SupersedeInvariant(c *LogicalTaskContract, id string) bool {
	return setInvariantStatus(c, id, InvariantSuperseded)
}

// RevokeInvariant 用户明确撤销：标记 revoked（历史仍可查）。
func RevokeInvariant(c *LogicalTaskContract, id string) bool {
	return setInvariantStatus(c, id, InvariantRevoked)
}

func setInvariantStatus(c *LogicalTaskContract, id, status string) bool {
	if c == nil {
		return false
	}
	for i := range c.Invariants {
		if c.Invariants[i].ID == id {
			if c.Invariants[i].Status == status {
				return true
			}
			c.Invariants[i].Status = status
			return true
		}
	}
	return false
}

// AdvanceStatus 状态推进（不合法迁移返回错误，绝不静默丢弃历史）。
func AdvanceStatus(c *LogicalTaskContract, next string) error {
	if c == nil {
		return fmt.Errorf("logical task contract required")
	}
	cur := c.Status
	if cur == "" {
		cur = TaskStatusDraft
	}
	if next == cur {
		return nil
	}
	if !validTaskStatus(next) {
		return fmt.Errorf("logical task: illegal status %q", next)
	}
	if cur == TaskStatusAccepted || cur == TaskStatusSuperseded {
		return fmt.Errorf("logical task: status %q is final, cannot move to %q", cur, next)
	}
	if !statusTransitionAllowed(cur, next) {
		return fmt.Errorf("logical task: illegal transition %s -> %s", cur, next)
	}
	c.Status = next
	return nil
}

// statusTransitionAllowed 定义允许的迁移集合（保守：未知迁移一律拒绝）。
func statusTransitionAllowed(from, to string) bool {
	switch from {
	case TaskStatusDraft:
		return to == TaskStatusActive || to == TaskStatusBlocked || to == TaskStatusCancelled || to == TaskStatusAwaitingClarification
	case TaskStatusActive:
		switch to {
		case TaskStatusBlocked, TaskStatusAwaitingClarification, TaskStatusAwaitingVerification,
			TaskStatusVerified, TaskStatusDeliveredUnverified, TaskStatusFailed, TaskStatusCancelled, TaskStatusSuperseded:
			return true
		}
	case TaskStatusBlocked, TaskStatusAwaitingClarification:
		return to == TaskStatusActive || to == TaskStatusCancelled || to == TaskStatusFailed || to == TaskStatusSuperseded
	case TaskStatusAwaitingVerification:
		switch to {
		case TaskStatusVerified, TaskStatusDeliveredUnverified, TaskStatusFailed, TaskStatusCancelled, TaskStatusSuperseded, TaskStatusActive:
			return true
		}
	case TaskStatusVerified, TaskStatusDeliveredUnverified:
		return to == TaskStatusAccepted || to == TaskStatusSuperseded
	case TaskStatusFailed, TaskStatusCancelled:
		return to == TaskStatusAccepted || to == TaskStatusSuperseded
	}
	return false
}

// --- 不变量继承（§3.3 保守策略，不额外调用 LLM） ---------------------------

// InvariantInheritance 继承结果：继承项 + 需用户确认项（不得静默丢弃）。
type InvariantInheritance struct {
	Inherited    []Invariant
	NeedsConfirm []Invariant
	Revoked      []Invariant
}

// revocationPatterns 是用户显式撤销句式（保守：只认明确否定）。
var revocationPatterns = []string{
	"撤销", "取消之前", "取消此前", "不要之前的", "不要之前", "别用之前的",
	"恢复原来", "恢复原样", "重来", "重新做", "回滚",
}

// looksLikeRevocation 判断用户输入是否包含显式撤销语义。
func looksLikeRevocation(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	for _, p := range revocationPatterns {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

// InheritInvariants 计算新任务从旧任务继承的不变量：
//   - 只继承 active；
//   - 无显式撤销语义；
//   - 目标与当前任务已解析的语义目标/路径重叠（同工作区文件即视为重叠）；
//   - 不重叠且无法判断相关性时进入 NeedsConfirm（保留约束并标记需确认）。
//
// 选择"重叠"而不是"同会话全部继承"：计划 §3.3 明确无关旧任务不得全部注入。
func InheritInvariants(prev *LogicalTaskContract, nextTargets []SemanticTarget, nextRules []ResourceRule, userInput string) InvariantInheritance {
	res := InvariantInheritance{}
	if prev == nil {
		return res
	}
	revokeAll := looksLikeRevocation(userInput)
	nextFiles := map[string]bool{}
	for _, t := range nextTargets {
		if t.File != "" {
			nextFiles[normalizeRelPath(t.File)] = true
		}
	}
	for _, r := range nextRules {
		if r.Kind == RuleKindPath || r.Kind == RuleKindGlob {
			if v := normalizeRelPath(r.Value); v != "" {
				nextFiles[v] = true
			}
		}
	}
	for _, inv := range prev.Invariants {
		if inv.Status != InvariantActive {
			continue
		}
		if revokeAll {
			inv.Status = InvariantRevoked
			res.Revoked = append(res.Revoked, inv)
			continue
		}
		if inv.Target.File != "" && nextFiles[normalizeRelPath(inv.Target.File)] {
			res.Inherited = append(res.Inherited, inv)
			continue
		}
		// 与当前任务已解析目标不重叠、或当前任务尚未解析目标：无法判断是否仍相关
		// → 保留约束并标记需确认，不得静默丢弃（§3.3 第 5 条）。需确认项不进入
		// "已生效约束"注入的正确性断言，渲染时显式标注，由模型/用户裁决。
		inv.NeedsConfirm = true
		res.NeedsConfirm = append(res.NeedsConfirm, inv)
	}
	return res
}

// --- 投影重建 -------------------------------------------------------------

// ApplyEvent 把一条事件应用到投影（纯函数；RebuildProjection 与增量更新共用）。
// 事件 payload 为合同 JSON（created/refined）或迁移载荷（其余）。
func ApplyEvent(proj *LogicalTaskContract, ev LogicalTaskEvent) (*LogicalTaskContract, error) {
	switch ev.Kind {
	case LogicalEventCreated, LogicalEventRefined:
		var c LogicalTaskContract
		if err := json.Unmarshal([]byte(ev.Payload), &c); err != nil {
			return proj, fmt.Errorf("logical task: decode %s payload: %w", ev.Kind, err)
		}
		if c.LogicalTaskID == "" {
			c.LogicalTaskID = ev.LogicalTaskID
		}
		if c.SessionID == "" {
			c.SessionID = ev.SessionID
		}
		if c.Revision <= 0 {
			c.Revision = ev.Revision
		}
		if c.Status == "" {
			c.Status = TaskStatusActive
		}
		return &c, nil
	case LogicalEventSuperseded:
		if proj == nil {
			return nil, fmt.Errorf("logical task: supersede without projection")
		}
		proj.Status = TaskStatusSuperseded
		return proj, nil
	case LogicalEventRevoked:
		if proj == nil {
			return nil, fmt.Errorf("logical task: revoke without projection")
		}
		if ev.Payload != "" {
			var payload struct {
				InvariantIDs []string `json:"invariant_ids"`
			}
			if err := json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
				return proj, fmt.Errorf("logical task: decode revoke payload: %w", err)
			}
			for _, id := range payload.InvariantIDs {
				RevokeInvariant(proj, id)
			}
		}
		return proj, nil
	case LogicalEventVerified:
		if proj == nil {
			return nil, fmt.Errorf("logical task: verify without projection")
		}
		proj.Status = TaskStatusVerified
		return proj, nil
	case LogicalEventUnverified:
		if proj == nil {
			return nil, fmt.Errorf("logical task: unverified without projection")
		}
		proj.Status = TaskStatusDeliveredUnverified
		return proj, nil
	case LogicalEventAccepted:
		if proj == nil {
			return nil, fmt.Errorf("logical task: accept without projection")
		}
		proj.Status = TaskStatusAccepted
		return proj, nil
	case LogicalEventRejected:
		if proj == nil {
			return nil, fmt.Errorf("logical task: reject without projection")
		}
		proj.Status = TaskStatusFailed
		return proj, nil
	case LogicalEventFailed:
		if proj == nil {
			return nil, fmt.Errorf("logical task: fail without projection")
		}
		proj.Status = TaskStatusFailed
		return proj, nil
	case LogicalEventCancelled:
		if proj == nil {
			return nil, fmt.Errorf("logical task: cancel without projection")
		}
		proj.Status = TaskStatusCancelled
		return proj, nil
	default:
		return proj, fmt.Errorf("logical task: unknown event kind %q", ev.Kind)
	}
}

// RebuildProjection 从事件流重建投影（最后一条 created/refined 为基准，后续迁移按序应用）。
// 只用于恢复/对账：投影失败必须显式报错，不能伪造状态。
func RebuildProjection(sessionID, taskID string, events []LogicalTaskEvent) (*LogicalTaskContract, error) {
	var proj *LogicalTaskContract
	for _, ev := range events {
		if taskID != "" && ev.LogicalTaskID != taskID {
			continue
		}
		if sessionID != "" && ev.SessionID != sessionID {
			continue
		}
		next, err := ApplyEvent(proj, ev)
		if err != nil {
			return proj, err
		}
		proj = next
	}
	return proj, nil
}

// RenderContractBlock 渲染注入上下文的【当前逻辑任务】块（不参与压缩，见 I-STATE-1）。
// 只含当前合同与活跃不变量；历史/被撤销项不注入。
func RenderContractBlock(c *LogicalTaskContract, maxRunes int) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【当前逻辑任务】id=%s rev=%d 状态=%s hash=%s\n", c.LogicalTaskID, c.Revision, c.Status, shortHash(c.ContentHash))
	if g := strings.TrimSpace(c.Goal); g != "" {
		fmt.Fprintf(&b, "目标：%s\n", g)
	}
	if len(c.Targets) > 0 {
		b.WriteString("目标资源：\n")
		for _, t := range c.Targets {
			fmt.Fprintf(&b, "- %s", t.ID)
			if t.File != "" {
				fmt.Fprintf(&b, "（%s", t.File)
				if t.Kind != "" {
					fmt.Fprintf(&b, " · %s", t.Kind)
				}
				b.WriteString("）")
			}
			if t.Anchor != "" {
				fmt.Fprintf(&b, " 锚点=%s", t.Anchor)
			}
			b.WriteString("\n")
		}
	}
	if len(c.WriteSet) > 0 {
		fmt.Fprintf(&b, "允许写入：%s\n", renderRules(c.WriteSet))
	} else {
		b.WriteString("允许写入：（未解析——生产写入将被拒绝，需先解析目标资源）\n")
	}
	if len(c.ExcludedSet) > 0 {
		fmt.Fprintf(&b, "禁止触碰：%s\n", renderRules(c.ExcludedSet))
	}
	if invs := c.ActiveInvariants(); len(invs) > 0 {
		b.WriteString("活跃不变量（不得破坏，违反即验收失败）：\n")
		for _, inv := range invs {
			line := "- [" + inv.Kind + "] " + inv.ID
			if inv.Text != "" {
				line += "：" + inv.Text
			}
			if inv.Target.File != "" {
				line += "（" + inv.Target.File
				if inv.Target.Kind != "" {
					line += " · " + inv.Target.Kind
				}
				line += "）"
			}
			if inv.NeedsConfirm {
				line += "【需确认是否仍适用】"
			}
			b.WriteString(line + "\n")
		}
	}
	if len(c.Acceptance) > 0 {
		b.WriteString("验收规则：")
		parts := make([]string, 0, len(c.Acceptance))
		for _, a := range c.Acceptance {
			tag := a.Kind
			if a.Required {
				tag += "*"
			}
			parts = append(parts, tag)
		}
		b.WriteString(strings.Join(parts, " / ") + "\n")
	}
	b.WriteString("纪律：修改必须落在允许写入范围内；扩大范围必须先产生新合同版本；不得通过 RunCommand 绕过。")
	out := b.String()
	if maxRunes > 0 && len([]rune(out)) > maxRunes {
		// 确定性装配：不截断不变量/写集段，只截断目标资源列表尾部的展示细节。
		out = truncateRunesKeepTail(out, maxRunes)
	}
	return out
}

func truncateRunesKeepTail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n（合同块超出预算，已截断展示；权威内容见事件底账）"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func renderRules(rules []ResourceRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, r.Kind+":"+r.Value)
	}
	return strings.Join(parts, "；")
}
