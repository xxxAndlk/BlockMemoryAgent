package flow

// definition.go flow 定义的加载/校验/落盘。真相源 = 工作区 `.bma/flows/<id>.json`
//（随项目持久、git 可版本化，不落库）；run 启动时快照定义全文进 flow_runs.flow_json。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// idPattern flow/节点 id 合法形状（进文件名与变量引用语法，限安全字符集）。
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]*$`)

// varRefPattern 是 {{node.key}} 变量引用语法（key 支持点路径，如 items.0.name）。
var varRefPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_\-]+)\.([A-Za-z0-9_\-\.]+)\s*\}\}`)

// keyPattern 是变量 key（ParamSpec.Key）合法形状。
var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-]*$`)

// paramTypes 是支持的变量类型（校验宽松，仅声明用途）。
var paramTypes = map[string]bool{
	"string": true, "number": true, "boolean": true, "array": true, "object": true,
}

// conditionOps 是 branch 条件支持的操作符。
var conditionOps = map[string]bool{
	"==": true, "!=": true, "contains": true, "not_contains": true,
	">": true, "<": true, ">=": true, "<=": true,
	"exists": true, "not_exists": true,
}

// FlowsDir 返回工作区的 flow 定义目录（<workdir>/.bma/flows）。
func FlowsDir(workdir string) string {
	return filepath.Join(workdir, ".bma", "flows")
}

// ParseFlow 解析 flow JSON 并校验。
func ParseFlow(data []byte) (*Flow, error) {
	var f Flow
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse flow json: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// MarshalFlow 序列化 flow 为 JSON（缩进两格，进文件可 diff）。
func MarshalFlow(f *Flow) ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal flow json: %w", err)
	}
	return append(data, '\n'), nil
}

// Validate 校验 flow 结构完整性（保存与加载都走这里）。
// 校验项：id/节点 id 字符集与唯一性、类型已知、依赖存在、无环、start/answer 数量、
// 各类型 config 必填段、{{node.key}} 引用可静态解析（节点存在 + 该节点声明了该输出）。
func (f *Flow) Validate() error {
	if f == nil {
		return fmt.Errorf("flow is nil")
	}
	f.ID = strings.TrimSpace(f.ID)
	if f.ID == "" {
		return fmt.Errorf("flow.id is required")
	}
	if !idPattern.MatchString(f.ID) {
		return fmt.Errorf("flow.id 非法（限字母数字下划线连字符）: %q", f.ID)
	}
	if f.Version < 0 {
		return fmt.Errorf("flow.version 不能为负")
	}
	if len(f.Nodes) == 0 {
		return fmt.Errorf("flow.nodes 不能为空")
	}

	byID := make(map[string]*Node, len(f.Nodes))
	startCount, answerCount := 0, 0
	for i, n := range f.Nodes {
		if n == nil || strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("flow.nodes[%d].id is required", i)
		}
		n.ID = strings.TrimSpace(n.ID)
		if !idPattern.MatchString(n.ID) {
			return fmt.Errorf("节点 id 非法 %q（限字母数字下划线连字符）", n.ID)
		}
		if byID[n.ID] != nil {
			return fmt.Errorf("节点 id 重复: %q", n.ID)
		}
		if !nodeTypes[n.Type] {
			return fmt.Errorf("节点 %s 类型未知 %q（可用: start/llm/tool/branch/approval/answer）", n.ID, n.Type)
		}
		byID[n.ID] = n
		switch n.Type {
		case NodeTypeStart:
			startCount++
		case NodeTypeAnswer:
			answerCount++
		}
	}
	if startCount != 1 {
		return fmt.Errorf("flow 必须有且仅有一个 start 节点（当前 %d 个）", startCount)
	}
	if answerCount == 0 {
		return fmt.Errorf("flow 至少需要一个 answer 节点")
	}

	// 依赖存在性 + 无环（复用 pkg/types 的 Kahn 拓扑排序）。
	tasks := make([]*types.Task, 0, len(f.Nodes))
	for _, n := range f.Nodes {
		deps := n.DependsOn
		if n.Type == NodeTypeStart && len(deps) > 0 {
			return fmt.Errorf("start 节点 %s 不允许有 depends_on", n.ID)
		}
		for _, d := range deps {
			if byID[d] == nil {
				return fmt.Errorf("节点 %s 依赖不存在的节点 %q", n.ID, d)
			}
		}
		tasks = append(tasks, &types.Task{ID: n.ID, DependsOn: deps})
	}
	if _, err := types.TopoSort(tasks); err != nil {
		return fmt.Errorf("flow 依赖存在环或重复: %w", err)
	}

	// 各类型 config 校验。
	for _, n := range f.Nodes {
		if err := validateNodeConfig(n); err != nil {
			return err
		}
	}

	// branch 路由目标（case.next/default）必须指向存在的节点。
	for _, n := range f.Nodes {
		if n.Type != NodeTypeBranch {
			continue
		}
		for _, c := range n.branchCases() {
			if byID[c.Next] == nil {
				return fmt.Errorf("节点 %s: 分支 %q 目标节点 %q 不存在", n.ID, c.Label, c.Next)
			}
		}
		if def := strings.TrimSpace(n.configString("default")); def != "" && byID[def] == nil {
			return fmt.Errorf("节点 %s: default 目标节点 %q 不存在", n.ID, def)
		}
	}

	// 变量引用静态解析：{{node.key}} 必须指向已声明的输出。
	for _, n := range f.Nodes {
		if err := f.validateNodeRefs(n, byID); err != nil {
			return err
		}
	}
	return nil
}

// validateNodeConfig 校验单个节点的类型专属配置。
func validateNodeConfig(n *Node) error {
	switch n.Type {
	case NodeTypeStart:
		for _, spec := range n.paramSpecs("inputs") {
			if err := validateParamSpec(n.ID, spec); err != nil {
				return err
			}
		}
	case NodeTypeLLM:
		if strings.TrimSpace(n.configString("prompt")) == "" {
			return fmt.Errorf("节点 %s: llm.prompt 不能为空", n.ID)
		}
		outs := n.paramSpecs("outputs")
		if len(outs) == 0 {
			return fmt.Errorf("节点 %s: llm.outputs 至少声明一个输出字段", n.ID)
		}
		for _, spec := range outs {
			if err := validateParamSpec(n.ID, spec); err != nil {
				return err
			}
		}
	case NodeTypeTool:
		if strings.TrimSpace(n.configString("tool")) == "" {
			return fmt.Errorf("节点 %s: tool.tool（工具名）不能为空", n.ID)
		}
	case NodeTypeBranch:
		cases := n.branchCases()
		if len(cases) == 0 {
			return fmt.Errorf("节点 %s: branch.cases 至少一条分支", n.ID)
		}
		for i, c := range cases {
			if len(c.When) == 0 {
				return fmt.Errorf("节点 %s 分支[%d]: when 至少一个条件", n.ID, i)
			}
			for _, cond := range c.When {
				if strings.TrimSpace(cond.Left) == "" {
					return fmt.Errorf("节点 %s 分支[%d]: 条件 left 不能为空", n.ID, i)
				}
				if !conditionOps[cond.Op] {
					return fmt.Errorf("节点 %s 分支[%d]: 未知操作符 %q", n.ID, i, cond.Op)
				}
			}
		}
	case NodeTypeApproval:
		if strings.TrimSpace(n.configString("question")) == "" {
			return fmt.Errorf("节点 %s: approval.question 不能为空", n.ID)
		}
	case NodeTypeAnswer:
		outs := n.configMap("outputs")
		if len(outs) == 0 {
			return fmt.Errorf("节点 %s: answer.outputs 至少一个输出", n.ID)
		}
	}
	return nil
}

// validateParamSpec 校验变量声明。
func validateParamSpec(nodeID string, spec ParamSpec) error {
	if !keyPattern.MatchString(spec.Key) {
		return fmt.Errorf("节点 %s: 变量 key 非法 %q（字母开头，限字母数字下划线连字符）", nodeID, spec.Key)
	}
	if !paramTypes[spec.Type] {
		return fmt.Errorf("节点 %s: 变量 %s 类型未知 %q", nodeID, spec.Key, spec.Type)
	}
	return nil
}

// declaredOutputs 返回节点声明的输出变量表（引用校验用）。
// start=inputs；llm=outputs；tool=固定 result；approval=approved/comment；branch=无；answer=终态不进引用。
func declaredOutputs(n *Node) map[string]bool {
	out := map[string]bool{}
	switch n.Type {
	case NodeTypeStart:
		for _, spec := range n.paramSpecs("inputs") {
			out[spec.Key] = true
		}
	case NodeTypeLLM:
		for _, spec := range n.paramSpecs("outputs") {
			out[spec.Key] = true
		}
	case NodeTypeTool:
		out["result"] = true
		out["path"] = true
	case NodeTypeApproval:
		out["approved"] = true
		out["comment"] = true
	}
	return out
}

// validateNodeRefs 校验节点配置里所有 {{node.key}} 引用可静态解析。
// 逐字符串字段扫描（prompt/question/args/answer.outputs/branch 条件）。
func (f *Flow) validateNodeRefs(n *Node, byID map[string]*Node) error {
	check := func(where, s string) error {
		for _, m := range varRefPattern.FindAllStringSubmatch(s, -1) {
			refNode, refKey := m[1], m[2]
			target := byID[refNode]
			if target == nil {
				return fmt.Errorf("节点 %s %s: 引用了不存在的节点 %q", n.ID, where, refNode)
			}
			// key 可能带 .0.name 点路径，校验首段即可。
			first := refKey
			if i := strings.Index(refKey, "."); i >= 0 {
				first = refKey[:i]
			}
			if !declaredOutputs(target)[first] {
				return fmt.Errorf("节点 %s %s: 节点 %s 未声明输出变量 %q", n.ID, where, refNode, first)
			}
		}
		return nil
	}
	// 递归遍历配置值要先声明再赋值（:= 的标识符在函数字面量内部不可见）。
	var checkValue func(where string, v any) error
	checkValue = func(where string, v any) error {
		switch t := v.(type) {
		case string:
			return check(where, t)
		case map[string]any:
			for k, vv := range t {
				if err := checkValue(where+"."+k, vv); err != nil {
					return err
				}
			}
		case []any:
			for i, vv := range t {
				if err := checkValue(fmt.Sprintf("%s[%d]", where, i), vv); err != nil {
					return err
				}
			}
		}
		return nil
	}

	switch n.Type {
	case NodeTypeLLM:
		if err := check("prompt", n.configString("prompt")); err != nil {
			return err
		}
		if err := check("system", n.configString("system")); err != nil {
			return err
		}
	case NodeTypeTool:
		if err := checkValue("args", n.configMap("args")); err != nil {
			return err
		}
	case NodeTypeApproval:
		if err := check("question", n.configString("question")); err != nil {
			return err
		}
	case NodeTypeAnswer:
		for k, v := range n.configMap("outputs") {
			if err := checkValue("outputs."+k, v); err != nil {
				return err
			}
		}
	case NodeTypeBranch:
		for i, c := range n.branchCases() {
			for j, cond := range c.When {
				w := fmt.Sprintf("cases[%d].when[%d]", i, j)
				if err := check(w+".left", cond.Left); err != nil {
					return err
				}
				if err := check(w+".right", cond.Right); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// NodeByID 按 id 查节点（不存在返回 nil）。
func (f *Flow) NodeByID(id string) *Node {
	for _, n := range f.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// validateFlowID 防路径穿越（id 直接进文件名）。
func validateFlowID(id string) error {
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("非法 flow id: %q", id)
	}
	return nil
}

// FlowFilePath 返回 flow 文件绝对路径。
func FlowFilePath(workdir, id string) (string, error) {
	if err := validateFlowID(id); err != nil {
		return "", err
	}
	return filepath.Join(FlowsDir(workdir), id+".json"), nil
}

// LoadFlow 从工作区加载 flow（<workdir>/.bma/flows/<id>.json）。
func LoadFlow(workdir, id string) (*Flow, error) {
	path, err := FlowFilePath(workdir, id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("flow %q 不存在（%s）", id, path)
		}
		return nil, fmt.Errorf("读取 flow %q: %w", id, err)
	}
	f, err := ParseFlow(data)
	if err != nil {
		return nil, fmt.Errorf("flow %s: %w", path, err)
	}
	return f, nil
}

// FlowSummary 是 flow 库列表的一行。
type FlowSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Description string `json:"description,omitempty"`
	NodeCount   int    `json:"node_count"`
	Error       string `json:"error,omitempty"` // 解析失败带原因（列表不因单文件损坏而失败）
}

// ListFlows 扫描工作区 flow 目录，返回摘要列表（按 id 排序；目录不存在返回空）。
func ListFlows(workdir string) ([]FlowSummary, error) {
	dir := FlowsDir(workdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []FlowSummary{}, nil
		}
		return nil, fmt.Errorf("读取 flow 目录 %s: %w", dir, err)
	}
	out := make([]FlowSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		sum := FlowSummary{ID: strings.TrimSuffix(e.Name(), ".json")}
		data, err := os.ReadFile(path)
		if err == nil {
			var f Flow
			if err = json.Unmarshal(data, &f); err == nil {
				err = f.Validate()
			}
			if err == nil {
				sum.Name, sum.Version, sum.Description = f.Name, f.Version, f.Description
				sum.NodeCount = len(f.Nodes)
			} else {
				sum.Error = err.Error()
			}
		} else {
			sum.Error = err.Error()
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SaveFlowJSON 落盘 flow JSON：解析 → 校验 → 版本规则（文件已存在时 version 至少现版+1）
// → 原子写。bodyID 是请求路径里的 id（必须和正文一致，防张冠李戴）。
// 返回落盘后的 flow（含生效版本号）。
func SaveFlowJSON(workdir, bodyID, jsonText string) (*Flow, error) {
	bodyID = strings.TrimSpace(bodyID)
	if err := validateFlowID(bodyID); err != nil {
		return nil, err
	}
	f, err := ParseFlow([]byte(jsonText))
	if err != nil {
		return nil, err
	}
	if f.ID != bodyID {
		return nil, fmt.Errorf("路径 id %q 与正文 id %q 不一致", bodyID, f.ID)
	}
	path, err := FlowFilePath(workdir, f.ID)
	if err != nil {
		return nil, err
	}
	if existing, lerr := LoadFlow(workdir, f.ID); lerr == nil && existing != nil {
		if f.Version <= existing.Version {
			f.Version = existing.Version + 1
		}
	} else if f.Version <= 0 {
		f.Version = 1
	}
	data, err := MarshalFlow(f)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(FlowsDir(workdir), 0o755); err != nil {
		return nil, fmt.Errorf("创建 flow 目录: %w", err)
	}
	// 原子写：临时文件同目录 + rename（Windows rename 不覆盖，先删旧）。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("写 flow 临时文件: %w", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return nil, fmt.Errorf("清理旧 flow 文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("落盘 flow: %w", err)
	}
	return f, nil
}
