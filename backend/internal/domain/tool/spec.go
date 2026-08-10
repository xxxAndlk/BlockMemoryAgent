package tool

// spec.go 提供 WriteSpec 工具：派发方（MetaAgent/DomainAgent）在调用 call_sub_agent
// 前，把当前任务的结构化规范（目标/验收/约束/涉及文件）写入共享记忆的固定 slot "spec"。
// dispatcher.injectSpec 读取该 slot，作为【任务规范】前缀注入子 Agent 任务体；
// dispatcher.callSubAgentTool.Execute 在 SpecEnforcementEnabled 开启时强制 parentID:spec
// 存在且新鲜，缺失则拒绝派发，实现"规范先行"语义。
//
// 与 WriteSharedMemory 的关系：
//   - 共用 SharedMemoryStore 后端与 MD frontmatter 编码（agent/slot/files mtime），
//     复用 Layer 2 invalidateSharedMemoryForPath 与 Layer 3 verifyFileMtimes 失效机制；
//   - 固定 slot "spec"，与 WriteSharedMemory 的命名槽位（shared/file_tree/...）独立存储，
//     互不干扰；injectSpec 与 injectKVMemory 各自读取，前缀独立拼装。
//   - spec 的 goal/acceptance/constraints 额外进 frontmatter，injectSpec/hasFreshSpec
//     直接读 frontmatter 不解析 body（O(1) 解码）。

import (
	"context"
	"fmt"
	"strings"
)

// SpecSlot 是 WriteSpec 写入的固定 slot 名。
// 存储键为 "<agentID>:spec"，与 injectKVMemory 枚举的 "<agentID>:<key>" 命名一致。
const SpecSlot = "spec"

// Spec 是任务规范的结构化载体。encodeSpecMD 把字段写入 MD frontmatter 与 body。
// 字段语义：
//   - Goal：一句话任务目标；
//   - Acceptance：验收条件列表（可执行/可验证）；
//   - Constraints：约束/边界（不碰哪些、性能要求、兼容性等）；
//   - Files：涉及的文件路径列表，写入时 stat 记录 mtime，任一文件被 WriteFile 修改后该 spec 失效。
type Spec struct {
	Goal        string   `json:"goal"`
	Acceptance  []string `json:"acceptance,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	Files       []string `json:"files,omitempty"`
}

// writeSpecTool 是 WriteSpec 工具的封装。
type writeSpecTool struct {
	store SharedMemoryStore
}

// Name 返回工具标准名称 WriteSpec。
func (t *writeSpecTool) Name() string { return "WriteSpec" }

// Aliases 返回 WriteSpec 的别名列表。
func (t *writeSpecTool) Aliases() []string {
	return []string{"write_spec", "writeSpec"}
}

// Description 返回工具的人类可读描述，供 schema 与 UI 展示。
func (t *writeSpecTool) Description() string {
	return "派发子 Agent 前写入结构化任务规范（目标/验收/约束/涉及文件），" +
		"dispatcher 会强制 call_sub_agent 前先调本工具，并把规范作为【任务规范】前缀注入子 Agent。" +
		"files 字段填涉及的文件路径列表，写入时记录 mtime；任一文件被 WriteFile 修改后该规范自动失效，" +
		"下次派发子 Agent 不再注入旧规范。" +
		"例外：logs/ 与 .bma/ 目录下的文件是系统持续写入的活体文件（日志等），豁免 mtime 校验、不会导致失效。" +
		"\n覆盖语义：同一 parent 的写入覆盖前一次内容（不追加）。每个 parent 只存一份 spec，兄弟子 Agent 共享。"
}

// Execute 写入任务规范。
// 入参 goal/acceptance/constraints/files 结构化组装为 Spec，encodeSpecMD 编码为 MD
// 存入共享记忆，键为 "<agentID>:spec"。goal 与 acceptance 至少一项非空才视为合法规范。
func (t *writeSpecTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.store == nil {
		return &Result{Tool: "WriteSpec", Error: "shared memory store not configured"}
	}
	goal, _ := args["goal"].(string)
	goal = strings.TrimSpace(goal)
	acceptance := parseStringListArg(args["acceptance"])
	constraints := parseStringListArg(args["constraints"])
	files := parseFilesArg(args["files"])

	if goal == "" {
		return &Result{Tool: "WriteSpec", Error: "goal is required", Category: ResultCategoryValidationRejected}
	}
	if len(acceptance) == 0 {
		return &Result{Tool: "WriteSpec", Error: "acceptance is required (at least one verifiable condition)", Category: ResultCategoryValidationRejected}
	}

	agentID := AgentIDFromContext(ctx)
	if agentID == "" {
		return &Result{Tool: "WriteSpec", Error: "missing agent identity in context"}
	}

	spec := Spec{
		Goal:        goal,
		Acceptance:  acceptance,
		Constraints: constraints,
		Files:       files,
	}

	// 构建文件 mtime 索引：stat 各 path，规范化为绝对路径。
	// 该 map 进 MD frontmatter，供 dispatcher.verifyFileMtimes 校验与 registry.invalidateSharedMemoryForPath 反查失效。
	filesMtime := make(map[string]int64, len(files))
	for _, p := range files {
		ap, mt, ok := statFile(p)
		if !ok {
			continue
		}
		filesMtime[ap] = mt
	}

	// 编码为结构化 MD：frontmatter 含 goal/acceptance/constraints/files mtime（机器读），
	// body 为人读 MD（# 任务规范 / ## 目标 / ## 验收条件 / ...）。
	md := encodeSpecMD(agentID, spec, filesMtime)

	key := agentID + ":" + SpecSlot
	if err := t.store.Set(ctx, key, md); err != nil {
		return &Result{Tool: "WriteSpec", Error: fmt.Sprintf("set: %v", err)}
	}
	out := fmt.Sprintf("spec written (key=%s, goal=%q, %d acceptance, %d constraints, %d files tracked)", key, truncateRunesForLog(goal, 60), len(acceptance), len(constraints), len(filesMtime))
	// 防御纵深（TODO #30）：files 含持续增长目录（logs/.bma）时当场提示——
	// 这类文件 mtime 恒变（系统自己写日志），已豁免新鲜度校验，让模型知道
	// "列出日志文件不会导致 spec stale"，避免误以为必须剔除或反复重写。
	var growing []string
	for _, p := range files {
		if IsGrowingPath(p) {
			growing = append(growing, p)
		}
	}
	if len(growing) > 0 {
		out += "。提示: files 含持续增长文件 [" + strings.Join(growing, ", ") + "]（logs/.bma 目录），其 mtime 已豁免校验、不会导致 spec 失效"
	}
	return &Result{
		Tool:    "WriteSpec",
		Success: true,
		Output:  out,
	}
}

// writeSpecInput 是 WriteSpec 工具的入参结构，通过 json tag 暴露给 blades schema 生成器。
type writeSpecInput struct {
	// Goal 一句话任务目标，子 Agent 据此理解做什么。
	Goal string `json:"goal" description:"任务目标，一句话。子 Agent 据此理解做什么。"`
	// Acceptance 验收条件列表（可执行/可验证），子 Agent 据此判断任务完成。
	Acceptance []string `json:"acceptance" description:"验收条件列表（可执行/可验证）。至少一条，子 Agent 据此判断任务是否完成。"`
	// Constraints 约束/边界（不碰哪些、性能要求、兼容性、风格等），可空。
	Constraints []string `json:"constraints" description:"约束/边界（不碰哪些、性能要求、兼容性、风格等）。可空。"`
	// Files 涉及的文件路径列表，用于自动失效与版本校验。任一文件被 WriteFile 修改后该 spec 自动失效。
	Files []string `json:"files" description:"涉及的文件路径列表（相对或绝对）。任一文件被 WriteFile 修改后该规范自动失效，避免子 Agent 读到旧规范。可空。"`
}

// parseStringListArg 从 args[key] 提取字符串列表，兼容 []any / []string / 缺省。
// 与 parseFilesArg 同结构，独立保留以提升可读性。
func parseStringListArg(v any) []string {
	switch vv := v.(type) {
	case []string:
		out := make([]string, 0, len(vv))
		for _, s := range vv {
			if t := strings.TrimSpace(s); t != "" {
				out = append(out, t)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, p := range vv {
			if s, ok := p.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	default:
		return nil
	}
}

// truncateRunesForLog 按 rune 数截断字符串并追加省略号，用于日志输出。
// 与 dispatcher.truncateRunes 同语义，独立保留避免跨包依赖。
func truncateRunesForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
