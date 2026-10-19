package flow

// types.go 可视化编排（扣子/Dify 式节点图）的核心类型。
// 定义真相源 = 工作区 `.bma/flows/<id>.json`（随项目持久、git 可版本化，不落库）；
// 运行真相 = flow_runs / flow_node_runs 两表（权威全在表，重启可恢复）。
//
// v1 节点集：start / llm / tool / branch / approval / answer。
// 执行模型：图解释器逐节点走（branch 决定下一跳），节点粒度 = 一次模型调用 / 一次工具
// 调用，不是完整会话；v1 节点间串行，分支互斥（未选中的路径终态时统一标 skipped）。

import (
	"time"
)

// 节点类型（v1 全集）。
const (
	NodeTypeStart    = "start"
	NodeTypeLLM      = "llm"
	NodeTypeTool     = "tool"
	NodeTypeBranch   = "branch"
	NodeTypeApproval = "approval"
	NodeTypeAnswer   = "answer"
)

// nodeTypes 是 v1 支持的节点类型集合（校验用）。
var nodeTypes = map[string]bool{
	NodeTypeStart:    true,
	NodeTypeLLM:      true,
	NodeTypeTool:     true,
	NodeTypeBranch:   true,
	NodeTypeApproval: true,
	NodeTypeAnswer:   true,
}

// run 状态机：pending（全局活跃闸排队）→ running → waiting_approval（approval 节点挂起）
// → running → done | failed | aborted。
const (
	RunStatusPending        = "pending"
	RunStatusRunning        = "running"
	RunStatusWaitingApprove = "waiting_approval"
	RunStatusDone           = "done"
	RunStatusFailed         = "failed"
	RunStatusAborted        = "aborted"
)

// node_run 状态机：pending → running → success | failed | waiting；
// skipped = 分支未选中或 answer 已先行终结，终态时统一标。
const (
	NodeStatusPending = "pending"
	NodeStatusRunning = "running"
	NodeStatusSuccess = "success"
	NodeStatusFailed  = "failed"
	NodeStatusWaiting = "waiting"
	NodeStatusSkipped = "skipped"
)

// IsRunTerminal 判断 run 是否终态。
func IsRunTerminal(status string) bool {
	return status == RunStatusDone || status == RunStatusFailed || status == RunStatusAborted
}

// Flow 是一张节点图的定义（保存进 .bma/flows/<id>.json 的 JSON 本体）。
type Flow struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Version     int         `json:"version"`
	Description string      `json:"description,omitempty"`
	Nodes       []*Node     `json:"nodes"`
	// Inputs/Outputs 是展示冗余（编辑器和列表页用）；权威分别是 start 节点与 answer 节点的配置。
	Inputs  []ParamSpec `json:"inputs,omitempty"`
	Outputs []ParamSpec `json:"outputs,omitempty"`
}

// ParamSpec 是变量声明（start 输入 / llm 输出 / flow 展示冗余）。
type ParamSpec struct {
	Key         string `json:"key"`
	Type        string `json:"type"` // string|number|boolean|array|object（类型校验宽松，仅提示）
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// Node 是图中的一个节点。Config 的 schema 随 Type 不同：
//   - start:    {inputs: []ParamSpec}
//   - llm:      {model: 角色ID(空=轻量模型), system, prompt, temperature, outputs: []ParamSpec}
//   - tool:     {tool: 工具名, args: {k: 字面量或 "{{node.key}}"}}
//   - branch:   {cases: [{label, when: [{left, op, right}], next}], default: 节点ID}
//   - approval: {question, timeout_minutes}
//   - answer:   {outputs: {key: 字面量或 "{{node.key}}" 模板}}
type Node struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	Position  *Position       `json:"position,omitempty"` // 画布坐标（编辑器布局，引擎忽略）
	DependsOn []string        `json:"depends_on,omitempty"`
	Config    map[string]any  `json:"config,omitempty"`
}

// Position 是节点在画布上的坐标。
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// BranchCase 是 branch 节点的一条分支（when 全部成立才命中，cases 按序首个命中生效）。
type BranchCase struct {
	Label string       `json:"label"`
	When  []Condition  `json:"when"`
	Next  string       `json:"next"` // 命中后跳到的节点 ID
}

// Condition 是分支条件：left/right 为字面量或 "{{node.key}}" 引用。
// Op: == != contains not_contains > < >= <= exists not_exists
// exists/not_exists 只看 left 是否解析得到值（right 忽略）。
type Condition struct {
	Left  string `json:"left"`
	Op    string `json:"op"`
	Right string `json:"right,omitempty"`
}

// Run 是一次流程执行（flow 定义启动时快照进 flow_json）。
type Run struct {
	ID          string            `json:"id"`
	FlowID      string            `json:"flow_id"`
	FlowName    string            `json:"flow_name"`
	FlowVersion int               `json:"flow_version"`
	FlowJSON    string            `json:"flow_json,omitempty"` // 启动快照
	Inputs      map[string]any    `json:"inputs,omitempty"`
	Status      string            `json:"status"`
	Outputs     map[string]any    `json:"outputs,omitempty"`
	Error       string            `json:"error,omitempty"`
	SessionID   string            `json:"session_id,omitempty"` // 发起会话（可选锚点）
	Workdir     string            `json:"workdir"`              // flow 定义所在工作区
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	FinishedAt  *time.Time        `json:"finished_at,omitempty"`
}

// NodeRun 是一个节点在一次 run 中的执行记录（trace 数据源）。
type NodeRun struct {
	ID         string         `json:"id"`
	RunID      string         `json:"run_id"`
	NodeID     string         `json:"node_id"`
	NodeType   string         `json:"node_type"`
	Status     string         `json:"status"`
	Inputs     map[string]any `json:"inputs,omitempty"`  // 解析后的实际入参
	Outputs    map[string]any `json:"outputs,omitempty"` // 产出的变量
	Error      string         `json:"error,omitempty"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
}

// nodeConfig 访问辅助（Config 是 map[string]any，按类型取段，缺省给零值）。

func (n *Node) configSlice(key string) []any {
	if n.Config == nil {
		return nil
	}
	v, _ := n.Config[key].([]any)
	return v
}

func (n *Node) configString(key string) string {
	if n.Config == nil {
		return ""
	}
	s, _ := n.Config[key].(string)
	return s
}

func (n *Node) configMap(key string) map[string]any {
	if n.Config == nil {
		return nil
	}
	m, _ := n.Config[key].(map[string]any)
	return m
}

// paramSpecs 从 config[key] 解析变量声明列表。
func (n *Node) paramSpecs(key string) []ParamSpec {
	raw := n.configSlice(key)
	out := make([]ParamSpec, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		spec := ParamSpec{
			Key:         strOf(m["key"]),
			Type:        strOf(m["type"]),
			Required:    m["required"] == true,
			Description: strOf(m["description"]),
		}
		if spec.Type == "" {
			spec.Type = "string"
		}
		out = append(out, spec)
	}
	return out
}

// branchCases 解析分支配置。
func (n *Node) branchCases() []BranchCase {
	raw := n.configSlice("cases")
	out := make([]BranchCase, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		c := BranchCase{Label: strOf(m["label"]), Next: strOf(m["next"])}
		for _, w := range anySlice(m["when"]) {
			wm, ok := w.(map[string]any)
			if !ok {
				continue
			}
			c.When = append(c.When, Condition{
				Left:  strOf(wm["left"]),
				Op:    strOf(wm["op"]),
				Right: strOf(wm["right"]),
			})
		}
		out = append(out, c)
	}
	return out
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}
