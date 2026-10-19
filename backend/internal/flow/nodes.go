package flow

// nodes.go 变量引用解析 + 各类型节点的执行逻辑（v1：llm/tool/branch/answer/start；
// approval 的挂起/裁决在 engine.go，因为涉及 run 级状态机）。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/blockmemory/agent/backend/internal/model"
)

// scope 是运行时变量作用域：节点 ID → 该节点产出的变量表。
type scope map[string]map[string]any

// stringify 把变量值转成插值字符串（string 原样，其余 JSON 编码）。
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// resolveString 展开 s 中的 {{node.key}} 引用；解析不到返回错误（引用节点未执行/未声明该输出）。
func resolveString(s string, sc scope) (string, error) {
	var rerr error
	out := varRefPattern.ReplaceAllStringFunc(s, func(m string) string {
		if rerr != nil {
			return m
		}
		parts := varRefPattern.FindStringSubmatch(m)
		v, err := lookupVar(sc, parts[1], parts[2])
		if err != nil {
			rerr = err
			return m
		}
		return stringify(v)
	})
	if rerr != nil {
		return "", rerr
	}
	return out, nil
}

// resolveValue 递归解析配置值：含 {{}} 的字符串展开，map/array 递归，其余原样。
func resolveValue(v any, sc scope) (any, error) {
	switch t := v.(type) {
	case string:
		return resolveString(t, sc)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			rv, err := resolveValue(vv, sc)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, vv := range t {
			rv, err := resolveValue(vv, sc)
			if err != nil {
				return nil, err
			}
			out = append(out, rv)
		}
		return out, nil
	default:
		return v, nil
	}
}

// lookupVar 按 nodeID + keyPath（支持 items.0.name 点路径）查变量。
func lookupVar(sc scope, nodeID, keyPath string) (any, error) {
	nodeOut, ok := sc[nodeID]
	if !ok {
		return nil, fmt.Errorf("引用的节点 %s 尚未产出变量", nodeID)
	}
	var cur any = nodeOut
	for _, seg := range strings.Split(keyPath, ".") {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[seg]
			if !ok {
				return nil, fmt.Errorf("节点 %s 未产出变量 %q", nodeID, keyPath)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("节点 %s 变量 %q 下标越界", nodeID, keyPath)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("节点 %s 变量 %q 路径不可深入", nodeID, keyPath)
		}
	}
	return cur, nil
}

// evalStart 执行 start 节点：把 run 输入按声明过滤产出（required 校验在建 run 时已做）。
func evalStart(n *Node, inputs map[string]any) map[string]any {
	out := map[string]any{}
	for _, spec := range n.paramSpecs("inputs") {
		if v, ok := inputs[spec.Key]; ok {
			out[spec.Key] = v
		}
	}
	return out
}

// evalLLM 执行 llm 节点：prompt 已在外层展开变量；按角色/轻量模型调用 → 解析声明的输出字段。
// 提示词尾追加"只输出 JSON"指令，回复按 JSON 对象解析；解析失败且仅声明单输出时
// 整体文本进该输出。
func (e *Engine) evalLLM(ctx context.Context, n *Node, prompt string) (map[string]any, error) {
	specs := n.paramSpecs("outputs")
	prompt += "\n\n只输出一个 JSON 对象，不要输出其他内容。字段要求：" + jsonFieldRequirement(specs)

	// 模型选择：model=角色 ID（走该角色的模型绑定），空=轻量模型。
	var client model.LLMClient
	var err error
	roleID := strings.TrimSpace(n.configString("model"))
	if roleID != "" {
		client, err = e.models.GetModel(ctx, roleID)
	} else {
		client, err = e.models.GetLightweightModel(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("取模型失败: %w", err)
	}
	var text string
	if temp, ok := n.Config["temperature"].(float64); ok && temp > 0 {
		text, err = model.GenerateWithTemperature(ctx, client, prompt, temp)
	} else {
		text, err = client.Generate(ctx, prompt)
	}
	if err != nil {
		return nil, fmt.Errorf("模型调用失败: %w", err)
	}

	parsed := parseLLMJSON(text)
	out := map[string]any{}
	matched := 0
	for _, spec := range specs {
		if v, ok := parsed[spec.Key]; ok {
			out[spec.Key] = v
			matched++
		}
	}
	if matched == 0 {
		if len(specs) == 1 {
			out[specs[0].Key] = strings.TrimSpace(text)
			return out, nil
		}
		return nil, fmt.Errorf("模型回复未解析出声明的输出字段（声明 %d 个，命中 0 个）", len(specs))
	}
	return out, nil
}

// jsonFieldRequirement 生成"只输出 JSON"指令里的字段说明。
func jsonFieldRequirement(specs []ParamSpec) string {
	typeName := map[string]string{
		"string": "字符串", "number": "数字", "boolean": "布尔", "array": "数组", "object": "对象",
	}
	parts := make([]string, 0, len(specs))
	for _, s := range specs {
		tn := typeName[s.Type]
		if tn == "" {
			tn = "字符串"
		}
		parts = append(parts, fmt.Sprintf("%q（%s）", s.Key, tn))
	}
	return strings.Join(parts, "、") + "。"
}

// llmJSONBlock 截取回复里第一个 {...} 块（容忍模型前后加废话）。
var llmJSONBlock = regexp.MustCompile(`(?s)\{.*\}`)

// parseLLMJSON 从模型回复解析 JSON 对象（取第一个 {...} 块；失败返回空表）。
func parseLLMJSON(text string) map[string]any {
	out := map[string]any{}
	m := llmJSONBlock.FindString(text)
	if m == "" {
		return out
	}
	// 贪婪匹配可能吞掉尾部多对象，只取到第一个 balanced 结束（简单截断：最后一个大括号前的内容
	// 对"单对象+尾噪"场景够用；真数组场景由调用方兜底整体文本）。
	if err := json.Unmarshal([]byte(m), &out); err != nil {
		// 退回：找第一个 } 收尾的最短前缀。
		if i := strings.Index(m, "}"); i > 0 {
			_ = json.Unmarshal([]byte(m[:i+1]), &out)
		}
	}
	return out
}

// evalTool 执行 tool 节点：args 已在外层展开变量 → 程序化 Dispatch → 产出 result。
// 工具成功与否进 node_run 状态（失败即 run 失败，v1 无重试/失败分支）。
func (e *Engine) evalTool(ctx context.Context, run *Run, toolName string, argMap map[string]any) (map[string]any, error) {
	dctx := toolDispatchCtx(ctx, run)
	res, derr := e.tools.Dispatch(dctx, toolName, argMap)
	if derr != nil {
		return nil, fmt.Errorf("工具调度失败: %w", derr)
	}
	if !res.Success {
		return nil, fmt.Errorf("工具执行失败: %s", res.Error)
	}
	return map[string]any{"result": res.Output, "path": res.Path}, nil
}

// evalBranch 执行 branch 节点：按序首个全条件命中的 case 返回其 next，否则 default。
func evalBranch(n *Node, sc scope) (string, error) {
	for _, c := range n.branchCases() {
		hit, err := evalConditions(c.When, sc)
		if err != nil {
			return "", err
		}
		if hit {
			return c.Next, nil
		}
	}
	def := strings.TrimSpace(n.configString("default"))
	if def == "" {
		return "", fmt.Errorf("branch 未命中任何分支且未配置 default")
	}
	return def, nil
}

// evalConditions 全部条件成立（AND）才命中。
func evalConditions(when []Condition, sc scope) (bool, error) {
	for _, cond := range when {
		ok, err := evalCondition(cond, sc)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// evalCondition 求值单个条件。exists/not_exists 对未定义引用容忍（其余引用未定义是错误）。
func evalCondition(cond Condition, sc scope) (bool, error) {
	if cond.Op == "exists" || cond.Op == "not_exists" {
		_, err := resolveOperand(cond.Left, sc)
		exists := err == nil
		if cond.Op == "exists" {
			return exists, nil
		}
		return !exists, nil
	}
	left, lerr := resolveOperand(cond.Left, sc)
	right, rerr := resolveOperand(cond.Right, sc)
	if lerr != nil {
		return false, lerr
	}
	if rerr != nil {
		return false, rerr
	}
	ls, rs := stringify(left), stringify(right)
	switch cond.Op {
	case "==":
		return ls == rs, nil
	case "!=":
		return ls != rs, nil
	case "contains":
		return strings.Contains(ls, rs), nil
	case "not_contains":
		return !strings.Contains(ls, rs), nil
	}
	// 数值比较：两边都能解析成数字按数字比，否则按字符串字典序。
	lf, lerr2 := strconv.ParseFloat(ls, 64)
	rf, rerr2 := strconv.ParseFloat(rs, 64)
	if lerr2 == nil && rerr2 == nil {
		switch cond.Op {
		case ">":
			return lf > rf, nil
		case "<":
			return lf < rf, nil
		case ">=":
			return lf >= rf, nil
		case "<=":
			return lf <= rf, nil
		}
	}
	switch cond.Op {
	case ">":
		return ls > rs, nil
	case "<":
		return ls < rs, nil
	case ">=":
		return ls >= rs, nil
	case "<=":
		return ls <= rs, nil
	}
	return false, fmt.Errorf("未知操作符 %q", cond.Op)
}

// resolveOperand 解析操作数：{{}} 引用求值，其余按字面量。
func resolveOperand(s string, sc scope) (any, error) {
	s = strings.TrimSpace(s)
	if m := varRefPattern.FindStringSubmatch(s); m != nil && m[0] == s {
		return lookupVar(sc, m[1], m[2])
	}
	return s, nil
}

// evalAnswer 执行 answer 节点：解析 outputs 模板，作为 run 的最终输出。
func evalAnswer(n *Node, sc scope) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range n.configMap("outputs") {
		rv, err := resolveValue(v, sc)
		if err != nil {
			return nil, fmt.Errorf("answer.%s: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}
