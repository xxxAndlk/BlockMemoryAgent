package tool

// forge_tools.go 实现 tool_forge 机制（Agent 会话内锻造自定义工具，v1 仅限 MetaAgent）：
//   - tool_forge  内置工具：提交 name/description/language/schema/code，脚本落盘
//     <工作目录>/.bma/forged/<name>.<ext>，注册为 forged__<name> 动态工具；
//   - forgedTool  动态工具（仿 mcpbridge remoteTool）：持 description+schema 实现
//     SchemaSource 进 Registry.Schema() 兜底块，仅锻造者 scope 可见（adapter 侧按
//     属主过滤），入参按锻造 schema 校验后以 JSON 字符串作 argv[1] 走 runCommand
//     沙箱链路执行。
//
// 可见性口径：Registry 簿记 forgeScopes（工具名→属主 scope），adapter Schema()
// 对 forged__ 工具按属主过滤（白名单为空的分支同样生效）——兄弟会话/子 Agent
// 不可见、不可调。会话结束 finalizeSession 按 scope 摘除注册（脚本文件留档不删）。
//
// 风险 posture：forgedTool.Destructive()=false——与 run_command 同等
//（脚本本来就能经 RunCommand 手动跑），不加审批摩擦。

import (
	"context"       // 工具执行上下文
	"encoding/json" // schema 合法性校验与入参序列化
	"fmt"           // 错误文案
	"os"            // 脚本写盘
	"path/filepath" // .bma/forged/ 路径拼接
	"regexp"        // 锻造名合法性
	"strings"       // 文案拼接与行数统计
	"unicode/utf8"  // description 字数（rune）统计

	"github.com/google/jsonschema-go/jsonschema" // 入参 schema
)

const (
	// forgedPrefix 是锻造工具的注册名前缀：跨会话防撞（与插件 "<id>__" 前缀同构），
	// 同时是 adapter 可见性过滤的识别标志。
	forgedPrefix = "forged__"

	// maxForgedPerSession 是单 scope（会话）锻造工具数量上限：防失控铺量——
	// 超限提示改用内置工具或合并既有锻造。
	maxForgedPerSession = 8

	// maxForgeDescriptionRunes 是锻造描述长度上限（字/rune）。
	maxForgeDescriptionRunes = 300
	// maxForgeCodeLines 是锻造脚本行数上限。
	maxForgeCodeLines = 500
	// maxForgeCodeBytes 是锻造脚本字节数上限。
	maxForgeCodeBytes = 32 * 1024

	// forgedDirName 是锻造脚本落盘目录（<工作目录>/.bma/forged/）。
	forgedDirName = "forged"
)

// forgeNameRe 校验锻造名：小写字母开头，小写字母/数字/连字符，≤48 字符。
var forgeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// forgeExt 是语言到脚本扩展名/解释器的映射（v1 只做 py|sh|js，不做 ts）。
var forgeExt = map[string]string{"py": ".py", "sh": ".sh", "js": ".js"}

// forgeRunner 是语言到命令解释器的映射：执行经 runCommand（cwd=会话工作目录、
// 命令守卫、超时树杀全在链路内），sh 显式 bash（Windows 下由 runCommand 既有
// git-bash 选择逻辑承载）。
var forgeRunner = map[string]string{"py": "python", "sh": "bash", "js": "node"}

// ---- tool_forge ----

// forgeTool 实现 tool_forge：校验入参 → 脚本写盘 .bma/forged/ → RegisterForged
// 登记为 forged__<name>（属主 scope = 当前 agentID，回退 sessionID）。
type forgeTool struct {
	exec     *Executor // 取会话工作目录（workDirOf 同款口径）
	registry *Registry // 锻造簿记与动态注册
}

// Name 返回工具名称。
func (t *forgeTool) Name() string { return "tool_forge" }

// Aliases 返回别名列表（无别名）。
func (t *forgeTool) Aliases() []string { return nil }

// Description 返回工具功能描述（注入 LLM function calling schema）。
func (t *forgeTool) Description() string {
	return "锻造自定义工具：提交 name/description/language(py|sh|js)/schema/code，脚本落盘 .bma/forged/ 并注册为 forged__<name>，" +
		"仅本会话可见。定位是最后手段——内置工具（含 HTTPGet/RunCommand/插件工具）能干的事禁止锻造；" +
		"适合需要参数化复用的工艺（本轮要多次以不同入参调用）或内置组合表达笨拙的场景。" +
		"入参按锻造 schema 校验后以 JSON 字符串作脚本第 1 个命令行参数传入。"
}

// InputSchema 返回入参 JSON Schema。
func (t *forgeTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":        {Type: "string", Description: "锻造工具名（^[a-z][a-z0-9-]{0,47}$；最终注册名 = forged__<name>）"},
			"description": {Type: "string", Description: "一句话用途（≤300 字，注入 forged__<name> 的工具描述）"},
			"language":    {Type: "string", Description: "脚本语言：py / sh / js（v1 不做 ts）"},
			"schema":      {Type: "object", Description: "入参 JSON Schema（可选，必须 object 类型、简单平铺字段；缺省 = 无参数工具）"},
			"code":        {Type: "string", Description: "脚本内容（≤500 行、≤32KB，非空；入参经 argv[1] 以 JSON 字符串传入）"},
		},
		Required: []string{"name", "description", "language", "code"},
	}
}

// Execute 逐字段校验（仿 widget_tools.go 风格，失败 validation_rejected）→ 写盘 →
// 登记。同名再锻造：属主 scope 相同 = 覆盖更新；不同 = 拒绝提示改名。
func (t *forgeTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "tool_forge"}
	reject := func(msg string) *Result {
		res.Error = msg
		res.Category = ResultCategoryValidationRejected
		return res
	}

	// name：合法性 + 前缀防撞后的最终名。
	name := strings.TrimSpace(strArg(args, "name"))
	if !forgeNameRe.MatchString(name) {
		return reject(fmt.Sprintf("name 非法 %q：须匹配 ^[a-z][a-z0-9-]{0,47}$（小写开头，小写字母/数字/连字符，≤48 字符）", name))
	}
	fullName := forgedPrefix + name

	// description：一句话用途，≤300 字。
	description := strings.TrimSpace(strArg(args, "description"))
	if description == "" {
		return reject("description 必填（一句话用途，注入 forged 工具描述）")
	}
	if n := utf8.RuneCountInString(description); n > maxForgeDescriptionRunes {
		return reject(fmt.Sprintf("description 超长：%d 字 > 上限 %d 字", n, maxForgeDescriptionRunes))
	}

	// language：py|sh|js 三选一。
	language := strings.TrimSpace(strArg(args, "language"))
	if _, ok := forgeExt[language]; !ok {
		return reject(fmt.Sprintf("language 非法 %q：仅支持 py / sh / js（v1 不做 ts）", language))
	}

	// code：非空、≤500 行、≤32KB。
	code := strArg(args, "code")
	if strings.TrimSpace(code) == "" {
		return reject("code 必填（脚本内容，非空）")
	}
	if lines := strings.Count(code, "\n") + 1; lines > maxForgeCodeLines {
		return reject(fmt.Sprintf("code 超长：%d 行 > 上限 %d 行", lines, maxForgeCodeLines))
	}
	if len(code) > maxForgeCodeBytes {
		return reject(fmt.Sprintf("code 超大：%d 字节 > 上限 %d 字节", len(code), maxForgeCodeBytes))
	}

	// schema：可选。须能 marshal→unmarshal 成 jsonschema.Schema 且 type=object
	//（与 mcpbridge wrapTool 同款转换口径）；缺省 = 无参数工具。
	var schema *jsonschema.Schema
	if raw, ok := args["schema"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err != nil {
			return reject(fmt.Sprintf("schema 非法：无法序列化为 JSON（%v）", err))
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(b, &s); err != nil {
			return reject(fmt.Sprintf("schema 非法：不是合法 JSON Schema（%v）", err))
		}
		if s.Type != "object" {
			return reject(fmt.Sprintf("schema 必须为 object 类型（顶层 type: %q），简单平铺字段（string/number/boolean），勿嵌套深层对象", s.Type))
		}
		schema = &s
	}

	// 属主 scope：与 adapter 可见性过滤同口径（agentID 回退 sessionID）；
	// adapter Dispatch 已在 ctx 缺失时以 adapter.scope 补齐。
	scope := scopeKeyFromCtx(ctx)
	if scope == "" {
		return reject("无法确定锻造属主（缺少会话/Agent 上下文）：请在会话内经 tool_forge 调用")
	}

	// 同名再锻造：属主不同 = 名字被其他会话占用，拒绝提示改名。
	if owner, ok := t.registry.ForgedScope(fullName); ok && owner != scope {
		return reject(fmt.Sprintf("名字 %s 已被其他会话占用（属主 scope=%s），请改名锻造", fullName, owner))
	}

	// 数量上限：同 scope 覆盖更新不占新名额，新名字才计数。
	if t.registry.ForgedScopeOwnedByCount(scope) >= maxForgedPerSession {
		if _, own := t.registry.ForgedScope(fullName); !own {
			return reject(fmt.Sprintf("本会话锻造工具已达上限 %d 个：请改用内置工具，或合并/覆盖既有锻造", maxForgedPerSession))
		}
	}

	// 写盘：<workDir>/.bma/forged/<name>.<ext>（0o644）。写盘失败返回 error 结果。
	workDir := t.exec.workDirOf(ctx)
	forgedDir := filepath.Join(workDir, ".bma", forgedDirName)
	if err := os.MkdirAll(forgedDir, 0o755); err != nil {
		res.Error = "创建锻造目录失败: " + err.Error()
		return res
	}
	scriptPath := filepath.Join(forgedDir, name+forgeExt[language])
	if err := os.WriteFile(scriptPath, []byte(code), 0o644); err != nil {
		res.Error = "写入锻造脚本失败: " + err.Error()
		return res
	}

	// 登记为 forged__<name>，仅锻造者 scope 可见。
	forged := &forgedTool{
		name:        fullName,
		description: description,
		schema:      schema,
		scriptPath:  scriptPath,
		executor:    t.exec,
		language:    language,
	}
	if err := t.registry.RegisterForged(forged, scope); err != nil {
		res.Error = err.Error()
		return res
	}

	res.Success = true
	res.Output = fmt.Sprintf("工具已锻造：%s，本轮起可直接调用；参数以 JSON 字符串作第 1 个命令行参数传入。脚本落盘 %s",
		fullName, filepath.ToSlash(scriptPath))
	return res
}

// ---- forged__<name> ----

// forgedTool 是锻造产出的动态工具（仿 mcpbridge remoteTool）：持 description+schema
// 实现 SchemaSource 进 Registry.Schema() 兜底块；Execute 按锻造 schema 校验入参后经
// runCommand 沙箱链路执行脚本。
type forgedTool struct {
	name        string             // 注册名 forged__<name>
	description string             // 锻造时提交的一句话用途
	schema      *jsonschema.Schema // 入参 schema；nil = 无参数
	scriptPath  string             // 脚本绝对路径（<workDir>/.bma/forged/<name>.<ext>）
	executor    *Executor          // 复用 runCommand 沙箱链路
	language    string             // py|sh|js
}

// Name 实现 tool.Tool。
func (t *forgedTool) Name() string { return t.name }

// Aliases 实现 tool.Tool：无别名（锻造名即唯一标识）。
func (t *forgedTool) Aliases() []string { return nil }

// Description 实现 SchemaSource：锻造描述 + 自动后缀（取参方式与落盘位置）。
func (t *forgedTool) Description() string {
	return t.description + fmt.Sprintf("（本会话锻造工具，脚本 .bma/forged/%s%s；入参按锻造 schema 校验后以 JSON 字符串作 argv[1] 传入）",
		strings.TrimPrefix(t.name, forgedPrefix), forgeExt[t.language])
}

// InputSchema 实现 SchemaSource：透传锻造 schema（nil 时退化为空对象 schema）。
func (t *forgedTool) InputSchema() *jsonschema.Schema {
	if t.schema == nil {
		return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
	}
	return t.schema
}

// Destructive 返回 false：与 run_command 同等风险 posture（脚本本来就能经
// RunCommand 跑），不加审批摩擦。
func (t *forgedTool) Destructive() bool { return false }

// Execute 实现 tool.Tool：入参按锻造 schema 校验（失败 validation_rejected，错误
// 指明字段）→ JSON 序列化为单个字符串 argv → runCommand 执行 → 结果原样回。
func (t *forgedTool) Execute(ctx context.Context, args map[string]any) *Result {
	if errs := validateForgedArgs(t.schema, args); len(errs) > 0 {
		return &Result{Tool: t.name, Category: ResultCategoryValidationRejected,
			Error: "入参不符合锻造 schema：" + strings.Join(errs, "；")}
	}
	argJSON, err := marshalNoHTMLEscape(args)
	if err != nil {
		return &Result{Tool: t.name, Error: "序列化入参失败: " + err.Error()}
	}
	// 组装 `runner <script> '<json>'`：单引号包裹防 shell 展开（POSIX 单引号转义），
	// 由 runCommand 选解释器执行（Windows 优先 Git Bash）。注意命令守卫对整串做
	// 黑名单匹配——入参值若含被禁命令词（如 " rm "）会被 fail-safe 拦截（宁可误杀）。
	cmd := forgeRunner[t.language] + " " + shellQuote(t.scriptPath) + " " + shellQuote(string(argJSON))
	res := t.executor.runCommand(ctx, map[string]any{"command": cmd})
	res.Tool = t.name
	return res
}

// shellQuote 把字符串包成 POSIX shell 单引号安全形式（内嵌单引号以 close+backslash 序列转义）。
// 与 runCommand 的解释器口径配套（bash -c）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// validateForgedArgs 手写最小实例校验：jsonschema-go v0.4.3 仅为 schema 定义库
// （无 Validate/ValidatorFor 实例校验 API），故按锻造纪律的"简单平铺字段"范围
// 手写一层校验：required 存在性 + properties 声明类型的 JSON 类型匹配（一层）。
// 嵌套对象/数组元素不做递归校验（锻造纪律禁止深层嵌套，注释声明口径）。
func validateForgedArgs(schema *jsonschema.Schema, args map[string]any) []string {
	if schema == nil {
		return nil
	}
	var errs []string
	for _, req := range schema.Required {
		if _, ok := args[req]; !ok {
			errs = append(errs, fmt.Sprintf("缺少必填字段 %q", req))
		}
	}
	for key, prop := range schema.Properties {
		v, ok := args[key]
		if !ok || v == nil {
			continue
		}
		if prop == nil || prop.Type == "" {
			continue
		}
		if got := jsonTypeOf(v); got != prop.Type {
			errs = append(errs, fmt.Sprintf("字段 %q 类型不符：期望 %s，实得 %s", key, prop.Type, got))
		}
	}
	return errs
}

// jsonTypeOf 返回任意 JSON 反序列化值的 JSON Schema 类型名。
// 数值统一报 "number"（整数场景锻造纪律用 number 表达，integer 由调用方特判）。
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "number" // float64/int 等数值类型
	}
}
