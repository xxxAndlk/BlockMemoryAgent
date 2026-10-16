package workflow

// types.go 工作流量产流水线的核心类型：run/unit 状态机常量、run/unit 行结构、名册项。
// 表与字段以《2026-10-16 工作流量产流水线开发文档》§4 为准；owner 列硬约定随行带上。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// run 状态机：pending → running ⇄ paused → done | failed | aborted（§4.2）。
const (
	RunStatusPending = "pending"
	RunStatusRunning = "running"
	RunStatusPaused  = "paused"
	RunStatusDone    = "done"
	RunStatusFailed  = "failed"
	RunStatusAborted = "aborted"
)

// pause_reason 取值（§4.1 列注释）。
const (
	PauseBudgetExceeded = "budget_exceeded"
	PauseUser           = "user"
	PauseCircuitBreaker = "circuit_breaker"
)

// unit 状态机：pending → running → done | needs_review | failed；skipped（名册裁剪/人审跳过）。
const (
	UnitStatusPending     = "pending"
	UnitStatusRunning     = "running"
	UnitStatusDone        = "done"
	UnitStatusNeedsReview = "needs_review"
	UnitStatusFailed      = "failed"
	UnitStatusSkipped     = "skipped"
)

// 人审操作（HTTP /api/workflow/units/:uid/*）。
const (
	UnitActionAccept = "accept"
	UnitActionReject = "reject"
	UnitActionReroll = "reroll"
	UnitActionSkip   = "skip"
)

// IsRunTerminal 判断 run 是否处于终态（终态不再调度、webhook 触发一次）。
func IsRunTerminal(status string) bool {
	return status == RunStatusDone || status == RunStatusFailed || status == RunStatusAborted
}

// IsUnitTerminal 判断 unit 是否处于终态（调度器跳过；done/skipped 不回炉，
// needs_review/failed 经人审 reroll 才产生新 attempt）。
func IsUnitTerminal(status string) bool {
	return status == UnitStatusDone || status == UnitStatusNeedsReview ||
		status == UnitStatusFailed || status == UnitStatusSkipped
}

// Run 是一次量产（名册 × 配方），独立于 Agent 生命周期（权威全在表 + 事件账本）。
type Run struct {
	ID            string       `json:"id"`
	Owner         string       `json:"owner"`
	SessionID     string       `json:"session_id"` // 发起会话（工作区/事件展示锚点）
	Workdir       string       `json:"workdir"`    // 配方与产物所在工作区
	RecipeID      string       `json:"recipe_id"`
	RecipeVersion int          `json:"recipe_version"`
	RecipeYAML    string       `json:"recipe_yaml,omitempty"` // 启动快照（"这批是哪个标准下做的"）
	Roster        []RosterItem `json:"roster"`
	Status        string       `json:"status"`
	PauseReason   string       `json:"pause_reason,omitempty"`
	BudgetCap     int          `json:"budget_cap"`     // 轻量调用次数上限（估算口径见 Estimate）
	SpentEstimate int          `json:"spent_estimate"` // 已消耗估算
	Concurrency   int          `json:"concurrency"`    // 件间并发
	FailFast      bool         `json:"fail_fast"`      // 早期熔断开关
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	FinishedAt    *time.Time   `json:"finished_at,omitempty"`
}

// Unit 是 run 的最小执行格（件 × 阶段）的一次尝试（attempt）。
// 重摇生成新 attempt 行；被否 attempt 只标不删（拒绝理由是配方演化的输入）。
type Unit struct {
	ID           string    `json:"id"`
	RunID        string    `json:"run_id"`
	Owner        string    `json:"owner"`
	ItemID       string    `json:"item_id"`
	ItemName     string    `json:"item_name,omitempty"`
	Stage        string    `json:"stage"`
	AttemptNo    int       `json:"attempt_no"`
	Status       string    `json:"status"`
	ArtifactPath string    `json:"artifact_path,omitempty"` // 工作区相对路径（正斜杠）
	CheckResult  string    `json:"check_result,omitempty"`  // JSON 文本：{program,judge,drift,upgrade}
	RejectReason string    `json:"reject_reason,omitempty"`
	IdemKey      string    `json:"idem_key"` // run_id+item_id+stage+attempt_no（唯一）
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RosterItem 是名册的一行（一件）。除 id/name/stages 外的字段全部进 Fields（任意槽位值，
// 如 element/family/body_plan/palette），JSON/YAML 编解码时平铺展开——对用户是普通对象。
type RosterItem struct {
	ID     string            `json:"-"`
	Name   string            `json:"-"`
	Stages []string          `json:"-"` // 逐件裁剪的阶段子集；空=全阶段
	Fields map[string]string `json:"-"` // 其余槽位值（含 element/family 等）
}

// Slot 按槽位名取值（id/name 为内建槽，其余查 Fields）。
func (r RosterItem) Slot(name string) string {
	switch name {
	case "id":
		return r.ID
	case "name":
		return r.Name
	}
	return r.Fields[name]
}

// StagesFor 按件内 stages 裁剪返回要执行的阶段序列（保持配方阶段顺序）。
func (r RosterItem) StagesFor(stages []*Stage) []*Stage {
	if len(r.Stages) == 0 {
		return stages
	}
	want := make(map[string]bool, len(r.Stages))
	for _, s := range r.Stages {
		want[strings.TrimSpace(s)] = true
	}
	out := make([]*Stage, 0, len(stages))
	for _, s := range stages {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// MarshalJSON 平铺展开（id/name/stages + Fields 全量）。
func (r RosterItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.toMap())
}

// UnmarshalJSON 收录任意键：id/name/stages 进结构字段，其余进 Fields。
func (r *RosterItem) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("roster item: %w", err)
	}
	return r.fromMap(raw)
}

// MarshalYAML 平铺展开（与 JSON 同形）。
func (r RosterItem) MarshalYAML() (any, error) {
	return r.toMap(), nil
}

// UnmarshalYAML 收录任意键（YAML 名册文件支持逐行对象写法）。
func (r *RosterItem) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("roster item: %w", err)
	}
	return r.fromMap(raw)
}

func (r RosterItem) toMap() map[string]any {
	m := make(map[string]any, len(r.Fields)+3)
	for k, v := range r.Fields {
		m[k] = v
	}
	m["id"] = r.ID
	if r.Name != "" {
		m["name"] = r.Name
	}
	if len(r.Stages) > 0 {
		m["stages"] = r.Stages
	}
	return m
}

func (r *RosterItem) fromMap(raw map[string]any) error {
	fields := make(map[string]string, len(raw))
	for k, v := range raw {
		switch k {
		case "id":
			r.ID = scalarString(v)
		case "name":
			r.Name = scalarString(v)
		case "stages":
			switch vv := v.(type) {
			case []any:
				for _, e := range vv {
					if s := scalarString(e); s != "" {
						r.Stages = append(r.Stages, s)
					}
				}
			case []string:
				r.Stages = append(r.Stages, vv...)
			case string:
				if strings.TrimSpace(vv) != "" {
					for _, s := range strings.Split(vv, ",") {
						if s = strings.TrimSpace(s); s != "" {
							r.Stages = append(r.Stages, s)
						}
					}
				}
			}
		default:
			if s := scalarString(v); s != "" {
				fields[k] = s
			}
		}
	}
	r.ID = strings.TrimSpace(r.ID)
	if r.ID == "" {
		return fmt.Errorf("roster item missing id")
	}
	if len(fields) > 0 {
		r.Fields = fields
	}
	return nil
}

// scalarString 把 YAML/JSON 解出的标量转字符串（int/float/bool 走 fmt ，字符串原样）。
func scalarString(v any) string {
	switch vv := v.(type) {
	case nil:
		return ""
	case string:
		return vv
	case bool:
		if vv {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", vv)
	case int64:
		return fmt.Sprintf("%d", vv)
	case float64:
		if vv == float64(int64(vv)) {
			return fmt.Sprintf("%d", int64(vv))
		}
		return fmt.Sprintf("%g", vv)
	default:
		return fmt.Sprint(vv)
	}
}
