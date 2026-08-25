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
	"encoding/json"
	"fmt"
	"strings"
)

// SpecSlot 是 WriteSpec 写入的默认 slot 名。
// 存储键为 "<agentID>:spec"（key 为空时）或 "<agentID>:spec:<key>"（TODO #65 多 key 化，
// key 通常为领域名：兄弟各持各的 spec，staleness 按各自 files 交集隔离），
// 与 injectKVMemory 枚举的 "<agentID>:<key>" 命名一致。
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
	// VerifyLevels 验收层级（TODO #59 分层验收模板）：existence/static/integration/
	// runtime/visual 子集。存在性与静态层由 dispatcher 冒烟检查自动覆盖；
	// integration 触发入口引用图探针、visual 强制截图回显证据（缺则落 delivered-unverified）。
	// 可空=不强制额外层级（默认存在性+静态）。
	VerifyLevels []string `json:"verify_levels,omitempty"`
	// Contract 跨域引用协议（TODO #57）：机器可校验的集成点清单，可空。
	// dispatcher 在兄弟域全完成后跑静态契约检查器逐条核对。
	Contract *Contract `json:"contract,omitempty"`
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
		"verify_levels 字段填验收层级（existence/static/integration/runtime/visual 子集）：" +
		"UI/游戏类任务填 visual（强制截图回显证据），多文件集成填 integration（入口引用图探针）。" +
		"例外：logs/ 与 .bma/ 目录下的文件是系统持续写入的活体文件（日志等），豁免 mtime 校验、不会导致失效。" +
		"\n覆盖语义：同一 parent 的同一 key 写入覆盖前一次内容（不追加）。" +
		"key 可空（默认 spec，全兄弟共享一份）；多领域任务建议按领域名各写一份（key=领域名），" +
		"兄弟各持各的、staleness 按各自 files 交集隔离，互不误伤。"
}

// Execute 写入任务规范。
// 入参 goal/acceptance/constraints/files/verify_levels/contract 结构化组装为 Spec，
// encodeSpecMD 编码为 MD 存入共享记忆，键为 "<agentID>:spec" 或 "<agentID>:spec:<key>"。
// goal 与 acceptance 至少一项非空才视为合法规范。
func (t *writeSpecTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.store == nil {
		return &Result{Tool: "WriteSpec", Error: "shared memory store not configured"}
	}
	goal, _ := args["goal"].(string)
	goal = strings.TrimSpace(goal)
	acceptance := parseStringListArg(args["acceptance"])
	constraints := parseStringListArg(args["constraints"])
	files := parseFilesArg(args["files"])
	contract := parseContractArg(args["contract"])
	verifyLevels, err := NormalizeVerifyLevels(parseStringListArg(args["verify_levels"]))
	if err != nil {
		return &Result{Tool: "WriteSpec", Error: err.Error(), Category: ResultCategoryValidationRejected}
	}

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

	// 防御纵深（TODO #64）：goal/acceptance/constraints 含降级/兜底关键词时自动追加
	// happy-path 主路径可用性验收项——防"贴图优先、手绘兜底"式防御设计被验收只查文件
	// 存在而放行，兜底静默变成唯一路径（实证 2026-08-24 塔防 23 张贴图全走手绘）。
	appendedHappyPath := false
	if !hasPrefixAcceptance(acceptance, "【自动追加·happy-path】") && HasFallbackKeyword(goal, strings.Join(acceptance, " "), strings.Join(constraints, " ")) {
		acceptance = append(acceptance, HappyPathAcceptance)
		appendedHappyPath = true
	}
	// 命名槽位 key（TODO #65 多 key 化）：可空（默认共享单份）；填领域名时
	// 兄弟各持各的 spec，staleness 按各自 files 交集隔离。规范化同 WriteSharedMemory。
	slot, _ := args["key"].(string)
	slot = strings.TrimSpace(slot)
	if slot == "" {
		slot = SpecSlot
	}
	if strings.ContainsAny(slot, ": \t\n\r") {
		return &Result{Tool: "WriteSpec", Error: "key must not contain ':', whitespace", Category: ResultCategoryValidationRejected}
	}

	spec := Spec{
		Goal:         goal,
		Acceptance:   acceptance,
		Constraints:  constraints,
		Files:        files,
		VerifyLevels: verifyLevels,
		Contract:     contract,
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

	key := agentID + ":" + slot
	if slot != SpecSlot {
		key = agentID + ":" + SpecSlot + ":" + slot
	}
	if err := t.store.Set(ctx, key, md); err != nil {
		return &Result{Tool: "WriteSpec", Error: fmt.Sprintf("set: %v", err)}
	}
	out := fmt.Sprintf("spec written (key=%s, goal=%q, %d acceptance, %d constraints, %d files tracked)", key, truncateRunesForLog(goal, 60), len(acceptance), len(constraints), len(filesMtime))
	if appendedHappyPath {
		out += "。检测到降级/兜底关键词，已自动追加 happy-path 主路径可用性验收项（见 acceptance 末条）——主路径必须真实生效，兜底不得成为唯一实现路径"
	}
	// 派发预算提醒（TODO #38-3）：把合规时机前移一轮——写规范时即提示 task 预算，
	// 避免下轮 call_sub_agent 因 task 超长被拒（3000 软限放行附警告，4000 硬拒）。
	out += "。派发 task 预算 3000 字（按 rune 计，超 4000 硬拒）：规格细节走本工具，task 只写目标+验收标准"
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
	// VerifyLevels 验收层级（TODO #59）：existence/static/integration/runtime/visual 子集。
	// UI/游戏类任务填 visual；多文件集成填 integration；默认存在性+静态由 dispatcher 自动覆盖。
	VerifyLevels []string `json:"verify_levels" description:"验收层级（existence/static/integration/runtime/visual 子集，可空）。UI/游戏/绘制类任务必须含 visual（dispatcher 强制截图回显证据，缺则判未验证）；多文件集成类任务含 integration（dispatcher 跑入口引用图探针）。"`
	// Key 命名槽位（TODO #65 多 key 化）：可空（默认 spec，全兄弟共享一份）。
	// 多领域任务建议按领域名各写一份（key=领域名），兄弟各持各的、staleness 互不误伤。
	Key string `json:"key" description:"命名槽位（默认 spec）。多领域任务按领域名各写一份（key=领域名）可隔离 staleness；同 key 写入覆盖前值。可空。"`
	// Contract 跨域契约（TODO #57）：多域任务必须填写机器可校验的集成点清单。
	// dispatcher 在全部兄弟域完成后自动跑静态契约检查，违例按文件归属打回责任域。
	// 四类条目均可空；单域/无跨域引用任务整个 contract 可空。
	Contract *Contract `json:"contract" description:"跨域契约（多域任务填写）：symbols=跨域符号映射（symbol 声明于 file，refs 列引用方文件）；dom_ids=DOM 元素 id 清单（id 声明于 file）；scripts=script 加载顺序（条目顺序即加载顺序）；signatures=跨域函数签名（signature 文本必须出现在 file 中）。dispatcher 机器校验用，零 LLM；单域任务可空。"`
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

// parseContractArg 从 args[key] 提取 Contract：兼容 *Contract / map[string]any（JSON 往返）/
// 缺省。LLM 经 blades schema 传入 map，JSON 序列化往返是结构映射的最省路径。
func parseContractArg(v any) *Contract {
	switch vv := v.(type) {
	case *Contract:
		return vv
	case Contract:
		return &vv
	case map[string]any:
		data, err := json.Marshal(vv)
		if err != nil {
			return nil
		}
		var c Contract
		if err := json.Unmarshal(data, &c); err != nil {
			return nil
		}
		if c.Empty() {
			return nil
		}
		return &c
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

// VerifyLevelsAllowed 是验收分层（TODO #59）的合法层级集合：
//   - existence 存在性：文件在不在（dispatcher 冒烟检查自动覆盖）；
//   - static 静态：语法/格式检查（node --check/tsc --noEmit/gofmt，冒烟层自动覆盖）；
//   - integration 集成：入口引用图探针（script src/import 解析到存在的文件，防"空壳产物"）；
//   - runtime 运行时：探针脚本实跑断言（验收标准自带可执行断言，agent 自执行）；
//   - visual 视觉：截图回显证据（dispatcher 强制 browser_take_screenshot 成功证据，缺则未验证）。
var VerifyLevelsAllowed = []string{"existence", "static", "integration", "runtime", "visual"}

// NormalizeVerifyLevels 规范化验收层级列表：trim + 小写 + 去重 + 校验合法值。
// 非法值返回错误并列出合法集合，让 LLM 一次改正（严格优于静默丢弃）。
func NormalizeVerifyLevels(levels []string) ([]string, error) {
	if len(levels) == 0 {
		return nil, nil
	}
	allowed := make(map[string]bool, len(VerifyLevelsAllowed))
	for _, v := range VerifyLevelsAllowed {
		allowed[v] = true
	}
	seen := make(map[string]bool, len(levels))
	var out []string
	for _, l := range levels {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		if !allowed[l] {
			return nil, fmt.Errorf("verify_levels 含非法值 %q，合法集合: %s", l, strings.Join(VerifyLevelsAllowed, "/"))
		}
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out, nil
}

// hasPrefixAcceptance 判断 acceptance 列表是否已有同前缀条目（防重复追加）。
func hasPrefixAcceptance(acceptance []string, prefix string) bool {
	for _, a := range acceptance {
		if strings.HasPrefix(strings.TrimSpace(a), prefix) {
			return true
		}
	}
	return false
}

// fallbackKeywords 触发 happy-path 自动追加（TODO #64）：acceptance/constraints/goal
// 出现降级/兜底语义时，dispatcher 追加主路径可用性验收项，防"兜底变成唯一路径"
// （实证 2026-08-24 塔防：23 张 AI 贴图一张未用、全走 canvas 手绘兜底）。
var fallbackKeywords = []string{"降级", "fallback", "兜底", "degraded", "备用路径"}

// HasFallbackKeyword 判断文本是否含降级/兜底关键词（大小写不敏感，英文子串匹配）。
// 供 WriteSpec 自动追加 happy-path 验收项判定，与 roles.yaml 纪律文案口径一致。
func HasFallbackKeyword(texts ...string) bool {
	for _, t := range texts {
		tl := strings.ToLower(t)
		for _, k := range fallbackKeywords {
			if strings.Contains(tl, k) {
				return true
			}
		}
	}
	return false
}

// HappyPathAcceptance 是 fallback 场景自动追加的主路径可用性验收项（TODO #64）。
// 追加到 acceptance 尾部，并在工具输出中显式提示（模型可见、可据此细化具体断言）。
const HappyPathAcceptance = "【自动追加·happy-path】主路径必须真实可用并被验证：降级/兜底仅作异常保护，" +
	"不得成为唯一实现路径。终答前提供主路径运行的客观证据（如资源加载断言 getImage('x')!==null、主入口可被实例化）。"
