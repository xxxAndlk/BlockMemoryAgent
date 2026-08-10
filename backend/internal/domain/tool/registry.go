package tool

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于传递上下文与取消信号
	"fmt"           // fmt 用于格式化错误信息
	"log"           // log 用于记录拦截/失败等不影响主流程的可观测事件
	"path/filepath" // filepath 用于规范化文件路径
	"strings"       // strings 用于拼接已读文件列表
	"sync"          // sync 提供互斥锁保护并发状态

	"github.com/blockmemory/agent/backend/internal/config"  // config 包提供 Agent 阈值配置
	"github.com/blockmemory/agent/backend/internal/project" // project 包提供 DomainClassifier 接口
	"github.com/go-kratos/blades/tools"                     // blades tools 包提供对外暴露的工具定义
)

// maxConsecutiveFailures 定义单个工具连续失败的最大次数，
// 超过此次数将触发循环退出，避免无限重试。
const maxConsecutiveFailures = 3

// maxConsecutiveValidationRejections 定义单个工具连续校验拒绝的最大次数（TODO #32）。
// 校验拒绝是"修正参数即可"的前置条件问题，与执行失败分开计：阈值放宽到 5，
// 仍防"模型复读同一错误参数"的无效循环（编造参数硬闯校验）。
const maxConsecutiveValidationRejections = 5

// exploreBudget 是单个 Agent 任务内探索类工具（ReadFile/ListDir/SearchInFiles）调用次数上限。
// 防止 Agent 陷入探索循环不收敛：实证 domain-2 调 81 次探索工具 30m 超时未写完。
// 超过后这些工具返回错误，逼迫 Agent 开始 WriteFile。HTTPGet 不计（联网查询场景不同）。
// RunCommand 仅"只读型命令"（cat/Get-Content/type/head/tail/more）计入：实证集成验证 Agent
// 用 RunCommand 逐文件 cat 绕过 ReadFile 预算，串行读文件 19 分钟不收敛（logs/tui/2026-08-03.log）。
// 验证/动作类 RunCommand（node --check、go build、mkdir）仍不计入：封禁会让 Agent 写完文件后
// 无法按纪律验证，在"必须验证"与"工具被拒"之间死循环（实证：配置 Agent 被拒 8 轮空转 4 分钟）。
// 取值 20：实证（2026-08-10 塔防日志）旧值 8 次 × 单页实测 60-110 行（4000 字符截停），
// 单领域职责文件（如 game.js 849 行 + monster.js 521 行）需 ~17 页才能读完，
// 4 个领域 Agent 全部耗尽预算被 loop guard 判死；20 次 × 200 行/页覆盖 ~4000 行。
const exploreBudget = 20

// exploreBudgetDomain 是 DomainAgent（协调者）写入未开始前的探索预算。
// domain 的职责是拆任务/派发/整合/验证，亲自探索是全舰队最贵路径（glm-5.2 thinking
// 单轮 3min+；2026-08-10 塔防日志 domain-2 亲自读 tower.js/bullet.js，60 分钟墙钟
// 超时被杀时 20 次预算远未耗尽——瓶颈是带大上下文的慢轮次，不是次数）。
// 8 次足够协调者用：查共享契约/file_tree + 1-2 次 SearchInFiles 定位 + 验收期精读失败点。
// 通读/多文件了解结构应连同实现一起下放叶子（叶子模型快、预算独立 20 次、失败隔离），
// 耗尽文案导向 call_sub_agent 而非 WriteFile（与叶子文案相反——叶子不能派发才逼它写）。
const exploreBudgetDomain = 8

// exploreBudgetPostWrite 是"写入已开始"后的探索预算升档上限。
// v13 基准实证：验收领域开工 57 秒读 8 个文件耗尽预算后被禁止再读，只能凭记忆整文件
// 盲重写（4 次 25-40K output tokens 巨型调用耗 29 分钟），盲改回归震荡致 13/16 平台期
// 30 分钟——修复期工作本质是"读报错位置→改→复验"循环，禁读等于逼盲改。
// 首次 WriteFile 成功后预算升档到本常量：写前 20 次反空转纪律不变，写后放开精读修复。
// 仍设上限防"逐文件通读"式发散（v2 实证 19 分钟不收敛），并有连读循环守卫与 token 预算兜底。
const exploreBudgetPostWrite = 40

// readLikeCmdPrefixes 是只读型 shell 命令前缀（小写匹配，覆盖 bash 与 PowerShell 两侧）。
// RunCommand 以这些前缀读文件时按探索工具计费；批量读取（如 cat a b c）只算 1 次，
// 借此把"逐文件多次读"逼成"单条命令批量读"。
var readLikeCmdPrefixes = []string{
	"cat ", "type ", "more ", "head ", "tail ",
	"get-content", "gc ", // PowerShell
}

// isReadLikeCommand 判断 RunCommand 参数是否为只读型文件查看命令。
func isReadLikeCommand(args map[string]any) bool {
	cmd, _ := args["command"].(string)
	c := strings.ToLower(strings.TrimSpace(cmd))
	for _, p := range readLikeCmdPrefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// maxConsecutiveSameRead 是同一 scope 内"参数完全相同的 ReadFile"连续调用次数上限。
// 重读不再被拦截（每次直返磁盘最新内容，天然无脏数据），但参数完全不变的连续重复
// 调用意味着模型未吸收内容、陷入死循环（实证：代码助手对 config.js 反复 ReadFile 10+ 次）：
// 第 2 次直返内容并附一句提醒，第 3 次触发 ActionLoopExit 终止 ReAct 循环兜底。
const maxConsecutiveSameRead = 3

// Tool 是内置工具的通用接口，所有具体工具都需要实现该接口。
type Tool interface {
	// Name 返回工具的标准名称，作为主键用于注册和调度。
	Name() string
	// Aliases 返回工具的别名列表，用于兼容不同的调用习惯。
	Aliases() []string
	// Execute 执行工具的核心方法，接收上下文和参数映射，返回执行结果。
	Execute(ctx context.Context, args map[string]any) *Result
}

// failureCounter 用于按工具名称统计连续失败次数，
// 支持并发安全地增加计数和重置计数。
// 两类计数（TODO #32）：counts=执行失败（execution_failed，阈值 3 终止）；
// validationCounts=校验拒绝（validation_rejected，阈值 5 终止）——分开计，互不共享。
type failureCounter struct {
	// mu 保护 counts 与 validationCounts 的读写锁，避免并发竞争。
	mu sync.Mutex
	// counts 记录每个工具名称对应的连续执行失败次数。
	counts map[string]int
	// validationCounts 记录每个工具名称对应的连续校验拒绝次数。
	validationCounts map[string]int
}

// newFailureCounter 创建一个新的失败计数器，内部 map 已经初始化。
func newFailureCounter() *failureCounter {
	return &failureCounter{
		counts:           make(map[string]int),
		validationCounts: make(map[string]int),
	}
}

// fail 将指定工具的连续失败次数加 1，并返回当前次数。
func (f *failureCounter) fail(name string) int {
	// 加锁保护 counts 的并发修改。
	f.mu.Lock()
	// 函数退出时释放锁，避免遗忘。
	defer f.mu.Unlock()
	// 对应工具计数加 1。
	f.counts[name]++
	// 返回增加后的次数，供调用方判断是否达到阈值。
	return f.counts[name]
}

// failValidation 将指定工具的连续校验拒绝次数加 1，并返回当前次数。
func (f *failureCounter) failValidation(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validationCounts[name]++
	return f.validationCounts[name]
}

// reset 将指定工具的连续失败次数清零（从 map 中删除）。
func (f *failureCounter) reset(name string) {
	// 加锁保护 counts 的并发修改。
	f.mu.Lock()
	// 函数退出时释放锁。
	defer f.mu.Unlock()
	// 删除该工具的计数记录，表示失败状态已恢复。
	delete(f.counts, name)
	delete(f.validationCounts, name)
}

// Registry 是工具注册表，保存所有内置工具、执行器、进度回调以及任务级状态。
type Registry struct {
	// exec 是实际负责工具执行的 Executor 实例。
	exec *Executor
	// progress 是进度事件回调函数，用于向外部报告工具调用和结果。
	progress ProgressCallback
	// failures 管理每个工具的连续失败计数。
	failures *failureCounter
	// tools 按标准名称存储已注册的工具实例。
	tools map[string]Tool
	// aliases 存储别名到标准名称的映射。
	aliases map[string]string
	// readMu 保护下列 per-scope 读取状态 map，防止并发读写。
	readMu sync.Mutex
	// lastReadKey 按 agentID 记录该 scope 最近一次 ReadFile 的调用键
	// （cleanPath+"\x00"+offset+"\x00"+limit），用于连续相同调用检测。
	// per-agent 维度隔离：兄弟 DomainAgent 同 session 各自独立，互不影响；
	// 新用户消息进入时 ResetReadHistory 按主 Agent agentID 清空。
	lastReadKey map[string]string
	// sameReadCount 按 agentID 记录"参数与上一次完全相同"的 ReadFile 连续次数。
	// 达 maxConsecutiveSameRead 触发 LoopExit（真死循环兜底）；
	// 参数有任何变化（翻页/换文件）即归零——重读本身合法，每次直返磁盘最新内容。
	sameReadCount map[string]int
	// exploreCount 按 scopeKey 记录本任务内探索类工具（ReadFile/ListDir）调用次数。
	// 防止 Agent 陷入探索循环不收敛：实证 domain-2 调 81 次探索工具 30m 超时未写完。
	// 超过 exploreBudget 后 ReadFile/ListDir 返回错误，逼迫 Agent 开始 WriteFile。
	// RunCommand 不计（验证/动作类），避免写完文件后无法验证陷入重试死循环。
	exploreCount map[string]int
	// writeCount 按 scopeKey 记录 WriteFile 成功次数：>0 后探索预算升档到
	// exploreBudgetPostWrite（修复期"读报错→改→复验"循环需要精读，见常量注释）。
	writeCount map[string]int
	// sharedMemory 是 WriteSharedMemory 工具的 KV 后端，由 bootstrap 注入。
	// 为 nil 时 WriteSharedMemory 注册但不生效，调用返回 store 未配置错误。
	sharedMemory SharedMemoryStore
	// refresher 去抖异步刷新 .bma/PROJECT.md：WriteFile/删改类 RunCommand 成功后 schedule，
	// 安静期触发 LLM 按职责重分区，使领域影响范围随文件增删改自动更新。
	refresher *projectRefresher
	// approvalHook 是破坏性操作的用户确认回调（TODO #17 P1）。nil（默认）= 全放行，
	// 零行为变化；非 nil 时仅对命中边界的调用触发（WriteFile 在生产目录 / 危险命令模式），
	// 常规编码流不阻塞。由 bootstrap 注入 ReactService.ApprovalHook。
	approvalHook ApprovalHookFunc
	// productionWorkDir 是配置的生产环境工作目录（绝对路径）；空 = 未启用生产边界确认，
	// 仅危险命令模式（isDangerousCommand）触发确认。
	productionWorkDir string
}

// NewBuiltinRegistry 创建一个已注册所有默认工具的 Registry 实例。
// workDir 是工作目录；cfg 是代理配置；progress 是进度回调。
func NewBuiltinRegistry(workDir string, cfg *config.AgentConfig, progress ProgressCallback) *Registry {
	// 使用指定工作目录创建执行器。
	exec := NewExecutor(workDir)
	// 如果传入配置，则设置到执行器中，供后续读取配置项。
	if cfg != nil {
		exec.SetAgentConfig(cfg)
	}
	// 初始化 Registry 结构体，各 map 也一并初始化。
	r := &Registry{
		exec:             exec,
		progress:         progress,
		failures:         newFailureCounter(),
		tools:            make(map[string]Tool),
		aliases:          make(map[string]string),
		lastReadKey:      make(map[string]string),
		sameReadCount:    make(map[string]int),
		exploreCount:     make(map[string]int),
		writeCount:       make(map[string]int),
		productionWorkDir: "",
	}
	if cfg != nil {
		r.productionWorkDir = cfg.ProductionWorkDir
	}
	// 去抖异步刷新 PROJECT.md：文件增删改后安静期触发 LLM 按职责重分区。
	// cls nil（测试）时 RefreshProjectDoc 走启发式，刷新仍更新文件列表。
	r.refresher = newProjectRefresher(projectRefreshDelay, func(ctx context.Context, workDir string) {
		if err := project.RefreshProjectDoc(ctx, workDir, r.exec.DomainClassifier()); err != nil {
			log.Printf("project refresh failed: workDir=%s err=%v", workDir, err)
		}
	})
	// 注册系统内置的默认工具列表。
	r.registerDefaults()
	// 注册 WriteSharedMemory 工具；store 在 SetSharedMemory 注入后生效。
	// 注册始终发生，使 Schema 中可见；调用时若 store 未注入返回错误。
	r.Register(&writeSharedMemoryTool{})
	// 注册 WriteSpec 工具；store 同样在 SetSharedMemory 注入后生效。
	// 与 WriteSharedMemory 共用同一 SharedMemoryStore 后端，固定 slot "spec"。
	r.Register(&writeSpecTool{})
	// 注册 ask_user 工具（TODO #24 人在回路）；hook 在 SetAskUserHook 注入后生效。
	// 注册始终发生，使 Schema 中可见；调用时 hook 未注入返回错误。
	r.Register(&askUserTool{})
	// 注册 remember_preference 工具（TODO #28 用户画像）；hook 在 SetUserProfileHook 注入后生效。
	r.Register(&rememberPreferenceTool{})
	// 注册 search_knowledge 工具（TODO #27 外部知识库）；hook 在 SetKnowledgeSearchHook 注入后生效。
	r.Register(&searchKnowledgeTool{})
	// 返回构造完成的注册表。
	return r
}

// registerDefaults 将项目内置的所有工具注册到当前 Registry。
func (r *Registry) registerDefaults() {
	// 依次注册文件、命令、HTTP、Git 等类别的内置工具。
	r.Register(&readFileTool{exec: r.exec})
	r.Register(&writeFileTool{exec: r.exec})
	r.Register(&listDirTool{exec: r.exec})
	r.Register(&runCommandTool{exec: r.exec})
	r.Register(&searchInFilesTool{exec: r.exec})
	r.Register(&httpGetTool{exec: r.exec})
	r.Register(&httpPostTool{exec: r.exec})
	r.Register(&gitDiffTool{exec: r.exec})
	r.Register(&gitStatusTool{exec: r.exec})
	r.Register(&gitLogTool{exec: r.exec})
	r.Register(&gitBlameTool{exec: r.exec})
	// RefreshProjectDoc：重写 .bma/PROJECT.md managed 区。仅 MetaAgent/DomainAgent 白名单含。
	r.Register(&refreshProjectDocTool{exec: r.exec})
}

// Register 将工具及其别名注册到注册表中；若传入 nil 则忽略。
func (r *Registry) Register(t Tool) {
	// 防御性判断，避免空指针导致 panic。
	if t == nil {
		return
	}
	// 以工具标准名称为键存入 tools map。
	r.tools[t.Name()] = t
	// 遍历工具别名，将别名映射到标准名称。
	for _, alias := range t.Aliases() {
		r.aliases[alias] = t.Name()
	}
}

// WorkDir 返回 Executor 的工作目录，供 ReActAgent 在系统提示词中注入环境信息。
func (r *Registry) WorkDir() string {
	if r == nil || r.exec == nil {
		return ""
	}
	return r.exec.WorkDir()
}

// SetApprovalHook 注入破坏性操作的用户确认回调（TODO #17 P1）。
// nil（默认）= 全放行，零行为变化；非 nil 时仅对命中边界的调用触发，常规编码流不阻塞。
func (r *Registry) SetApprovalHook(fn ApprovalHookFunc) {
	if r != nil {
		r.approvalHook = fn
	}
}

// needsApproval 判定本次工具调用是否需要用户确认（破坏性工具分级）：
//   - WriteFile（静态 destructive）：仅生产工作目录下触发；
//   - RunCommand：命中危险命令模式（git push/rm -rf/drop table 等）恒触发（与目录无关）；
//     生产目录下的写类命令（rm/mv/cp/touch/mkdir/git rm/git mv）亦触发；
//   - 其余工具与普通命令：不触发，保持自主。
//
// approvalHook 为 nil 时不触发（零行为变化）。
func (r *Registry) needsApproval(name string, args map[string]any) bool {
	if r == nil || r.approvalHook == nil {
		return false
	}
	prod := inProductionWorkDir(r.WorkDir(), r.productionWorkDir)
	switch name {
	case "WriteFile":
		return prod
	case "RunCommand":
		cmd, _ := args["command"].(string)
		if isDangerousCommand(cmd) {
			return true
		}
		return prod && commandAffectsFiles(cmd)
	}
	return false
}

// SetSandboxConfig 把 SafetyConfig 翻译成 Executor 的 SandboxConfig 并注入。
// yaml 中的 tool_sandbox_disabled / tool_sandbox_allowed_paths / tool_sandbox_blocked_cmds
// 经此方法才真正生效；否则 Executor 始终用 DefaultSandboxConfig。
func (r *Registry) SetSandboxConfig(cfg *config.SafetyConfig) {
	// cfg 为 nil 时退化为默认沙箱（保留命令黑名单、限制写路径）。
	if cfg == nil {
		return
	}
	sbCfg := &SandboxConfig{
		AllowedPaths:             cfg.ToolSandboxAllowedPaths,
		AllowWriteOutsideWorkDir: cfg.ToolSandboxDisabled,
	}
	// 追加用户配置的额外命令黑名单到默认黑名单尾部。
	defaultBlocked := DefaultSandboxConfig().BlockedCmds
	sbCfg.BlockedCmds = append(append([]string{}, defaultBlocked...), cfg.ToolSandboxBlockedCmds...)
	r.exec.SetSandboxConfig(sbCfg)
}

// SetRoleWritePathResolver 注入角色级写路径解析器到内部 Executor（Layer 4）。
// bootstrap 传入闭包：roleID -> roleRegistry.Get(roleID).Sandbox.AllowedWritePaths。
// nil 解析器或角色无 Sandbox 配置时 enforceRoleWritePath 跳过，等价于不限制。
func (r *Registry) SetRoleWritePathResolver(fn func(roleID string) []string) {
	if r == nil || r.exec == nil {
		return
	}
	r.exec.SetRoleWritePathResolver(fn)
}

// SetProgressCallback 在构造完成后替换进度回调函数。
// 这允许引导流程先创建注册表，随后由 agent service 注入自身回调。
func (r *Registry) SetProgressCallback(cb ProgressCallback) {
	// 直接覆盖注册表中的 progress 字段。
	r.progress = cb
}

// SetDomainClassifier 注入 LLM 领域命名器到内部 Executor，供 RefreshProjectDoc 工具调用。
// bootstrap 在构造 ModelFactory 后调用；nil 时 RefreshProjectDoc 走启发式命名。
func (r *Registry) SetDomainClassifier(cls project.DomainClassifier) {
	if r == nil || r.exec == nil {
		return
	}
	r.exec.SetDomainClassifier(cls)
}

// SetSharedMemory 注入 WriteSharedMemory 与 WriteSpec 工具共享的 KV 后端。
// bootstrap 在创建 sharedKV 后调用；为 nil 时两个工具调用均返回未配置错误。
// 同时把 store 写入已注册的 writeSharedMemoryTool 与 writeSpecTool 实例，使其立即可用。
func (r *Registry) SetSharedMemory(store SharedMemoryStore) {
	r.sharedMemory = store
	if t, ok := r.tools["WriteSharedMemory"].(*writeSharedMemoryTool); ok {
		t.store = store
	}
	if t, ok := r.tools["WriteSpec"].(*writeSpecTool); ok {
		t.store = store
	}
}

// Dispatch 根据名称调度并执行工具，返回 JSON 序列化后的 Result。
// name 可以是标准名称或已注册别名；args 为工具参数映射。
func (r *Registry) Dispatch(ctx context.Context, name string, args map[string]any) (*Result, error) {
	// 若 name 是别名，则解析为工具的标准名称。
	if canonical, ok := r.aliases[name]; ok {
		name = canonical
	}
	// 从注册表中查找工具；未找到则返回错误。
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}

	// 将参数序列化为 JSON 字符串，忽略错误（参数可能为空）。
	argsStr, _ := marshalNoHTMLEscape(args)
	// 发送工具调用进度事件，便于外部观测当前调用。
	r.emitTool(ctx, "tool_call", name, "调用工具 "+name, string(argsStr))

	// ReadFile 连读检测：重读不再拦截（每次直返磁盘最新内容，天然无脏数据，
	// 也兼容 WriteFile/sed/外部进程改写等一切修改途径）。仅检测"参数完全相同"的连续
	// 调用：第 2 次直返内容并附提醒，第 maxConsecutiveSameRead 次判定死循环触发 LoopExit。
	readNote := ""
	if name == "ReadFile" {
		// 注入 offset/limit 默认值，使执行器与连读检测使用同一取值。
		if v, ok := args["offset"].(float64); !ok || v <= 0 {
			args["offset"] = float64(1)
		}
		if v, ok := args["limit"].(float64); !ok || v <= 0 {
			args["limit"] = float64(defaultReadFileLimit)
		}
		// 从参数中读取目标文件路径并解析为绝对路径，拼装调用键（path+offset+limit）。
		path, _ := args["path"].(string)
		normalizedPath := r.exec.resolvePath(path)
		key := fmt.Sprintf("%s\x00%d\x00%d", filepath.Clean(normalizedPath),
			int(args["offset"].(float64)), int(args["limit"].(float64)))
		scope := scopeKeyFromCtx(ctx)
		n := r.bumpSameRead(scope, key)
		// 连续相同调用达上限：LLM 陷入死循环（内容已直返过仍原样再调），
		// 触发 LoopExit 终止 ReAct 循环，让上层子 Agent 失败回灌摘要，避免烧 token 与时间。
		if n >= maxConsecutiveSameRead {
			msg := fmt.Sprintf("已连续 %d 次以完全相同的参数读取该文件区间，内容均已返回，疑似死循环。需要其他段落应调整 offset 翻页；本次任务终止。", n)
			result := &Result{Tool: "ReadFile", Path: normalizedPath, Error: msg}
			r.fillResult(ctx, result, args)
			log.Printf("[tool] ReadFile LoopExit: scope=%s path=%s consecutive=%d reached max, exiting ReAct loop",
				scope, normalizedPath, n)
			// 发送结果事件后返回包装哨兵错误：主循环 errors.Is 命中即终止循环。
			r.emitResult(ctx, result)
			return result, fmt.Errorf("%w: %s", ErrLoopExit, msg)
		}
		// 第 2 次连续相同调用：直返内容，并在结果末尾附一句提醒引导翻页。
		if n == 2 {
			readNote = "\n[注意] 该区间刚刚已原样返回过，如需其他段落请用 ReadFile(offset=起始行) 翻页，勿重复读取同一区间。"
		}
	}

	// 探索预算：探索类工具合计调用次数上限，防 Agent 陷入探索循环不收敛。
	// 实证 domain-2 调 81 次探索工具 30m 超时未写完。超预算返回错误并终止循环：叶子导向 WriteFile 落地（它不能派发），domain 导向 call_sub_agent 下放叶子。
	// SearchInFiles 计入预算：v8 基准实证验收领域 ReadFile 预算耗尽后改用 SearchInFiles
	// 连搜 14 次零 WriteFile，探索 17 分钟未修一处——"定位性强"同样是发散载体。
	// HTTPGet 不计（联网查询场景不同）。WriteFile/WriteSharedMemory 不计（产出类）。
	// RunCommand 只读型命令（cat/Get-Content 等）按探索计费：防逐文件 cat 绕过预算串行读；
	// 验证/动作类 RunCommand 不计，封禁会让 Agent 写完文件后无法验证而陷入重试死循环。
	exploreLike := name == "ReadFile" || name == "ListDir" || name == "SearchInFiles" || (name == "RunCommand" && isReadLikeCommand(args))
	if exploreLike {
		if blocked := r.checkExploreBudget(ctx); blocked != "" {
			result := &Result{Tool: name, Error: blocked}
			r.fillResult(ctx, result, args)
			scope := scopeKeyFromCtx(ctx)
			log.Printf("[tool] explore budget exhausted: scope=%s tool=%s budget=%d reason=%q",
				scope, name, r.exploreLimit(ctx, scope), blocked)
			// 探索预算耗尽 = 空转死循环信号（实证：domain 只读不写空转 60min）。
			// 返回包装哨兵让主循环终止，而非吞成普通工具错误继续烧轮次。
			r.emitResult(ctx, result)
			return result, fmt.Errorf("%w: %s", ErrLoopExit, blocked)
		}
	}

	// 破坏性工具分级（TODO #17 P1）：命中生产边界/危险命令模式时先经 approvalHook 等用户确认。
	// 拒绝则返回工具级错误（不执行），Agent 可见并自行决策；hook 错误上抛中止本次调用。
	// 非生产环境与普通工具不经过此路径，保持自主。
	if r.needsApproval(name, args) {
		allowed, err := r.approvalHook(ctx, name, args)
		if err != nil {
			return nil, fmt.Errorf("%s approval failed: %w", name, err)
		}
		if !allowed {
			result := &Result{Tool: name, Error: "破坏性操作已被用户拒绝，未执行。"}
			r.fillResult(ctx, result, args)
			r.emitResult(ctx, result)
			return result, nil
		}
	}

	// 确保上下文中携带 SessionID，供后续结果填充和日志关联。
	ctx = WithSessionID(ctx, SessionIDFromContext(ctx))
	// 调用工具实现获取执行结果。
	result := t.Execute(ctx, args)

	// ReadFile 成功读取后：连续第 2 次相同参数调用时附上翻页提醒（内容直返，不拦截）。
	if name == "ReadFile" && result.Success && readNote != "" {
		result.Output += readNote
	}

	// 探索类工具调用后计数 +1（不论成功失败都计，避免失败重试绕过预算）。
	// ReadFile/ListDir 与只读型 RunCommand 计入；验证/动作类 RunCommand 不计。
	if exploreLike {
		r.recordExplore(ctx)
	}

	// WriteFile 成功后失效引用该 path 的共享记忆 entry（Layer 2 缓存一致性）。
	// 防止子 Agent 改文件后，父 Agent 下次派发仍把旧摘要注入新子 Agent task 导致幻觉。
	// ReadFile 无需清已读记录：重读本就直返磁盘最新内容，不存在脏数据问题。
	if name == "WriteFile" && result.Success && result.Path != "" {
		r.invalidateSharedMemoryForPath(ctx, result.Path)
		// 文件新增/修改触发去抖刷新 PROJECT.md 领域影响范围（安静期一次 LLM 重分区）。
		r.scheduleProjectRefresh()
		// 写入已开始：探索预算升档（修复期允许精读，见 exploreBudgetPostWrite）。
		r.recordWrite(ctx)
	}

	// RunCommand 命中删改类命令（rm/mv/mkdir/touch/cp/git rm/git mv）触发去抖刷新，
	// 使文件删除/移动后领域范围同步更新。best-effort，漏匹配滞后到下次显式 RefreshProjectDoc。
	if name == "RunCommand" && result.Success {
		if cmd, _ := args["command"].(string); commandAffectsFiles(cmd) {
			r.scheduleProjectRefresh()
		}
	}

	// 填充结果的通用字段，如 SessionID、ArgsJSON 以及执行器回调。
	r.fillResult(ctx, result, args)

	// 根据执行成功与否更新失败计数（TODO #32 分级：校验拒绝与执行失败分开计，互不共享）。
	if result.Success {
		// 成功则重置该工具的连续失败计数（两类一起清）。
		r.failures.reset(name)
	} else {
		// 无显式 Category 的失败按执行失败计（默认语义，保持既有行为）。
		cat := result.Category
		if cat == "" {
			cat = ResultCategoryExecutionFailed
		}
		if cat == ResultCategoryValidationRejected {
			// 校验拒绝：参数/前置条件问题，修正参数即可——单独计数（阈值更高），
			// 文案明示"这是校验拒绝，不计入失败"，防止模型误以为工具坏了。
			// MetaAgent 的 call_sub_agent/call_sub_agents 豁免连杀终止：编排者的纠偏循环是正常工作方式
			//（事故实证：自检任务连续三次可纠正的校验拒绝，第 3 次即 ErrLoopExit 终止整个 goal）。
			if isMetaDispatch(ctx, name) {
				log.Printf("[tool] validation rejected (meta dispatch exempt): scope=%s tool=%s err=%q", scopeKeyFromCtx(ctx), name, result.Error)
				r.emitResult(ctx, result)
				return result, nil
			}
			n := r.failures.failValidation(name)
			if n >= maxConsecutiveValidationRejections {
				msg := fmt.Sprintf("工具 %s 已连续 %d 次被校验拒绝（参数/前置条件问题，如 task 超长、必填参数缺失、spec 缺失或过期）——"+
					"这是校验拒绝不是执行失败，修正参数或补齐前置条件（如 WriteSpec）后重试即可；本次任务终止以防无限纠偏循环。", name, n)
				log.Printf("[tool] validation rejections LoopExit: scope=%s tool=%s rejections=%d", scopeKeyFromCtx(ctx), name, n)
				result.Error = msg
				r.emitResult(ctx, result)
				return result, fmt.Errorf("%w: %s", ErrLoopExit, msg)
			}
		} else {
			// 执行失败（默认）：累加连续失败计数。
			n := r.failures.fail(name)
			// 连续失败达阈值 = 无效重试死循环信号，返回包装哨兵终止 ReAct 循环
			//（原为 blades ActionLoopExit 上下文信号，无注入方，静默丢弃成死代码）。
			if n >= maxConsecutiveFailures {
				msg := fmt.Sprintf("工具 %s 已连续失败 %d 次，疑似无效重试死循环，本次任务终止。", name, n)
				log.Printf("[tool] consecutive failures LoopExit: scope=%s tool=%s failures=%d", scopeKeyFromCtx(ctx), name, n)
				result.Error = msg
				r.emitResult(ctx, result)
				return result, fmt.Errorf("%w: %s", ErrLoopExit, msg)
			}
		}
	}

	// 发送工具执行结果进度事件。
	r.emitResult(ctx, result)
	// 返回执行结果和错误（工具内部错误已封装在 result 中，此处 error 通常为 nil）。
	return result, nil
}

// isMetaDispatch 判断当前调用是否为 MetaAgent 的派发工具调用（TODO #32-3）：
// 编排者的 call_sub_agent 校验拒绝是正常纠偏（改了参数/补了 spec 再派），
// 豁免连杀终止，防止"正确纠偏的编排者"被守卫误杀。
func isMetaDispatch(ctx context.Context, name string) bool {
	if name != "call_sub_agent" && name != "call_sub_agents" {
		return false
	}
	return RoleIDFromContext(ctx) == "meta"
}

// scheduleProjectRefresh 去抖调度一次 PROJECT.md 刷新（文件增删改后调用）。
// workDir 取 Executor 权威值；refresher 未初始化（测试 Registry）时无操作。
func (r *Registry) scheduleProjectRefresh() {
	if r == nil || r.refresher == nil {
		return
	}
	r.refresher.schedule(r.exec.WorkDir())
}

// invalidateSharedMemoryForPath 遍历共享记忆，删除引用指定 path 的 entry（Layer 2 缓存一致性）。
// 在 WriteFile 成功后调用，防止子 Agent 改文件后父 Agent 下次派发仍注入旧摘要。
// 失败静默（仅影响缓存，不影响 WriteFile 主路径）；path 规范化为绝对路径比较。
// 旧格式 value（无 frontmatter）无法判断引用关系，保留不删，由 Layer 3 stat 校验兜底。
func (r *Registry) invalidateSharedMemoryForPath(ctx context.Context, path string) {
	if r.sharedMemory == nil || path == "" {
		return
	}
	cleanPath := filepath.Clean(path)
	keys := r.sharedMemory.Keys(ctx)
	for _, key := range keys {
		val, err := r.sharedMemory.Get(ctx, key)
		if err != nil || val == "" {
			continue
		}
		fm, _, ok := DecodeSharedMD(val)
		if !ok {
			// 旧格式（无 frontmatter）：无法判断引用关系，保留。
			continue
		}
		for fp := range fm.Files {
			if filepath.Clean(fp) == cleanPath {
				_ = r.sharedMemory.Delete(ctx, key)
				break
			}
		}
	}
}

// fillResult 填充 Result 的 SessionID、ArgsJSON，并触发执行器回调。
func (r *Registry) fillResult(ctx context.Context, result *Result, args map[string]any) {
	// 从上下文中提取 SessionID 并写入结果。
	result.SessionID = SessionIDFromContext(ctx)
	// 将参数序列化为 JSON，失败则跳过。
	if argsJSON, err := marshalNoHTMLEscape(args); err == nil {
		// 将 JSON 字节切片转为字符串。
		argsStr := string(argsJSON)
		// 获取配置允许的最大参数字节数。
		maxBytes := r.exec.agentConfig().ToolExecMaxBytes
		// 如果参数字符串过长，则截断并追加提示，避免结果过大。
		if len(argsStr) > maxBytes {
			argsStr = argsStr[:maxBytes] + "...(truncated)"
		}
		// 将处理后的参数 JSON 写入结果。
		result.ArgsJSON = argsStr
	}
	// 如果执行器设置了回调函数，则把结果透传给回调。
	if r.exec.callback != nil {
		r.exec.callback(result)
	}
}

// emitTool 发送一次工具调用相关的进度事件。
func (r *Registry) emitTool(ctx context.Context, kind, tool, msg, detail string) {
	// 如果未设置进度回调，直接返回，不做任何操作。
	if r.progress == nil {
		return
	}
	// 构造 ProgressEvent 并调用回调通知外部。
	r.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Tool: tool, Message: msg, Detail: detail})
}

// emitResult 发送一次工具执行结果的进度事件。
func (r *Registry) emitResult(ctx context.Context, result *Result) {
	// 如果未设置进度回调，直接返回。
	if r.progress == nil {
		return
	}
	// 将 result 序列化为 JSON，忽略序列化错误。
	detail, _ := marshalNoHTMLEscape(result)
	// 构造结果事件并调用回调。
	r.progress(ctx, ProgressEvent{
		SessionID: SessionIDFromContext(ctx),
		Kind:      "tool_result",
		Tool:      result.Tool,
		Message:   "工具结果 " + result.Tool,
		Detail:    string(detail),
	})
}

// scopeKeyFromCtx 取隔离键：优先 agentID（per-agent），回退 sessionID（兼容旧调用方）。
func scopeKeyFromCtx(ctx context.Context) string {
	if k := AgentIDFromContext(ctx); k != "" {
		return k
	}
	return SessionIDFromContext(ctx)
}

// exploreLimit 返回当前作用域生效的探索预算（仅用于日志，判定逻辑在 checkExploreBudget）：
// 写入未开始用 exploreBudget（叶子反空转）/ exploreBudgetDomain（domain 协调者），
// 写入已开始升档 exploreBudgetPostWrite（修复期精读）。
func (r *Registry) exploreLimit(ctx context.Context, scopeKey string) int {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.writeCount[scopeKey] > 0 {
		return exploreBudgetPostWrite
	}
	if RoleIDFromContext(ctx) == "domain" {
		return exploreBudgetDomain
	}
	return exploreBudget
}

// checkExploreBudget 检查当前作用域探索类工具调用次数是否超预算。
// 返回空字符串表示允许；否则返回拦截原因（叶子：转入 WriteFile；domain：转入派发叶子）。
// 对 ReadFile/ListDir/SearchInFiles 与只读型 RunCommand 生效；
// 验证/动作类 RunCommand 不计（封禁会导致写完文件后无法验证的重试死循环）。
// 预算分两档：首次 WriteFile 前叶子 20 次 / domain 8 次（反探索空转 + 倒逼探索下放），
// 写入已开始 40 次（修复期"读报错位置→改→复验"循环合法；v13 实证禁读逼出整文件盲重写长尾）。
func (r *Registry) checkExploreBudget(ctx context.Context) string {
	scopeKey := scopeKeyFromCtx(ctx)
	if scopeKey == "" {
		return ""
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	postWrite := r.writeCount[scopeKey] > 0
	isDomain := RoleIDFromContext(ctx) == "domain"
	limit := exploreBudget
	if postWrite {
		limit = exploreBudgetPostWrite
	} else if isDomain {
		limit = exploreBudgetDomain
	}
	if r.exploreCount[scopeKey] >= limit {
		if postWrite {
			return fmt.Sprintf("修复期探索预算耗尽（已调 %d 次，上限 %d）。凭已有信息与验收输出直接 WriteFile 修复；修错可在复跑中再校准。", r.exploreCount[scopeKey], limit)
		}
		if isDomain {
			return fmt.Sprintf("探索预算耗尽（domain 协调者上限 %d 次，已调 %d 次）。禁止再亲自探索/阅读：把剩余探索与实现按单文件/单函数拆给叶子助手（call_sub_agent，task 写清文件路径+关键签名+验收），你只负责拆任务、整合 mailbox 摘要与 RunCommand 验证。", limit, r.exploreCount[scopeKey])
		}
		return fmt.Sprintf("探索预算耗尽（已调 %d 次探索工具，上限 %d）。禁止再以任何工具探索/搜索/阅读，凭已有信息直接 WriteFile 实现或修复；修错可在复跑中再校准，空转探索零容忍。", r.exploreCount[scopeKey], limit)
	}
	return ""
}

// recordExplore 把当前作用域探索类工具调用计数 +1。
func (r *Registry) recordExplore(ctx context.Context) {
	scopeKey := scopeKeyFromCtx(ctx)
	if scopeKey == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	r.exploreCount[scopeKey]++
}

// recordWrite 把当前作用域 WriteFile 成功计数 +1（驱动探索预算升档）。
func (r *Registry) recordWrite(ctx context.Context) {
	scopeKey := scopeKeyFromCtx(ctx)
	if scopeKey == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	r.writeCount[scopeKey]++
}

// bumpSameRead 记录一次 ReadFile 调用键，返回"与上一次完全相同"的连续次数（含本次）。
// 参数有任何变化（翻页/换文件/改 limit）即归零重计：重读本身合法，每次直返磁盘最新内容；
// 只有参数完全不变的连续重复才意味着模型未吸收内容、陷入死循环。scope 为空时不计数返回 0。
func (r *Registry) bumpSameRead(scopeKey, key string) int {
	if scopeKey == "" {
		return 0
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.lastReadKey[scopeKey] == key {
		r.sameReadCount[scopeKey]++
	} else {
		r.lastReadKey[scopeKey] = key
		r.sameReadCount[scopeKey] = 1
	}
	return r.sameReadCount[scopeKey]
}

// ResetReadHistory 清空指定 agent 的读取状态（连读计数 + 探索预算）。
// 在新用户消息进入时调用，使连读循环检测与探索预算为单任务级而非整个会话级。
// per-agent 作用域：sessionID 仍可用作 MetaAgent 的 agentID（派发时 MetaAgent 持 sessionID 作 agentID），
// 子 Agent 各有独立 agentID，每次派发新 ID 自然隔离；调用方无需改动。
func (r *Registry) ResetReadHistory(sessionID string) {
	if sessionID == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	delete(r.lastReadKey, sessionID)
	delete(r.sameReadCount, sessionID)
	delete(r.exploreCount, sessionID)
	delete(r.writeCount, sessionID)
}

// 供外部框架（如 blades）动态发现和调用工具。
func (r *Registry) Schema() []tools.Tool {
	// 初始化空列表，用于收集所有工具定义。
	var toolsList []tools.Tool
	// 注册 ReadFile 工具：读取文件内容。
	if t, err := tools.NewFunc("ReadFile", "按行区间读取文件内容，输出带行号。path 为相对或绝对路径；offset 为起始行（1-based，默认 1），limit 为读取行数（默认 200，单页另受字符上限截停）。文件较大时用 offset 翻页，输出首行会给出总行数与下一页起点。建议先用 SearchInFiles/ListDir 定位再按区间精读；重读同一区间会直接返回磁盘最新内容。", func(ctx context.Context, in readFileInput) (string, error) {
		// 通过 Dispatch 调用内部 ReadFile 工具，忽略 Dispatch 返回的 error。
		res, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": in.Path, "offset": in.Offset, "limit": in.Limit})
		// 将结果序列化为 JSON 字符串。
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		// 创建成功则加入列表。
		toolsList = append(toolsList, t)
	}
	// 注册 WriteFile 工具：写入文件。
	if t, err := tools.NewFunc("WriteFile", "写入文件。若文件仅作为临时产物使用（例如运行脚本、中间分析、一次性计算），请设置 temporary=true，文件会写入会话级临时目录并在会话结束后自动清理；用户明确要求保留的文件请保持 temporary=false（默认）。", func(ctx context.Context, in writeFileInput) (string, error) {
		// 转发到内部 WriteFile 工具，包含路径、内容和 temporary 标志。
		res, _ := r.Dispatch(ctx, "WriteFile", map[string]any{"path": in.Path, "content": in.Content, "temporary": in.Temporary})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 ListDir 工具：列出目录内容。
	if t, err := tools.NewFunc("ListDir", "列出目录内容。path 为空时默认当前工作目录；用于了解项目结构、定位文件。", func(ctx context.Context, in listDirInput) (string, error) {
		res, _ := r.Dispatch(ctx, "ListDir", map[string]any{"path": in.Path})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 RunCommand 工具：执行 shell 命令。
	if t, err := tools.NewFunc("RunCommand", "执行 shell 命令。命令执行时环境变量 BMA_SESSION_TEMP_DIR 指向本会话的临时目录，如需创建临时文件请写入该目录，会话结束后会自动清理。\n注意：Windows 下走 PowerShell（不是 cmd），需用 PS 语法：`Get-ChildItem` 而非 `dir`，`2>$null` 而非 `2>nul`，`$env:VAR` 而非 `%VAR%`，`-and`/`-or` 而非 `&&`/`||`（PS7+ 才支持 &&）；Linux/macOS 走 bash/sh。系统提示词已注入当前 OS。", func(ctx context.Context, in runCommandInput) (string, error) {
		// 构造参数映射，命令为必填。
		args := map[string]any{"command": in.Command}
		// 若超时时间大于 0，则加入参数中。
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		res, _ := r.Dispatch(ctx, "RunCommand", args)
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 SearchInFiles 工具：在文件中搜索文本。
	// 描述显式声明字面匹配语义（不支持正则），防模型按 grep 习惯写正则导致零命中。
	if t, err := tools.NewFunc("SearchInFiles", "在文件中搜索文本（字面文本匹配，大小写不敏感，不支持正则表达式）。pattern 含 | 时按关键词拆分、任意关键词命中即记一行（如 \"bug|error\" 等价于两次搜索的并集）；dir 为起始目录（默认当前工作目录）。适合先定位再精读，返回匹配行及上下文；无匹配时返回提示文案而非错误。", func(ctx context.Context, in searchInFilesInput) (string, error) {
		res, _ := r.Dispatch(ctx, "SearchInFiles", map[string]any{"pattern": in.Pattern, "dir": in.Dir})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 HTTPGet 工具：发起 HTTP GET 请求。
	if t, err := tools.NewFunc("HTTPGet", "发起 HTTP GET 请求。url 必填，headers/timeout（秒）可选；返回状态码与响应体。", func(ctx context.Context, in httpGetInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		res, _ := r.Dispatch(ctx, "HTTPGet", args)
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 HTTPPost 工具：发起 HTTP POST 请求。
	if t, err := tools.NewFunc("HTTPPost", "发起 HTTP POST 请求（默认 JSON body）。url 必填，headers/body/timeout（秒）可选；返回状态码与响应体。", func(ctx context.Context, in httpPostInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers, "body": in.Body}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		res, _ := r.Dispatch(ctx, "HTTPPost", args)
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 GitDiff 工具：查看 Git 差异。
	if t, err := tools.NewFunc("GitDiff", `查看 Git 差异。target 为空时显示未暂存变更；"--staged" 显示暂存区变更；"HEAD~1..HEAD" 等显示历史区间差异。`, func(ctx context.Context, in gitDiffInput) (string, error) {
		res, _ := r.Dispatch(ctx, "GitDiff", map[string]any{"target": in.Target, "path": in.Path})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 GitStatus 工具：查看 Git 工作区状态。
	if t, err := tools.NewFunc("GitStatus", "查看 Git 工作区状态（简短格式）。", func(ctx context.Context, in gitStatusInput) (string, error) {
		res, _ := r.Dispatch(ctx, "GitStatus", map[string]any{})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 GitLog 工具：查看 Git 提交历史。
	if t, err := tools.NewFunc("GitLog", "查看 Git 提交历史。limit 控制返回条数（默认 20），path 可限定文件/目录。", func(ctx context.Context, in gitLogInput) (string, error) {
		res, _ := r.Dispatch(ctx, "GitLog", map[string]any{"limit": in.Limit, "path": in.Path})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 GitBlame 工具：查看指定文件每行最后修改者。
	if t, err := tools.NewFunc("GitBlame", "查看指定文件每行的最后修改者（git blame）。", func(ctx context.Context, in gitBlameInput) (string, error) {
		res, _ := r.Dispatch(ctx, "GitBlame", map[string]any{"path": in.Path})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 RefreshProjectDoc 工具：重写 .bma/PROJECT.md 的 managed 区。
	// 仅 MetaAgent/DomainAgent 白名单含。大改动后显式调用同步项目概览，标记区外的人手补充保留。
	if t, err := tools.NewFunc("RefreshProjectDoc", "重写 .bma/PROJECT.md 的 managed 区（启发式扫描当前工作目录：模块/语言/命令/推荐领域拆分/文档地图）。大改动后调用以同步项目概览。标记区外的人手补充保留。无参数。仅 MetaAgent/DomainAgent。", func(ctx context.Context, in refreshProjectDocInput) (string, error) {
		res, _ := r.Dispatch(ctx, "RefreshProjectDoc", map[string]any{})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 WriteSharedMemory 工具：写入 KV 共享记忆，供子 Agent 经 dispatcher.injectKVMemory 自动读取。
	// 仅暴露给 MetaAgent（meta 角色白名单），固定助手/DomainAgent 的白名单不含此工具。
	// files 字段填涉及的文件路径列表，写入时记录 mtime；任一文件被 WriteFile 修改后该记忆自动失效。
	if t, err := tools.NewFunc("WriteSharedMemory", "把派发前采集的关键上下文（文件路径、行号、函数签名、前置结论、验收标准）写入共享记忆。被派发的子 Agent 会自动读取，避免重读全文件。files 字段填涉及的文件路径列表，写入时记录 mtime，任一文件被 WriteFile 修改后该记忆自动失效。仅 MetaAgent 可用。", func(ctx context.Context, in writeSharedMemoryInput) (string, error) {
		args := map[string]any{"content": in.Content}
		if len(in.Files) > 0 {
			files := make([]any, 0, len(in.Files))
			for _, f := range in.Files {
				files = append(files, f)
			}
			args["files"] = files
		}
		res, _ := r.Dispatch(ctx, "WriteSharedMemory", args)
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 暴露 WriteSpec 工具：派发子 Agent 前写入结构化任务规范（goal/acceptance/constraints/files）。
	// SpecEnforcement 开启时 dispatcher 强制 call_sub_agent 前先调本工具，否则拒绝派发。
	// 与 WriteSharedMemory 共享 KV 后端，固定 slot "spec"，files 字段记录 mtime 供失效校验。
	if t, err := tools.NewFunc("WriteSpec", "派发子 Agent 前写入结构化任务规范（目标/验收/约束/涉及文件）。dispatcher 会强制 call_sub_agent 前先调本工具，并把规范作为【任务规范】前缀注入子 Agent。files 字段填涉及的文件路径列表，写入时记录 mtime；任一文件被 WriteFile 修改后该规范自动失效，下次派发子 Agent 不再注入旧规范。覆盖语义：同一 parent 的写入覆盖前一次内容（不追加）。每个 parent 只存一份 spec，兄弟子 Agent 共享。", func(ctx context.Context, in writeSpecInput) (string, error) {
		args := map[string]any{"goal": in.Goal}
		if len(in.Acceptance) > 0 {
			acc := make([]any, 0, len(in.Acceptance))
			for _, a := range in.Acceptance {
				acc = append(acc, a)
			}
			args["acceptance"] = acc
		}
		if len(in.Constraints) > 0 {
			con := make([]any, 0, len(in.Constraints))
			for _, c := range in.Constraints {
				con = append(con, c)
			}
			args["constraints"] = con
		}
		if len(in.Files) > 0 {
			files := make([]any, 0, len(in.Files))
			for _, f := range in.Files {
				files = append(files, f)
			}
			args["files"] = files
		}
		res, _ := r.Dispatch(ctx, "WriteSpec", args)
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 暴露 call_sub_agent 工具（若已由 subagent.Dispatcher 安装到注册表）。
	// 该工具不在 registerDefaults 中注册，而是启动期由调度器按需安装；
	// 描述文本由工具实现通过 Description() 提供（含当前可调用角色的动态清单）。
	if ct, ok := r.tools["call_sub_agent"]; ok {
		desc := "将子任务派发给指定角色的子 Agent 异步执行。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("call_sub_agent", desc, func(ctx context.Context, in callSubAgentInput) (string, error) {
			res, _ := r.Dispatch(ctx, "call_sub_agent", map[string]any{"role_id": in.RoleID, "task": in.Task, "domain": in.Domain, "responsibility": in.Responsibility, "mode": in.Mode})
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 暴露 send_message 工具（若已由 subagent.Dispatcher 安装到注册表）。
	// 该工具支持任意 Agent 向另一个 Agent 实例邮箱投递消息，是多 Agent 协作验证闭环的原语。
	if ct, ok := r.tools["send_message"]; ok {
		desc := "向另一个 Agent 实例邮箱投递消息。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("send_message", desc, func(ctx context.Context, in sendMessageInput) (string, error) {
			args := map[string]any{"to_agent_id": in.ToAgentID, "subject": in.Subject}
			if in.Body != "" {
				args["body"] = in.Body
			}
			if in.MessageType != "" {
				args["message_type"] = in.MessageType
			}
			if in.ThreadID != "" {
				args["thread_id"] = in.ThreadID
			}
			res, _ := r.Dispatch(ctx, "send_message", args)
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 暴露 create_role 工具（若已由 role.Registry.RegisterTools 安装到注册表）。
	// 仅 MetaAgent 白名单含此工具，运行时注册动态角色供 call_sub_agent 派发。
	if ct, ok := r.tools["create_role"]; ok {
		desc := "运行时注册一个新的动态角色。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("create_role", desc, func(ctx context.Context, in createRoleInput) (string, error) {
			args := map[string]any{
				"id":            in.ID,
				"name":          in.Name,
				"system_prompt": in.SystemPrompt,
				"description":   in.Description,
				"can_be_called": in.CanBeCalled,
			}
			if len(in.Tools) > 0 {
				tools := make([]any, 0, len(in.Tools))
				for _, t := range in.Tools {
					tools = append(tools, t)
				}
				args["tools"] = tools
			}
			if len(in.Parents) > 0 {
				parents := make([]any, 0, len(in.Parents))
				for _, p := range in.Parents {
					parents = append(parents, p)
				}
				args["parents"] = parents
			}
			res, _ := r.Dispatch(ctx, "create_role", args)
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 暴露 list_roles 工具（若已由 role.Registry.RegisterTools 安装到注册表）。
	// 仅 MetaAgent 白名单含此工具，列出当前所有角色供派发决策参考。
	if ct, ok := r.tools["list_roles"]; ok {
		desc := "列出当前所有可用角色。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("list_roles", desc, func(ctx context.Context, in listRolesInput) (string, error) {
			res, _ := r.Dispatch(ctx, "list_roles", map[string]any{})
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 返回收集到的所有 blades 工具定义。
	return toolsList
}

// sendMessageInput 是 send_message 工具的入参结构。
type sendMessageInput struct {
	ToAgentID   string `json:"to_agent_id" description:"目标 Agent 实例 ID"`
	Subject     string `json:"subject" description:"一行摘要"`
	Body        string `json:"body" description:"详情正文（可空）"`
	MessageType string `json:"message_type" description:"消息类型：request（默认，期望回复）/ info（单向通知）/ reply（对先前 request 的回复）"`
	ThreadID    string `json:"thread_id" description:"会话线程标识（可空，同一问答链共享）"`
}

// createRoleInput 是 create_role 工具的入参结构。
type createRoleInput struct {
	ID           string   `json:"id" description:"角色唯一 ID。不可为 meta/domain（内置保留）。"`
	Name         string   `json:"name" description:"角色人类可读名称。"`
	SystemPrompt string   `json:"system_prompt" description:"角色系统提示词。决定角色行为边界与工作模式。"`
	Description  string   `json:"description" description:"角色职责描述，供 LLM 在选择派发目标时参考。"`
	Tools        []string `json:"tools" description:"该角色可用的工具名列表（如 ReadFile/WriteFile/call_sub_agent）。"`
	CanBeCalled  bool     `json:"can_be_called" description:"是否可被其他 Agent 调用。默认 true；false 表示纯调度型。"`
	Parents      []string `json:"parents" description:"可调用此角色的父角色 ID 列表。空表示任何编排者可调用。"`
}

// listRolesInput 是 list_roles 工具的入参结构，无字段。
type listRolesInput struct{}

// ---- 工具实现 ----

// readFileTool 是 ReadFile 工具的封装，内部委托 Executor 执行。
type readFileTool struct{ exec *Executor }

// Name 返回工具标准名称 ReadFile。
func (t *readFileTool) Name() string { return "ReadFile" }

// Aliases 返回 ReadFile 的别名列表。
func (t *readFileTool) Aliases() []string { return []string{"read_file", "readFile"} }

// Execute 调用 Executor 的 readFile 方法完成读取。
func (t *readFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.readFile(args)
}

// writeFileTool 是 WriteFile 工具的封装。
type writeFileTool struct{ exec *Executor }

// Name 返回工具标准名称 WriteFile。
func (t *writeFileTool) Name() string { return "WriteFile" }

// Aliases 返回 WriteFile 的别名列表。
func (t *writeFileTool) Aliases() []string { return []string{"write_file", "writeFile"} }

// Destructive 标记 WriteFile 为破坏性操作（文件内容不可逆覆盖）：
// 生产工作目录下触发用户确认（TODO #17 P1 破坏性工具分级）。
func (t *writeFileTool) Destructive() bool { return true }

// Execute 调用 Executor 的 writeFile 方法完成写入。
func (t *writeFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.writeFile(ctx, args)
}

// listDirTool 是 ListDir 工具的封装。
type listDirTool struct{ exec *Executor }

// Name 返回工具标准名称 ListDir。
func (t *listDirTool) Name() string { return "ListDir" }

// Aliases 返回 ListDir 的别名列表。
func (t *listDirTool) Aliases() []string { return []string{"list_dir", "listDir"} }

// Execute 调用 Executor 的 listDir 方法完成目录列出。
func (t *listDirTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.listDir(args)
}

// runCommandTool 是 RunCommand 工具的封装。
type runCommandTool struct{ exec *Executor }

// Name 返回工具标准名称 RunCommand。
func (t *runCommandTool) Name() string { return "RunCommand" }

// Aliases 返回 RunCommand 的别名列表。
func (t *runCommandTool) Aliases() []string { return []string{"run_command", "runCommand"} }

// Execute 调用 Executor 的 runCommand 方法执行命令。
func (t *runCommandTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.runCommand(ctx, args)
}

// searchInFilesTool 是 SearchInFiles 工具的封装。
type searchInFilesTool struct{ exec *Executor }

// Name 返回工具标准名称 SearchInFiles。
func (t *searchInFilesTool) Name() string { return "SearchInFiles" }

// Aliases 返回 SearchInFiles 的别名列表。
func (t *searchInFilesTool) Aliases() []string { return []string{"search_in_files", "searchInFiles"} }

// Execute 调用 Executor 的 searchInFiles 方法完成搜索。
func (t *searchInFilesTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.searchInFiles(args)
}

// httpGetTool 是 HTTPGet 工具的封装。
type httpGetTool struct{ exec *Executor }

// Name 返回工具标准名称 HTTPGet。
func (t *httpGetTool) Name() string { return "HTTPGet" }

// Aliases 返回 HTTPGet 的别名列表。
func (t *httpGetTool) Aliases() []string { return []string{"http_get", "httpGet"} }

// Execute 调用 Executor 的 httpGet 方法发起 GET 请求。
func (t *httpGetTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.httpGet(ctx, args)
}

// httpPostTool 是 HTTPPost 工具的封装。
type httpPostTool struct{ exec *Executor }

// Name 返回工具标准名称 HTTPPost。
func (t *httpPostTool) Name() string { return "HTTPPost" }

// Aliases 返回 HTTPPost 的别名列表。
func (t *httpPostTool) Aliases() []string { return []string{"http_post", "httpPost"} }

// Execute 调用 Executor 的 httpPost 方法发起 POST 请求。
func (t *httpPostTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.httpPost(ctx, args)
}

// gitDiffTool 是 GitDiff 工具的封装。
type gitDiffTool struct{ exec *Executor }

// Name 返回工具标准名称 GitDiff。
func (t *gitDiffTool) Name() string { return "GitDiff" }

// Aliases 返回 GitDiff 的别名列表。
func (t *gitDiffTool) Aliases() []string { return []string{"git_diff", "gitDiff"} }

// Execute 调用 Executor 的 gitDiff 方法查看差异。
func (t *gitDiffTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitDiff(args)
}

// gitStatusTool 是 GitStatus 工具的封装。
type gitStatusTool struct{ exec *Executor }

// Name 返回工具标准名称 GitStatus。
func (t *gitStatusTool) Name() string { return "GitStatus" }

// Aliases 返回 GitStatus 的别名列表。
func (t *gitStatusTool) Aliases() []string { return []string{"git_status", "gitStatus"} }

// Execute 调用 Executor 的 gitStatus 方法查看状态。
func (t *gitStatusTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitStatus(args)
}

// gitLogTool 是 GitLog 工具的封装。
type gitLogTool struct{ exec *Executor }

// Name 返回工具标准名称 GitLog。
func (t *gitLogTool) Name() string { return "GitLog" }

// Aliases 返回 GitLog 的别名列表。
func (t *gitLogTool) Aliases() []string { return []string{"git_log", "gitLog"} }

// Execute 调用 Executor 的 gitLog 方法查看提交历史。
func (t *gitLogTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitLog(args)
}

// gitBlameTool 是 GitBlame 工具的封装。
type gitBlameTool struct{ exec *Executor }

// Name 返回工具标准名称 GitBlame。
func (t *gitBlameTool) Name() string { return "GitBlame" }

// Aliases 返回 GitBlame 的别名列表。
func (t *gitBlameTool) Aliases() []string { return []string{"git_blame", "gitBlame"} }

// Execute 调用 Executor 的 gitBlame 方法查看行级 blame 信息。
func (t *gitBlameTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitBlame(args)
}
