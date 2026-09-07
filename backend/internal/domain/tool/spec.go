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
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SpecSlot 是 WriteSpec 写入的默认 slot 名。
// 存储键为 "<agentID>:spec"（key 为空时）或 "<agentID>:spec:<key>"（TODO #65 多 key 化，
// key 通常为领域名：兄弟各持各的 spec，staleness 按各自 files 交集隔离），
// 与 injectKVMemory 枚举的 "<agentID>:<key>" 命名一致。
const SpecSlot = "spec"

// AcceptanceItem 是结构化验收条目（TODO #68 证据绑定 / #75 分层）。
// 兼容存量纯字符串（= manual + functional）。Text 为条目文本；
// Evidence 声明证据类型（dispatcher 据此挂接机器校验）；Layer 声明功能/品质分层。
type AcceptanceItem struct {
	// Text 验收条件文本（自然语言，可执行/可验证）。
	Text string `json:"text" yaml:"text"`
	// Evidence 证据类型（TODO #68）：command（验证类命令证据）/ screenshot（截图证据）/
	// probe（运行时探针证据）/ file（文件存在性）/ manual（纯纸面标准，机器不核）。
	// 空串按 manual 处理。
	Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	// Layer 分层（TODO #75）：functional（能跑）/ quality（像不像/好不好用）。
	// quality 层条目未过不允许整体标绿。空串按 functional。
	Layer string `json:"layer,omitempty" yaml:"layer,omitempty"`
}

// AcceptanceEvidenceAllowed 是合法证据类型集合（TODO #68）。
var AcceptanceEvidenceAllowed = []string{"command", "screenshot", "probe", "file", "manual"}

// AcceptanceLayerAllowed 是合法分层集合（TODO #75）。
var AcceptanceLayerAllowed = []string{"functional", "quality"}

// NormalizeAcceptanceEvidence 规范化证据类型；空串归 manual，非法值报错。
func NormalizeAcceptanceEvidence(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return "manual", nil
	}
	if slices.Contains(AcceptanceEvidenceAllowed, e) {
		return e, nil
	}
	return "", fmt.Errorf("acceptance 证据类型 %q 非法，合法集合: %s", e, strings.Join(AcceptanceEvidenceAllowed, "/"))
}

// NormalizeAcceptanceLayer 规范化分层；空串归 functional，非法值报错。
func NormalizeAcceptanceLayer(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" {
		return "functional", nil
	}
	if slices.Contains(AcceptanceLayerAllowed, l) {
		return l, nil
	}
	return "", fmt.Errorf("acceptance 分层 %q 非法，合法集合: %s", l, strings.Join(AcceptanceLayerAllowed, "/"))
}

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
	// Probes 运行时探针声明（TODO #67 runtime 层）：探针操作序列的散文描述
	// （navigate → 交互 → 断言 → console 无 error）。runtime 层的证据扫描按
	// browser_navigate/browser_evaluate/browser_console_messages 机器强制，
	// 本字段供子 Agent 知道探针要做什么（人读提示），dispatcher 不解析其内容。
	Probes []string `json:"probes,omitempty"`
	// Scenes 场景化截图清单（TODO #69 visual 层）：UI/游戏类任务须覆盖的场景名列表
	// （如 主菜单/游玩中/结算页）。dispatcher 证据扫描按截图内容哈希去重后核对
	// 数量 ≥ len(scenes) 且有 navigate/evaluate 邻接证据；空=退回单截图判定。
	Scenes []string `json:"scenes,omitempty"`
	// Baseline 对标基线产物清单（TODO #75）：还原/仿制/复刻类任务必须填写的参照物
	// 文件路径列表（参考截图/数值表等，落 workspace 可核）。命中还原关键词且为空时
	// WriteSpec 拒收。dispatcher 只校验引用文件存在性，不做内容理解。
	Baseline []string `json:"baseline,omitempty"`
}

// writeSpecTool 是 WriteSpec 工具的封装。
type writeSpecTool struct {
	store SharedMemoryStore
	// workDir 为默认工作目录（来自 FileSharedMemoryStore.WorkDir），baseline_content
	// 内联落盘写到 <workDir>/.bma/baseline/ 下；为空时 baseline_content 报错提示不可用。
	// Execute 时若 store 提供 WorkDirOf(ctx) 则按 ctx 会话目录覆盖本默认值（S2）。
	workDir string
}

// Name 返回工具标准名称 WriteSpec。
func (t *writeSpecTool) Name() string { return "WriteSpec" }

// Aliases 返回 WriteSpec 的别名列表。
func (t *writeSpecTool) Aliases() []string {
	return []string{"write_spec", "writeSpec"}
}

// Description 返回工具的人类可读描述，供 schema 与 UI 展示。
func (t *writeSpecTool) Description() string {
	return "派发子 Agent 前写入结构化任务规范（goal/acceptance/constraints/files），" +
		"dispatcher 强制 call_sub_agent 前先调本工具，规范注入子 Agent 任务体前缀。" +
		"files 记录 mtime，任一文件被改后规范自动失效（stale 拒派）；logs/ 与 .bma/ 活体文件豁免。" +
		"verify_levels 填验收层级子集（UI/游戏类 visual+runtime，多文件集成 integration）；" +
		"acceptance 支持 {text,evidence,layer} 结构化条目（缺证据标黄不计分，quality 未过不标绿）。" +
		"还原/复刻类必须填 baseline 或 baseline_content（内联基线自动落盘 .bma/baseline/），否则拒收。" +
		"覆盖语义：同 key 写入覆盖不追加；key 可空（默认单键共享），多领域任务 key=领域名" +
		"（须与 call_sub_agent 的 domain 一致）。"
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
	acceptance, accErr := parseAcceptanceArg(args["acceptance"])
	if accErr != "" {
		return &Result{Tool: "WriteSpec", Error: accErr, Category: ResultCategoryValidationRejected}
	}
	constraints := parseStringListArg(args["constraints"])
	files := parseFilesArg(args["files"])
	contract := parseContractArg(args["contract"])
	verifyLevels, err := NormalizeVerifyLevels(parseStringListArg(args["verify_levels"]))
	if err != nil {
		return &Result{Tool: "WriteSpec", Error: err.Error(), Category: ResultCategoryValidationRejected}
	}
	probes := parseStringListArg(args["probes"])
	scenes := parseStringListArg(args["scenes"])
	baseline := parseFilesArg(args["baseline"])
	baselineContent := parseBaselineContentArg(args["baseline_content"])

	if goal == "" {
		return &Result{Tool: "WriteSpec", Error: "goal is required", Category: ResultCategoryValidationRejected}
	}
	if len(acceptance) == 0 {
		return &Result{Tool: "WriteSpec", Error: "acceptance is required (at least one verifiable condition)", Category: ResultCategoryValidationRejected}
	}
	// 契约内容校验（TODO #70）：signature/symbol 拒绝散文形态（全角字符、∈、中文注解段）
	// ——写时拦截优于运行时假违例（实证 2026-08-25：含中文注解签名对真实代码永不命中，
	// 同一假违例 3 次推送）。非法直接报错回派发方。
	if contract != nil {
		if msg := ValidateContractShape(contract); msg != "" {
			return &Result{Tool: "WriteSpec", Error: msg, Category: ResultCategoryValidationRejected}
		}
	}
	// baseline_content 内联落盘（防死锁，实证 2026-08-25 水果忍者：MetaAgent 工具表
	// 无宿主写文件工具，被迫挂 computer_use 插件的 filesystem（跑在沙箱容器内、相对路径
	// 从容器 /root/Desktop 解析），基线写进容器后宿主 fileExists 永远不见，5 连拒被
	// loop guard 强杀）。调研结论本就在 MetaAgent 上下文里，直接内联落盘一步直达。
	// 落盘目录强制 <workDir>/.bma/baseline/（path 仅取文件名部分，防路径逃逸），
	// 落盘后追加进 baseline 清单参与后续存在性校验。
	// workDir 按 ctx 会话目录解析（S2）：store 为文件后端时经 WorkDirOf(ctx)
	// 取 ctx 生效目录（未注入回落 store 构造目录），非文件后端回落 t.workDir（空串）。
	workDir := t.workDir
	if w, ok := t.store.(interface{ WorkDirOf(context.Context) string }); ok {
		workDir = w.WorkDirOf(ctx)
	}
	if len(baselineContent) > 0 {
		if workDir == "" {
			return &Result{Tool: "WriteSpec", Error: "baseline_content 需要文件后端共享记忆存储（当前 store 未提供工作目录），请改用 baseline 字段引用已落盘文件", Category: ResultCategoryValidationRejected}
		}
		for _, bc := range baselineContent {
			if strings.TrimSpace(bc.Content) == "" {
				return &Result{Tool: "WriteSpec", Error: "baseline_content 项 " + bc.Path + " 的 content 为空", Category: ResultCategoryValidationRejected}
			}
		}
		rel, err := writeBaselineFiles(workDir, baselineContent)
		if err != nil {
			return &Result{Tool: "WriteSpec", Error: err.Error(), Category: ResultCategoryValidationRejected}
		}
		baseline = append(baseline, rel...)
	}
	// 对标基线强制（TODO #75）：goal/acceptance 命中还原类关键词且 baseline 空 → 拒收。
	// "还原度不可验收 = 需求未定义完"，先准备基线（用户供图/网络下载/先派分析任务）。
	// baseline_content 计入非空判定（其随后落盘并入 baseline 清单）。
	if len(baseline) == 0 && len(baselineContent) == 0 && HasFidelityKeyword(goal, strings.Join(acceptance, " ")) {
		return &Result{Tool: "WriteSpec", Error: "goal/acceptance 命中还原/复刻/对标类诉求但 baseline 为空：还原度不可验收 = 需求未定义完。请先准备对标基线（参考截图/数值表/帧分析文件路径，落 workspace 可核），写入 baseline 字段；或把基线全文内联进 baseline_content=[{path,content}]（本工具自动落盘 .bma/baseline/）", Category: ResultCategoryValidationRejected}
	}
	// baseline 引用文件存在性校验（TODO #75）：基线是验收依据，路径不存在=对照无从谈起。
	// 错误自描述（防死亡螺旋，实证 2026-08-25：MetaAgent 用沙箱容器内的 filesystem
	// 工具落盘基线，宿主不可见，盲试 4 种路径写法全拒）：附解析后的绝对路径与校验
	// 基准目录，并指引 baseline_content 内联落盘——一次重试可解，不进 loop guard。
	// 相对路径按 workDir 解析（与 baseline_content 落盘目录同源；workDir 空回退进程 cwd）。
	if len(baseline) > 0 {
		var missing []string
		for _, p := range baseline {
			rp := strings.TrimSpace(p)
			if workDir != "" && !filepath.IsAbs(rp) {
				rp = filepath.Join(workDir, rp)
			}
			if _, err := os.Stat(rp); err != nil {
				missing = append(missing, p+"（解析为 "+rp+"）")
			}
		}
		if len(missing) > 0 {
			return &Result{Tool: "WriteSpec", Error: "baseline 引用文件不存在: " + strings.Join(missing, ", ") +
				"。校验以宿主工作目录为基准（插件 filesystem/scrape 等工具写的是沙箱容器文件系统，宿主不可见，换路径写法无用）。" +
				"调研结论在上下文里时直接用 baseline_content=[{path,content}] 内联落盘（本工具自动写入 .bma/baseline/），一步到位", Category: ResultCategoryValidationRejected}
		}
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
		Probes:       probes,
		Scenes:       scenes,
		Baseline:     baseline,
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
	if len(probes) > 0 {
		out += fmt.Sprintf("。runtime 探针 %d 条（dispatcher 机器强制：browser_navigate+browser_evaluate+console 回读无 error 缺任一判未验证）", len(probes))
	}
	if len(scenes) > 0 {
		out += fmt.Sprintf("。visual 场景清单 %d 个（截图按内容去重后须覆盖全部场景）", len(scenes))
	}
	if len(baseline) > 0 {
		out += fmt.Sprintf("。对标基线 %d 项（品质层验收对照依据）", len(baseline))
	}
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
	// 双形态（TODO #68）：字符串（纯纸面标准）或 {text, evidence, layer} 对象——
	// evidence 声明证据类型（command/screenshot/probe/file/manual），dispatcher 逐项挂机器证据、
	// 缺证据条目不计入通过数；layer 声明 functional/quality 分层，quality 未过不整体标绿。
	Acceptance []any `json:"acceptance" description:"验收条件列表。纯字符串=纸面标准；结构化对象 {text, evidence, layer} 推荐：evidence=command|screenshot|probe|file|manual（dispatcher 按类型挂机器证据，缺证据条目标黄不计分），layer=functional|quality（quality 层未过不允许整体标绿）。"`
	// Constraints 约束/边界（不碰哪些、性能要求、兼容性、风格等），可空。
	Constraints []string `json:"constraints" description:"约束/边界（不碰哪些、性能要求、兼容性、风格等）。可空。"`
	// Files 涉及的文件路径列表，用于自动失效与版本校验。任一文件被 WriteFile 修改后该 spec 自动失效。
	Files []string `json:"files" description:"涉及的文件路径列表（相对或绝对）。任一文件被 WriteFile 修改后该规范自动失效，避免子 Agent 读到旧规范。可空。"`
	// VerifyLevels 验收层级（TODO #59）：existence/static/integration/runtime/visual 子集。
	// UI/游戏类任务填 visual；多文件集成填 integration；默认存在性+静态由 dispatcher 自动覆盖。
	VerifyLevels []string `json:"verify_levels" description:"验收层级（existence/static/integration/runtime/visual 子集，可空）。UI/游戏/绘制类任务必须含 visual（dispatcher 强制截图回显证据，缺则判未验证）与 runtime（dispatcher 机器强制 browser_navigate+browser_evaluate+console 无 error 探针证据，缺则判未验证）；多文件集成类任务含 integration（dispatcher 跑入口引用图探针）。"`
	// Key 命名槽位（TODO #65 多 key 化）：可空（默认 spec，全兄弟共享一份）。
	// 多领域任务建议按领域名各写一份（key=领域名），兄弟各持各的、staleness 互不误伤。
	Key string `json:"key" description:"命名槽位（默认 spec）。多领域任务按领域名各写一份（key=领域名）可隔离 staleness；同 key 写入覆盖前值。key 必须与 call_sub_agent 的 domain 参数一致（dispatcher 按 domain 查键）。可空。"`
	// Contract 跨域契约（TODO #57）：多域任务必须填写机器可校验的集成点清单。
	// dispatcher 在全部兄弟域完成后自动跑静态契约检查，违例按文件归属打回责任域。
	// 四类条目均可空；单域/无跨域引用任务整个 contract 可空。
	Contract *Contract `json:"contract" description:"跨域契约（多域任务填写）：symbols=跨域符号映射（symbol 声明于 file，refs 列引用方文件）；dom_ids=DOM 元素 id 清单；scripts=script 加载顺序；signatures=跨域函数签名（signature 文本必须出现在 file 中，只写代码文本、禁全角标点/中文注解）。dispatcher 机器校验用，零 LLM；单域任务可空。"`
	// Probes 运行时探针声明（TODO #67）：runtime 层验收须声明的探针操作序列描述。
	Probes []string `json:"probes" description:"运行时探针序列声明（verify_levels 含 runtime 时填写）：每条一个操作步骤描述（如 打开页面/点击开始按钮/断言实体生成/console 无 error）。dispatcher 机器强制探针证据（browser_navigate+browser_evaluate+browser_console_messages 回读无 error），缺任一判未验证。"`
	// Scenes 场景化截图清单（TODO #69）：visual 层须覆盖的场景名列表。
	Scenes []string `json:"scenes" description:"场景化截图清单（verify_levels 含 visual 时填写）：UI/游戏类任务须覆盖的场景名列表（如 主菜单/游玩中/切割瞬间/结算页）。dispatcher 按截图内容哈希去重后核对覆盖数，同图连拍充数无效。"`
	// Baseline 对标基线产物清单（TODO #75）：还原/复刻/对标类任务必填。
	Baseline []string `json:"baseline" description:"对标基线产物清单（还原/复刻/仿制/对标类任务必填）：参照物文件路径列表（参考截图/数值表/帧分析文件，须已落盘可核）。goal 命中还原类诉求且为空时拒收——还原度不可验收=需求未定义完。"`
	// BaselineContent 内联基线内容（TODO 防死锁）：基线尚未落盘时直接内联全文，
	// 本工具自动写入 <workDir>/.bma/baseline/<path> 再纳入 baseline 校验。
	// 用于 MetaAgent 无宿主写文件工具的场景（插件 filesystem 写入沙箱容器宿主不可见）。
	BaselineContent []BaselineContentItem `json:"baseline_content" description:"内联基线内容清单（基线尚未落盘时用）：[{path, content}]，path 仅取文件名部分（强制写入 .bma/baseline/ 目录），content 为基线全文。本工具先落盘再校验存在性——调研结论直接内联，不要用插件 filesystem 工具落盘（其写沙箱容器文件系统，宿主不可见）。与 baseline 字段可同时使用：baseline 引已落盘文件，baseline_content 内联新内容。"`
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

// BaselineContentItem 是 baseline_content 的内联基线条目：Path 为文件名（仅取
// path.Base 部分，目录强制 .bma/baseline/，防路径逃逸），Content 为基线全文。
type BaselineContentItem struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// parseBaselineContentArg 从 args["baseline_content"] 提取内联基线条目。
// 兼容 []any(map) / 缺省；path 为空或非字符串项跳过（由后续校验报错）。
func parseBaselineContentArg(v any) []BaselineContentItem {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]BaselineContentItem, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		bc := BaselineContentItem{}
		bc.Path, _ = m["path"].(string)
		bc.Content, _ = m["content"].(string)
		out = append(out, bc)
	}
	return out
}

// writeBaselineFiles 把内联基线内容写到 <workDir>/.bma/baseline/<name>，
// 返回写入的相对路径清单（供追加进 baseline 字段）。path 只取文件名部分，
// 同名覆盖（重写 spec 幂等）。
func writeBaselineFiles(workDir string, items []BaselineContentItem) ([]string, error) {
	root := filepath.Join(workDir, ".bma", "baseline")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建基线目录失败: %w", err)
	}
	var rel []string
	for _, bc := range items {
		name := strings.TrimSpace(bc.Path)
		if name == "" {
			return nil, fmt.Errorf("baseline_content 项 path 为空")
		}
		name = filepath.Base(filepath.FromSlash(name))
		if name == "." || name == ".." || name == string(filepath.Separator) {
			return nil, fmt.Errorf("baseline_content 项 path %q 不是合法文件名", bc.Path)
		}
		dst := filepath.Join(root, name)
		if err := os.WriteFile(dst, []byte(bc.Content), 0o644); err != nil {
			return nil, fmt.Errorf("基线落盘失败 %s: %w", dst, err)
		}
		rel = append(rel, filepath.ToSlash(filepath.Join(".bma", "baseline", name)))
	}
	return rel, nil
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

// parseAcceptanceArg 从 args["acceptance"] 提取验收条目（TODO #68 双形态）：
//   - []string / []any(string)：存量纯字符串形态，原样透传（渲染前缀时按无结构化处理）；
//   - []any(map)：结构化条目 {"text":..., "evidence":..., "layer":...}，
//     渲染为带标记的行 `文本 [evidence=X layer=Y]`，dispatcher 证据扫描按标记解析；
//   - 混合数组：字符串项透传、对象项结构化。
//
// 校验 evidence/layer 合法值，非法返回错误文案（非空串）。
// 结构化信息编码进字符串行而非改 []string 类型，是为了保持 Spec.Acceptance 与
// frontmatter/既有消费方（renderSpecPrefix/checkSpecKey/hasFreshSpec）零改动兼容；
// dispatcher 侧 ParseAcceptanceLine 对称解析。
func parseAcceptanceArg(v any) ([]string, string) {
	switch vv := v.(type) {
	case []string:
		return parseStringListArg(vv), ""
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			switch it := item.(type) {
			case string:
				if t := strings.TrimSpace(it); t != "" {
					out = append(out, t)
				}
			case map[string]any:
				text, _ := it["text"].(string)
				text = strings.TrimSpace(text)
				if text == "" {
					return nil, "acceptance 结构化条目缺 text 字段（{text, evidence?, layer?}）"
				}
				ev, _ := it["evidence"].(string)
				ev, err := NormalizeAcceptanceEvidence(ev)
				if err != nil {
					return nil, err.Error()
				}
				layer, _ := it["layer"].(string)
				layer, err = NormalizeAcceptanceLayer(layer)
				if err != nil {
					return nil, err.Error()
				}
				out = append(out, FormatAcceptanceLine(text, ev, layer))
			default:
				return nil, "acceptance 项必须是字符串或 {text, evidence?, layer?} 对象"
			}
		}
		return out, ""
	default:
		return nil, ""
	}
}

// AcceptanceLineTagPrefix 是结构化验收条目行内标记的起始分隔符（TODO #68）。
// 标记形如 ` [evidence:command layer:functional]` 追加在条目文本尾部；
// 解析侧 ParseAcceptanceLine 按此前缀切分文本与标记。
const AcceptanceLineTagPrefix = " [evidence:"

// FormatAcceptanceLine 把结构化验收条目渲染为带标记的单行字符串。
func FormatAcceptanceLine(text, evidence, layer string) string {
	if layer == "" || layer == "functional" {
		return text + " [evidence:" + evidence + "]"
	}
	return text + " [evidence:" + evidence + " layer:" + layer + "]"
}

// ParseAcceptanceLine 解析带标记的验收条目行（FormatAcceptanceLine 的逆操作，
// dispatcher 侧计分用）。无标记行返回 evidence=""（按 manual 处理）。
// 返回 (文本, evidence, layer)。
func ParseAcceptanceLine(line string) (text, evidence, layer string) {
	idx := strings.LastIndex(line, AcceptanceLineTagPrefix)
	if idx < 0 {
		return strings.TrimSpace(line), "", ""
	}
	text = strings.TrimSpace(line[:idx])
	tag := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[idx+len(AcceptanceLineTagPrefix):]), "]"))
	// tag 形如 "command" 或 "command layer:quality"
	if sp := strings.IndexByte(tag, ' '); sp >= 0 {
		evidence = tag[:sp]
		rest := strings.TrimSpace(tag[sp+1:])
		rest = strings.TrimPrefix(rest, "layer:")
		layer = strings.TrimSpace(rest)
	} else {
		evidence = tag
	}
	return text, strings.TrimSpace(evidence), strings.TrimSpace(layer)
}

// fidelityKeywords 触发 baseline 强制（TODO #75）：还原/仿制/复刻/对标类诉求。
var fidelityKeywords = []string{"还原", "复刻", "仿制", "高保真", "对标", "还原度", "fidelity", "replica", "clone of", "recreate"}

// HasFidelityKeyword 判断文本是否含还原类关键词（大小写不敏感）。
func HasFidelityKeyword(texts ...string) bool {
	for _, t := range texts {
		tl := strings.ToLower(t)
		for _, k := range fidelityKeywords {
			if strings.Contains(tl, k) {
				return true
			}
		}
	}
	return false
}

// contractShapeDisallowed 是契约 signature/symbol 字段禁止出现的"散文形态"字符与
// 模式（TODO #70 写时拦截）：全角标点、数学符号 ∈、中文注解段（；mode ∈ 'classic' 这类）
// 对真实代码永不命中，只会产出假违例反复打回。
var contractShapeDisallowedRunes = map[rune]bool{
	'∈': true, '→': true, '←': true, '（': true, '）': true, '，': true, '；': true, '：': true,
	'【': true, '】': true, '「': true, '」': true, '『': true, '』': true,
}

// ValidateContractShape 校验契约字段的代码形态（TODO #70）：signature/symbol 含
// 全角标点、∈ 等数学符号或中文时拒绝——LLM 把散文注解（`{ start(mode), version }；mode ∈ ...`）
// 写进签名字段会产生对真实代码永不命中的假违例。返回空串=合法。
// 中文按"签名/符号里出现任何 CJK 字符即非法"的保守口径（合法代码标识符不含 CJK）。
func ValidateContractShape(c *Contract) string {
	check := func(field, val string) string {
		for _, r := range val {
			if contractShapeDisallowedRunes[r] {
				return fmt.Sprintf("contract %s %q 含全角标点/数学符号（%c），不是合法代码形态——请只写代码里实际存在的文本（签名写函数签名本身，说明放 spec.constraints）", field, truncateRunesForLog(val, 60), r)
			}
			if r >= 0x4E00 && r <= 0x9FFF {
				return fmt.Sprintf("contract %s %q 含中文（代码标识符不含 CJK），注解请放 spec.constraints，signature/symbol 只写代码文本", field, truncateRunesForLog(val, 60))
			}
		}
		return ""
	}
	for _, sg := range c.Signatures {
		if msg := check("signatures.signature", sg.Signature); msg != "" {
			return msg
		}
		if msg := check("signatures.symbol", sg.Symbol); msg != "" {
			return msg
		}
	}
	for _, sy := range c.Symbols {
		if msg := check("symbols.symbol", sy.Symbol); msg != "" {
			return msg
		}
	}
	return ""
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
