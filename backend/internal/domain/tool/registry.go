package tool

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于传递上下文与取消信号
	"encoding/json" // json 用于动态工具兜底的入参反序列化
	"fmt"           // fmt 用于格式化错误信息
	"log"           // log 用于记录拦截/失败等不影响主流程的可观测事件
	"os"            // os 用于 stat 文件 mtime（读取状态追踪）
	"path/filepath" // filepath 用于规范化文件路径
	"strconv"       // strconv 用于解析 ReadFile 分页头总行数（长文件判定）
	"strings"       // strings 用于拼接已读文件列表
	"sync"          // sync 提供互斥锁保护并发状态
	"time"          // time 用于共享记忆失效标记时间戳（InvalidatedAt）

	"github.com/blockmemory/agent/backend/internal/config"  // config 包提供 Agent 阈值配置
	"github.com/blockmemory/agent/backend/internal/project" // project 包提供 DomainClassifier 接口
	"github.com/go-kratos/blades/tools"                     // blades tools 包提供对外暴露的工具定义
	"github.com/google/jsonschema-go/jsonschema"           // jsonschema 提供动态工具入参 schema 类型
)

// maxConsecutiveValidationRejections 定义单个工具连续校验拒绝的最大次数（TODO #32）。
// 校验拒绝是"修正参数即可"的前置条件问题，与执行失败分开计：阈值放宽到 5，
// 仍防"模型复读同一错误参数"的无效循环（编造参数硬闯校验）。
const maxConsecutiveValidationRejections = 5

// maxConsecutiveSameRead 是同一 scope 内"参数完全相同的 ReadFile"连续调用次数上限。
// 重读不再被拦截（每次直返磁盘最新内容，天然无脏数据），但参数完全不变的连续重复
// 调用意味着模型未吸收内容、陷入死循环（实证：代码助手对 config.js 反复 ReadFile 10+ 次）：
// 第 2 次直返内容并附一句提醒，第 3 次触发 ActionLoopExit 终止 ReAct 循环兜底。
const maxConsecutiveSameRead = 3

// SchemaSource 是动态注册工具（热插拔插件等）暴露给 LLM 的可选接口：
// 实现后 Registry.Schema() 兜底按注册顺序将其包装为 blades 工具定义。
// 未实现该接口的注册工具不会出现在 Schema 中（保持既有行为，如 ask_user 等
// "注册但按需可见"的内置工具不被意外推到 LLM）。
// 由 plugins 包的工具适配器实现（MCP 远端工具透传服务端描述与入参 schema）。
type SchemaSource interface {
	// Description 返回工具功能描述，注入 LLM function calling schema。
	Description() string
	// InputSchema 返回工具入参 JSON Schema；nil 时退化为空对象 schema。
	InputSchema() *jsonschema.Schema
}

// Tool 是内置工具的通用接口，所有具体工具都需要实现该接口。
type Tool interface {
	// Name 返回工具的标准名称，作为主键用于注册和调度。
	Name() string
	// Aliases 返回工具的别名列表，用于兼容不同的调用习惯。
	Aliases() []string
	// Execute 执行工具的核心方法，接收上下文和参数映射，返回执行结果。
	Execute(ctx context.Context, args map[string]any) *Result
}

// failureCounter 用于统计连续校验拒绝次数，支持并发安全地增加计数和重置计数。
// 校验拒绝（validation_rejected，阈值 5 终止）按工具名计数——"修正参数即可"
// 的前置条件问题，修正参数后重试即恢复，无需指纹区分（TODO #32）。
type failureCounter struct {
	// mu 保护 validationCounts 的读写锁，避免并发竞争。
	mu sync.Mutex
	// validationCounts 记录每个工具名称对应的连续校验拒绝次数。
	validationCounts map[string]int
}

// newFailureCounter 创建一个新的失败计数器，内部 map 已经初始化。
func newFailureCounter() *failureCounter {
	return &failureCounter{
		validationCounts: make(map[string]int),
	}
}

// failValidation 将指定工具的连续校验拒绝次数加 1，并返回当前次数。
func (f *failureCounter) failValidation(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validationCounts[name]++
	return f.validationCounts[name]
}

// reset 将指定工具的全部连续校验拒绝计数清零。
func (f *failureCounter) reset(name string) {
	// 加锁保护 validationCounts 的并发修改。
	f.mu.Lock()
	// 函数退出时释放锁，避免遗忘。
	defer f.mu.Unlock()
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
	// mu 保护 tools / aliases / schemaOrder 的并发读写（热插拔插件 enable/disable
	// 与进行中的 Dispatch / Schema 并存，Register/Unregister 写锁、读取读锁）。
	mu sync.RWMutex
	// tools 按标准名称存储已注册的工具实例。
	tools map[string]Tool
	// aliases 存储别名到标准名称的映射。
	aliases map[string]string
	// schemaOrder 按注册顺序记录工具标准名（去重），供 Schema() 动态工具兜底按稳定顺序暴露；
	// 热插拔插件工具走此列表，保证每轮 Schema 顺序确定（LLM 工具列表稳定性）。
	schemaOrder []string
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
	// longFileScopes 按 agentID 记录最近一次成功 ReadFile 是否为长文件（TODO #72）：
	// 返回总行数 > longFileReadLines 时置 true，下一次同文件同区间连读上限放宽。
	longFileScopes map[string]bool
	// fileReadMtime 按 agentID 记录该 scope 已读文件的 mtime（cleanPath → 上次成功
	// ReadFile/WriteFile/EditFile 时的 mtime），即"读取状态追踪"（Claude Code 同款机制的
	// 提示版）：重复读取且 mtime 未变时提示直接引用上文（消除防御性重读——实证领域 Agent
	// 两小时 ReadFile 610 次大量为"忘了文件现状"的重读）；写入前发现 mtime 已变/从未读过
	// 时附 stale 警告（old_string 可能基于过期内容）。advisory 提示，不做硬阻断。
	fileReadMtime map[string]map[string]time.Time
	// sharedMemory 是 WriteSharedMemory 工具的 KV 后端，由 bootstrap 注入。
	// 为 nil 时 WriteSharedMemory 注册但不生效，调用返回 store 未配置错误。
	sharedMemory SharedMemoryStore
	// refresher 去抖异步刷新 .bma/PROJECT.md：WriteFile/删改类 RunCommand 成功后 schedule，
	// 安静期触发 LLM 按职责重分区，使领域影响范围随文件增删改自动更新。
	refresher *projectRefresher
	// fileMap 任务级文件小地图追踪器：ReadFile/WriteFile/EditFile 成功后按 Agent 登记
	// 触碰文件（mtime 缓存符号轮廓），FileMapText 渲染注入文本供记忆流水线尾部常驻注入。
	fileMap *fileMapTracker
	// approvalHook 是破坏性操作的用户确认回调（TODO #17 P1）。nil（默认）= 全放行，
	// 零行为变化；非 nil 时仅对命中边界的调用触发（WriteFile 在生产目录 / 危险命令模式），
	// 常规编码流不阻塞。由 bootstrap 注入 ReactService.ApprovalHook。
	approvalHook ApprovalHookFunc
	// productionWorkDir 是配置的生产环境工作目录（绝对路径）；空 = 未启用生产边界确认，
	// 仅危险命令模式（isDangerousCommand）触发确认。
	productionWorkDir string
	// approvalDisabled 全信任模式（config: tool_approval_disabled）：true 时 needsApproval
	// 直接短路，所有破坏性操作不再推「需确认」、照常执行（动机与边界见 SafetyConfig 注释）。
	approvalDisabled bool
	// pluginMgr 是 plugin_* 工具（TODO #51）依赖的插件管理面，由 bootstrap 注入
	// plugins.ToolManagerAdapter；nil 时工具返回未配置错误。
	pluginMgr PluginManager
	// pluginVisibility 插件工具角色可见性回调（权限天花板，TODO #52）：由 bootstrap 注入
	// plugins.Manager.ToolVisibility（与 agent/dispatcher 侧同源）；nil 时挂载不做天花板校验。
	pluginVisibility PluginVisibilityFunc
	// mountedMu 保护 mounted 挂载集（TODO #52 按需挂载）。
	mountedMu sync.RWMutex
	// mounted 按 scope（agentID）记录已挂载的插件工具名；agent 包 adapter.Schema() 读此
	// 集收窄插件工具可见集 = 天花板 ∩ 已挂载集（默认收窄为角色基础工具）。
	mounted map[string]map[string]bool
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
		exec:              exec,
		progress:          progress,
		failures:          newFailureCounter(),
		tools:             make(map[string]Tool),
		aliases:           make(map[string]string),
		lastReadKey:       make(map[string]string),
		sameReadCount:     make(map[string]int),
		longFileScopes:    make(map[string]bool),
		fileReadMtime:     make(map[string]map[string]time.Time),
		fileMap:           newFileMapTracker(),
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
	// 注册 plugin_* 工具组（TODO #51 插件自安装闭环）；mgr 在 SetPluginManager 注入后生效。
	r.Register(&pluginSearchTool{})
	r.Register(&pluginInstallTool{})
	r.Register(&pluginToggleTool{enable: true})
	r.Register(&pluginToggleTool{enable: false})
	// 注册工具目录/挂载工具组（TODO #52 插件分配给 Agent）：tool_catalog 发现天花板内
	// 插件工具，tool_mount/tool_unmount 在天花板内按需挂载/卸载（角色白名单外不可见）。
	r.Register(&toolCatalogTool{reg: r})
	r.Register(&toolMountTool{reg: r})
	r.Register(&toolUnmountTool{reg: r})
	r.Register(&pluginListTool{})
	// 返回构造完成的注册表。
	return r
}

// registerDefaults 将项目内置的所有工具注册到当前 Registry。
func (r *Registry) registerDefaults() {
	// 依次注册文件、命令、HTTP、Git 等类别的内置工具。
	r.Register(&readFileTool{exec: r.exec})
	r.Register(&writeFileTool{exec: r.exec})
	r.Register(&editFileTool{exec: r.exec})
	r.Register(&restoreFileTool{exec: r.exec})
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
// 并发安全：写锁；同名重复注册覆盖（schemaOrder 不重复追加）。
func (r *Registry) Register(t Tool) {
	// 防御性判断，避免空指针导致 panic。
	if t == nil {
		return
	}
	name := t.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	// 首次注册的标准名追加进 schemaOrder，保持注册顺序稳定。
	if _, ok := r.tools[name]; !ok {
		r.schemaOrder = append(r.schemaOrder, name)
	}
	// 以工具标准名称为键存入 tools map。
	r.tools[name] = t
	// 遍历工具别名，将别名映射到标准名称。
	for _, alias := range t.Aliases() {
		r.aliases[alias] = name
	}
}

// Unregister 摘除工具及其全部别名（热插拔插件 disable / 断线摘除路径）。
// 进行中的 Dispatch 已持工具实例引用，摘除后自然完成，不中断；
// 之后对新调用方返回 "unknown tool"，由 ReAct 循环自愈。
// 返回是否确有摘除（未知工具名返回 false）。
// 并发安全：写锁。
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	// 摘除指向该标准名的全部别名。
	for alias, canonical := range r.aliases {
		if canonical == name {
			delete(r.aliases, alias)
		}
	}
	// 从注册顺序列表中移除，Schema() 不再暴露。
	for i, n := range r.schemaOrder {
		if n == name {
			r.schemaOrder = append(r.schemaOrder[:i], r.schemaOrder[i+1:]...)
			break
		}
	}
	return true
}

// toolByName 读锁取出已注册工具实例；未注册返回 nil, false。
func (r *Registry) toolByName(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Has 判断指定标准名是否已注册（热插拔插件 enable 时预检工具名冲突）。
func (r *Registry) Has(name string) bool {
	_, ok := r.toolByName(name)
	return ok
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

// SetApprovalDisabled 注入全信任模式开关（config: tool_approval_disabled）。
// true 时所有破坏性操作不再推「需确认」，直接执行。
func (r *Registry) SetApprovalDisabled(disabled bool) {
	if r != nil {
		r.approvalDisabled = disabled
	}
}

// needsApproval 判定本次工具调用是否需要用户确认（破坏性工具分级）：
//   - WriteFile（静态 destructive）：仅生产工作目录下触发；
//   - RunCommand：命中危险命令模式（git push/rm -rf/drop table 等）恒触发（与目录无关）；
//     生产目录下的写类命令（rm/mv/cp/touch/mkdir/git rm/git mv）亦触发；
//   - 其余工具与普通命令：不触发，保持自主。
//
// approvalHook 为 nil 时不触发（零行为变化）；approvalDisabled（全信任模式）时一律不触发。
func (r *Registry) needsApproval(name string, args map[string]any) bool {
	if r == nil || r.approvalHook == nil || r.approvalDisabled {
		return false
	}
	prod := inProductionWorkDir(r.WorkDir(), r.productionWorkDir)
	switch name {
	case "WriteFile", "EditFile":
		return prod
	case "RunCommand":
		cmd, _ := args["command"].(string)
		if isDangerousCommand(cmd) {
			return true
		}
		return prod && commandAffectsFiles(cmd)
	}
	// 插件等动态注册工具可自标 Destructive（如 Computer Use 敏感操作）：
	// 无论目录恒要求用户确认，接入同一守卫链（设计文档 §6.2 安全包装）。
	// 置于内置工具分支之后：WriteFile/EditFile 的静态 Destructive 仍只走生产边界语义。
	if t, ok := r.toolByName(name); ok {
		if d, ok := t.(interface{ Destructive() bool }); ok && d.Destructive() {
			return true
		}
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools["WriteSharedMemory"].(*writeSharedMemoryTool); ok {
		t.store = store
	}
	if t, ok := r.tools["WriteSpec"].(*writeSpecTool); ok {
		t.store = store
		// 文件后端时同步注入默认 workDir，供 baseline_content 内联落盘定位
		// <workDir>/.bma/baseline/；Execute 时经 store.WorkDirOf(ctx) 按 ctx
		// 会话目录覆盖（S2）。非文件后端保持空串。
		if wd, ok := store.(interface{ WorkDir() string }); ok {
			t.workDir = wd.WorkDir()
		} else {
			t.workDir = ""
		}
	}
}

// Dispatch 根据名称调度并执行工具，返回 JSON 序列化后的 Result。
// name 可以是标准名称或已注册别名；args 为工具参数映射。
func (r *Registry) Dispatch(ctx context.Context, name string, args map[string]any) (*Result, error) {
	// 读锁内解析别名与查找工具；锁只保护 map 访问，拿到实例引用后即可释放
	// （Unregister 摘除的实例仍被本调用持有，可安全执行完成）。
	r.mu.RLock()
	// 若 name 是别名，则解析为工具的标准名称。
	if canonical, ok := r.aliases[name]; ok {
		name = canonical
	}
	// 从注册表中查找工具；未找到则返回错误。
	t, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}

	// 将参数序列化为 JSON 字符串，忽略错误（参数可能为空）。
	argsStr, _ := marshalNoHTMLEscape(args)
	// 发送工具调用进度事件，便于外部观测当前调用。
	r.emitTool(ctx, "tool_call", name, "调用工具 "+name, string(argsStr))

	// ReadFile 连读检测：重读不再拦截（每次直返磁盘最新内容，天然无脏数据，
	// 也兼容 WriteFile/sed/外部进程改写等一切修改途径）。仅检测"参数完全相同"的连续
	// 调用：第 2 次直返内容并附提醒，达上限判定死循环触发 LoopExit。
	// TODO #72 确认性复读放行：长文件（返回总行数 > longFileReadLines）上限放宽到
	// maxConsecutiveSameReadLongFile——编辑前后同区间复读是合理确认工作流；
	// 短文件保持 3（真死循环）。
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
		normalizedPath := r.exec.resolvePath(ctx, path)
		key := fmt.Sprintf("%s\x00%d\x00%d", filepath.Clean(normalizedPath),
			int(args["offset"].(float64)), int(args["limit"].(float64)))
		scope := scopeKeyFromCtx(ctx)
		n := r.bumpSameRead(scope, key)
		// 长文件判定：同 scope 上一成功 ReadFile 返回总行数 > longFileReadLines 时放宽上限。
		limit := maxConsecutiveSameRead
		if r.isLongFileScope(scope, normalizedPath) {
			limit = maxConsecutiveSameReadLongFile
		}
		// 连续相同调用达上限：LLM 陷入死循环（内容已直返过仍原样再调），
		// 触发 LoopExit 终止 ReAct 循环，让上层子 Agent 失败回灌摘要，避免烧 token 与时间。
		if n >= limit {
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

	// 破坏性工具分级（TODO #17 P1）：命中生产边界/危险命令模式时先经 approvalHook 等用户确认。
	// 拒绝则返回工具级错误（不执行），Agent 可见并自行决策；hook 错误上抛中止本次调用。
	// 非生产环境与普通工具不经过此路径，保持自主。
	if r.needsApproval(name, args) {
		allowed, err := r.approvalHook(ctx, name, args)
		if err != nil {
			return nil, fmt.Errorf("%s approval failed: %w", name, err)
		}
		if !allowed {
			result := &Result{Tool: name, Error: "破坏性操作已被用户拒绝，未执行。请改用不命中危险命令模式的替代方式推进，或直接跳过该清理步骤继续主任务。"}
			r.fillResult(ctx, result, args)
			r.emitResult(ctx, result)
			return result, nil
		}
	}

	// 确保上下文中携带 SessionID，供后续结果填充和日志关联。
	ctx = WithSessionID(ctx, SessionIDFromContext(ctx))

	// 读取状态追踪（写前 stale 检查，advisory）：EditFile/WriteFile 目标文件已存在，
	// 且本 Agent 从未读过或 mtime 与上次读取不一致（期间被外部/别的 Agent 改过）时，
	// 操作仍执行，结果尾部附"内容可能过期"警告。新建文件（不存在）与 temporary 临时文件
	// 无旧内容可过期，不检查，避免每次新建都附噪音警告。
	staleWriteNote := ""
	if name == "EditFile" || name == "WriteFile" {
		if temporary, _ := args["temporary"].(bool); name == "EditFile" || !temporary {
			if path, _ := args["path"].(string); path != "" {
				abs := r.exec.resolvePath(ctx, path)
				if mt, ok := statMtime(abs); ok {
					if prev, seen := r.lastFileMtime(scopeKeyFromCtx(ctx), abs); !seen || !prev.Equal(mt) {
						staleWriteNote = "\n[注意] 该文件自你上次读取后已被修改（或你尚未读过），old_string 可能基于过期内容，建议核对。"
					}
				}
			}
		}
	}

	// 调用工具实现获取执行结果。
	result := t.Execute(ctx, args)

	// ReadFile 成功读取后：连续第 2 次相同参数调用时附上翻页提醒（内容直返，不拦截）；
	// 按总行数标记长文件状态（下一次连读上限放宽，TODO #72）。
	if name == "ReadFile" && result.Success && result.Output != "" {
		if readNote != "" {
			result.Output += readNote
		}
		r.markLongFileScope(scopeKeyFromCtx(ctx), result.Output)
		// 读取状态追踪登记：与上次读取记录比对 mtime 后再登记本次。
		// 同参数连读场景已附连读守卫提醒（readNote），不再重复附加免啰嗦。
		if mt, ok := statMtime(result.Path); ok {
			scope := scopeKeyFromCtx(ctx)
			if prev, seen := r.lastFileMtime(scope, result.Path); seen && readNote == "" {
				if prev.Equal(mt) {
					result.Output += "\n[提示] 该文件自你上次读取后未变更；若上文仍有该内容请直接引用，无需为重读而重读。"
				} else {
					result.Output += "\n[提示] 该文件自你上次读取后已被修改，上文内容可能已过期，以本次返回为准。"
				}
			}
			r.recordFileMtime(scope, result.Path, mt)
		}
	}

	// WriteFile/EditFile/RestoreFile 成功后标记失效引用该 path 的共享记忆 entry（Layer 2 缓存一致性）。
	// 普通槽不物理删除、内容保留：标记 stale 后注入时照常注入并附行号漂移警告，
	// 防止旧逻辑"删记忆"导致下次派发/复用时 Agent 拿裸任务从零重读同一批文件。
	// ReadFile 无需清已读记录：重读本就直返磁盘最新内容，不存在脏数据问题。
	// TODO #72 确认性复读放行：写入成功同时清零该路径连读计数——
	// Read(A)→Write(A)→Read(A)→Read(A) 的编辑后确认不再被杀。
	if (name == "WriteFile" || name == "EditFile" || name == "RestoreFile") && result.Success && result.Path != "" {
		r.invalidateSharedMemoryForPath(ctx, result.Path)
		r.resetSameReadForPath(scopeKeyFromCtx(ctx), result.Path)
		// 写前 stale 检查结果落地：操作已执行，尾部附"内容可能过期"警告（advisory）。
		result.Output += staleWriteNote
		// 登记写入后的新 mtime：本 Agent 刚写的内容视为已知，
		// 避免后续自编辑被误判为"已被外部修改"；其他 Agent 的记录仍正确变 stale。
		if mt, ok := statMtime(result.Path); ok {
			r.recordFileMtime(scopeKeyFromCtx(ctx), result.Path, mt)
		}
		// 文件新增/修改触发去抖刷新 PROJECT.md 领域影响范围（安静期一次 LLM 重分区）。
		r.scheduleProjectRefresh()
	}

	// 任务级文件小地图：ReadFile/WriteFile/EditFile/RestoreFile 成功后登记触碰文件（按 Agent 隔离，
	// mtime 缓存符号轮廓），记忆流水线每轮在 history 尾部常驻注入【本任务文件地图】，
	// 压缩循环压不掉（与上面的失效标记/连读清零同属文件类工具的后置钩子）。
	if result.Success && result.Path != "" &&
		(name == "ReadFile" || name == "WriteFile" || name == "EditFile" || name == "RestoreFile") {
		r.touchFileMap(ctx, result.Path)
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

	// 校验拒绝计数（TODO #32）：校验拒绝按工具名计数，连续达
	// maxConsecutiveValidationRejections 触发 ErrLoopExit 终止（防"复读同一错误参数"无效循环）。
	// 执行失败不计数（连杀指纹已退役，TODO #44）：模型"改→试→复验"的正常调试节奏
	// 不应被硬阈值误杀，真死循环由连读 guard 与 sub_agent_timeout 墙钟兜底。
	if !result.Success {
		// 无显式 Category 的失败按执行失败计（默认语义，保持既有行为）。
		cat := result.Category
		if cat == "" {
			cat = ResultCategoryExecutionFailed
		}
		if cat == ResultCategoryValidationRejected {
			// 校验拒绝：参数/前置条件问题，修正参数即可——单独计数（阈值更高），
			// 文案明示"这是校验拒绝，不计入失败"，防止模型误以为工具坏了。
			// MetaAgent 的 call_sub_agent/call_sub_agents 豁免校验拒绝终止：编排者的纠偏循环是正常工作方式
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
		}
	}

	// 发送工具执行结果进度事件。
	r.emitResult(ctx, result)
	// 返回执行结果和错误（工具内部错误已封装在 result 中，此处 error 通常为 nil）。
	return result, nil
}

// isMetaDispatch 判断当前调用是否为 MetaAgent 的派发工具调用（TODO #32-3）：
// 编排者的 call_sub_agent 校验拒绝是正常纠偏（改了参数/补了 spec 再派），
// 豁免校验拒绝终止，防止"正确纠偏的编排者"被守卫误杀。
func isMetaDispatch(ctx context.Context, name string) bool {
	if name != "call_sub_agent" && name != "call_sub_agents" {
		return false
	}
	return RoleIDFromContext(ctx) == "meta"
}

// verificationCommandPatterns 是验证类命令识别口径（TODO #63 同源模板）：子 Agent
// 证据模板（VerificationEvidenceTemplate）与 L0 重试消息按此生成——识别器认什么，
// prompt 就要求什么，杜绝"回传了 tsc/build 证据但识别器不认"（实证 2026-08-24 塔防
// verify_missing 7 连发）。匹配在命令小写化后的字符串上做子串包含。
var verificationCommandPatterns = []string{
	"--check", "lint", "verify", " test ", "test -",
	"node -c", "go build", "go vet", "py_compile", "pytest",
	// 实证缺口补录：npx tsc --noEmit / vite build / npm run build 等常见验收命令
	// 此前不在口径内（tsc 大写 E 变 noemit；vite 后接空格防匹配 invite 等无关词）。
	"noemit", "tsc ", "vite ", "npm run", "pnpm run", "yarn run", "jest", "vitest", "mocha", "go test",
}

// IsVerificationCommand 判断命令是否为验证/检查类（--check/lint/test/verify 等，
// 退出码即有效反馈）。
// 导出供 agent 包 L0 证据扫描（HasExecutableVerification，TODO #43）复用同一判定口径。
func IsVerificationCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	for _, m := range verificationCommandPatterns {
		if strings.Contains(c, m) {
			return true
		}
	}
	return strings.HasPrefix(c, "test ") || strings.HasSuffix(c, " test")
}

// VerificationEvidenceTemplate 渲染子 Agent 验证证据格式要求（TODO #63）：
// 与 IsVerificationCommand 识别口径同源生成（识别器认什么，prompt 就要求什么）。
// 注入 L0 可执行校验角色的任务前缀与反馈重试消息，保证两处口径一致不漂移。
func VerificationEvidenceTemplate() string {
	patterns := strings.Join(verificationCommandPatterns, " / ")
	return "【验证证据格式】终答前必须用 RunCommand 运行验证类命令（tsc --noEmit / node -c / go test / " +
		"npm run build 等），并必须以 ``` 代码块原样粘贴命令全文与退出码（EXIT_CODE=0）。" +
		"dispatcher 机器识别口径（命令含以下片段即计为验证证据）: " + patterns + "。" +
		"只靠自述\"已测试通过\"不计为证据。"
}

// scheduleProjectRefresh 去抖调度一次 PROJECT.md 刷新（文件增删改后调用）。
// workDir 取 Executor 权威值；refresher 未初始化（测试 Registry）时无操作。
func (r *Registry) scheduleProjectRefresh() {
	if r == nil || r.refresher == nil {
		return
	}
	r.refresher.schedule(r.exec.WorkDir())
}

// specTombstonePrefix 是 spec 槽位失效墓碑值前缀（TODO #74）：
// 物理删除改为写墓碑 "invalidated: 文件 X 已变更"，派发侧据前缀区分
// "从未写"与"已失效"，报错直指真实原因（key 错配时列已有 key 清单）。
const specTombstonePrefix = "invalidated: "

// SpecTombstonePrefix 导出 spec 墓碑前缀（dispatcher 派发侧诊断用，TODO #74）。
const SpecTombstonePrefix = specTombstonePrefix

// invalidateSharedMemoryForPath 遍历共享记忆，失效引用指定 path 的 entry（Layer 2 缓存一致性）。
// 在 WriteFile/EditFile 成功后调用。
// 失败静默（仅影响缓存，不影响 WriteFile 主路径）；path 规范化为绝对路径比较。
// 旧格式 value（无 frontmatter）无法判断引用关系，保留不动，由 Layer 3 stat 校验兜底。
// 普通共享记忆槽：不物理删除，frontmatter 打 invalidated_at 标记、body/Files 原样保留——
// dispatcher 注入侧判 stale 后照常注入并附行号漂移警告（常量值/签名类结论仍可直接采信）。
// 实证：物理删除后无任何重存引导，下次派发/复用拿裸任务从零重读同一批文件
//（单领域 Agent 两小时 ReadFile 610 次 + SearchInFiles 351 次）。
// spec 槽位（<agentID>:spec[...]) 不标 stale 而写墓碑（TODO #74，维持现状）：
// "被失效"与"从未写"在派发报错中语义不同，物理删导致 meta 误诊 key 覆盖
//（实证 2026-08-25：meta 误诊"每 parent 只存一份 spec 互相覆盖"改串行重派白烧一轮）。
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
		// spec 墓碑幂等：已是本 path 的墓碑不再重写（跳过后续解析）。
		if strings.HasPrefix(val, specTombstonePrefix) {
			continue
		}
		fm, body, ok := DecodeSharedMD(val)
		if !ok {
			// 旧格式（无 frontmatter）：无法判断引用关系，保留。
			continue
		}
		for fp := range fm.Files {
			if filepath.Clean(fp) == cleanPath {
				// spec 槽位写墓碑（key 形如 "<agentID>:spec" 或 "<agentID>:spec:<key>"）。
				if slot := key[strings.Index(key, ":")+1:]; slot == SpecSlot || strings.HasPrefix(slot, SpecSlot+":") {
					_ = r.sharedMemory.Set(ctx, key, specTombstonePrefix+filepath.Clean(fp)+" 已变更，请用 WriteSpec 重写")
					break
				}
				// 普通槽：打失效标记重编码落盘（Files 保留旧 mtime，Layer 3 校验与
				// invalidated_at 双重判 stale）；幂等——已标记的同 path 重写一次即可。
				fm.InvalidatedAt = time.Now().Format(time.RFC3339)
				_ = r.sharedMemory.Set(ctx, key, encodeMD(fm, body))
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

// statMtime 返回文件的修改时间；文件不存在或 stat 失败时返回 ok=false。
func statMtime(path string) (time.Time, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// lastFileMtime 取 scope 已登记的 path 上次读取/写入时 mtime（读取状态追踪）。
// 未登记过（从未读过）返回 ok=false。并发安全：与连读守卫共用 readMu。
func (r *Registry) lastFileMtime(scopeKey, path string) (time.Time, bool) {
	if scopeKey == "" || path == "" {
		return time.Time{}, false
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	m := r.fileReadMtime[scopeKey]
	if m == nil {
		return time.Time{}, false
	}
	mt, ok := m[filepath.Clean(path)]
	return mt, ok
}

// recordFileMtime 登记 scope 对 path 的最新已知 mtime（成功 ReadFile/WriteFile/EditFile 后调用）。
func (r *Registry) recordFileMtime(scopeKey, path string, mtime time.Time) {
	if scopeKey == "" || path == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	m := r.fileReadMtime[scopeKey]
	if m == nil {
		m = make(map[string]time.Time)
		r.fileReadMtime[scopeKey] = m
	}
	m[filepath.Clean(path)] = mtime
}

// maxConsecutiveSameReadLongFile 是长文件（ReadFile 返回总行数 > longFileReadLines）的
// 连读上限放宽值（TODO #72）：编辑长文件前后同区间确认性复读是合理工作流，
// 扁平常量 3 误杀（实证 2026-08-25 domain-2 被连读守卫杀时 plan_execute 仅 1/6）。
const maxConsecutiveSameReadLongFile = 6

// longFileReadLines 判定"长文件"的输出行数阈值（TODO #72）。
const longFileReadLines = 500

// resetSameReadForPath 清零指定 scope 中匹配 path 的连读计数（TODO #72 确认性复读放行）：
// WriteFile/EditFile 成功写某路径后调用——编辑后的同区间复读是确认性工作流
//（Read(A)→Write(A)→Read(A)→Read(A) 不再第 3 次被杀），纯探索性 3 连读仍杀。
func (r *Registry) resetSameReadForPath(scopeKey, path string) {
	if scopeKey == "" || path == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	key := r.lastReadKey[scopeKey]
	if key == "" {
		return
	}
	// 调用键格式 "path\x00offset\x00limit"：前缀匹配同路径任意区间。
	if strings.HasPrefix(key, filepath.Clean(path)+"\x00") {
		delete(r.lastReadKey, scopeKey)
		delete(r.sameReadCount, scopeKey)
	}
}

// isLongFileScope 返回该 scope 最近一次 ReadFile 是否为长文件（TODO #72）。
func (r *Registry) isLongFileScope(scopeKey, path string) bool {
	if scopeKey == "" {
		return false
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	return r.longFileScopes[scopeKey]
}

// markLongFileScope 按 ReadFile 成功结果的总行数标记长文件状态（TODO #72）。
// Output 分页头格式 `[共 %d 行 | ...`，解析失败按短文件处理（保守不放宽）。
func (r *Registry) markLongFileScope(scopeKey, output string) {
	long := false
	if strings.HasPrefix(output, "[共 ") {
		rest := strings.TrimPrefix(output, "[共 ")
		if sp := strings.IndexByte(rest, ' '); sp > 0 {
			if n, err := strconv.Atoi(rest[:sp]); err == nil && n > longFileReadLines {
				long = true
			}
		}
	}
	if scopeKey == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if long {
		r.longFileScopes[scopeKey] = true
	} else {
		delete(r.longFileScopes, scopeKey)
	}
}

// ResetReadHistory 清空指定 agent 的连读状态（参数完全相同的 ReadFile 连续次数）
// 与读取状态追踪记录（fileReadMtime）。
// 在新用户消息进入时调用，使连读循环检测为单任务级而非整个会话级。
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
	delete(r.fileReadMtime, sessionID)
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
	// 编辑纪律（2026-08-31 实证）：域 Agent 修改已有文件 90 次全走 WriteFile 整写
	// （EditFile 仅 3 次），单轮流式输出 30-64K token 拖慢 5-22 分钟/轮。修改已有
	// 文件一律 EditFile 局部替换；WriteFile 限新建文件与整文件重写。
	if t, err := tools.NewFunc("WriteFile", "新建文件（整文件写入）。**修改已存在的文件禁止用本工具**：一律用 EditFile 局部替换（只输出改动片段，省 token 且快）；仅当改动面覆盖文件大半时才允许 WriteFile 整写。content 必须是文件的**完整内容**--绝不允许只发修改片段（只发片段会把原文件整文件覆盖为片段，造成数据丢失）。若文件仅作为临时产物使用（例如运行脚本、中间分析、一次性计算），请设置 temporary=true，文件会写入会话级临时目录并在会话结束后自动清理；用户明确要求保留的文件请保持 temporary=false（默认）。极端缩小（新内容 < 原文件 10% 且原文件 >= 5KB）默认拒收，确为有意精简时加 confirm_shrink=true 绕过。", func(ctx context.Context, in writeFileInput) (string, error) {
		// 转发到内部 WriteFile 工具，包含路径、内容和 temporary/confirm_shrink 标志。
		res, _ := r.Dispatch(ctx, "WriteFile", map[string]any{"path": in.Path, "content": in.Content, "temporary": in.Temporary, "confirm_shrink": in.ConfirmShrink})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 EditFile 工具：精确局部替换（TODO #49）。
	// 修改已有文件的首选方式（对齐 Claude Code Edit 惯例）；仅新建文件走 WriteFile。
	if t, err := tools.NewFunc("EditFile", "修改已存在文件的首选方式：精确局部替换，只改指定片段，不重写整个文件（输出 token 与耗时仅为整文件重写的零头）。old_string 必须与文件现有内容逐字符一致（含缩进/空格；行尾 \\r\\n 与 \\n 视为等价），默认须唯一匹配，多处匹配会报错；确需全部替换时传 replace_all=true。定位片段前可先 ReadFile 该文件的目标行段（用 offset/limit 只读相关区间，不必整读大文件）。匹配失败返回错误并附文件开头片段供自查，不会改动文件；多处匹配时在 old_string 中多带几行上下文使其唯一。新建文件用 WriteFile；改动面确实覆盖文件大半时才整写。EditFile 与 WriteFile 同等触发共享记忆/spec 失效与 .bma/snapshots 备份。", func(ctx context.Context, in editFileInput) (string, error) {
		// 转发到内部 EditFile 工具，包含路径、old_string/new_string 与 replace_all 标志。
		res, _ := r.Dispatch(ctx, "EditFile", map[string]any{"path": in.Path, "old_string": in.OldString, "new_string": in.NewString, "replace_all": in.ReplaceAll})
		b, _ := marshalNoHTMLEscape(res)
		return string(b), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// 注册 RestoreFile 工具：从 .bma/snapshots 快照恢复文件。
	// 描述显式引导模型：回退误改/误删时用本工具，不要 ReadFile .bak 再 WriteFile 重写
	// （后者把全文件内容过一遍模型，费 token 且易写错）。
	if t, err := tools.NewFunc("RestoreFile", "把文件回退到 .bma/snapshots 中的备份版本（WriteFile/EditFile 覆盖已存在文件前自动生成快照，保留 24h）。误改/误删文件时优先用本工具恢复：harness 直接从快照拷回，不要 ReadFile .bak 文件再用 WriteFile 重写（费 token 且易出错）。path 为目标文件路径；timestamp 可选，指定要恢复的快照时间戳（格式 20060102-150530），缺省恢复最新一份；目标文件已被删除也可恢复。恢复会覆盖当前文件内容，覆盖前当前内容同样自动备份（恢复本身可回退）。无快照时报错并附可用时间戳。", func(ctx context.Context, in restoreFileInput) (string, error) {
		// 转发到内部 RestoreFile 工具，包含路径与可选时间戳。
		res, _ := r.Dispatch(ctx, "RestoreFile", map[string]any{"path": in.Path, "timestamp": in.Timestamp})
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
	if t, err := tools.NewFunc("WriteSharedMemory", "把派发前采集的关键上下文（文件路径、行号、函数签名、前置结论、验收标准）写入共享记忆。被派发的子 Agent 会自动读取，避免重读全文件。files 字段填涉及的文件路径列表，写入时记录 mtime，任一文件被 WriteFile 修改后该记忆自动失效。仅 MetaAgent 可用。\n侦察结论必须随时沉淀：每读完一批关键文件（消费点 API、签名、常量、行号证据）就立即写入一条，不要攒到最后——侦察中途失败/超时后，下一次重跑靠这些记忆跳过重读，否则全部侦察白干（实证领域 Agent 91 分钟侦察零沉淀，重跑时全套文件重读一遍）。", func(ctx context.Context, in writeSharedMemoryInput) (string, error) {
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
	if t, err := tools.NewFunc("WriteSpec", "派发子 Agent 前写入结构化任务规范（目标/验收/约束/涉及文件）。dispatcher 会强制 call_sub_agent 前先调本工具，并把规范作为【任务规范】前缀注入子 Agent。files 字段填涉及的文件路径列表，写入时记录 mtime；任一文件被 WriteFile 修改后该规范自动失效，下次派发子 Agent 不再注入旧规范。覆盖语义：同一 parent 的写入覆盖前一次内容（不追加）。每个 parent 只存一份 spec，兄弟子 Agent 共享。\n已验证的事实直接钉进 spec（关键常量值、API 签名、行号、结论），不要让子 Agent 现场\"自行验证\"——实证领域 Agent 为一个朝向常量现场写像素测量脚本、为消费点 API 通读全套文件，侦察烧掉整个预算。你已知的就写进去，子 Agent 未知的才让它查。\n还原/复刻类任务填 baseline（已落盘文件路径）或 baseline_content（内联基线全文，本工具自动落盘 .bma/baseline/ 再校验）；不要用插件 filesystem 工具落盘基线——其写入沙箱容器文件系统，宿主校验不可见。", func(ctx context.Context, in writeSpecInput) (string, error) {
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
	if ct, ok := r.toolByName("call_sub_agent"); ok {
		desc := "将子任务派发给指定角色的子 Agent 异步执行。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("call_sub_agent", desc, func(ctx context.Context, in callSubAgentInput) (string, error) {
			res, _ := r.Dispatch(ctx, "call_sub_agent", map[string]any{"role_id": in.RoleID, "task": in.Task, "domain": in.Domain, "responsibility": in.Responsibility, "mode": in.Mode, "verify_kind": in.VerifyKind, "skills": in.Skills})
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 暴露 send_message 工具（若已由 subagent.Dispatcher 安装到注册表）。
	// 该工具支持任意 Agent 向另一个 Agent 实例邮箱投递消息，是多 Agent 协作验证闭环的原语。
	if ct, ok := r.toolByName("send_message"); ok {
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
	// 暴露 submit_plan / review_plan 工具（若已由 subagent.Dispatcher.RegisterPlanTools 安装）。
	// 计划确认机制：下级中大型任务动手前 submit_plan 给上级确认，上级用 review_plan 审批。
	if ct, ok := r.toolByName("submit_plan"); ok {
		desc := "提交执行计划给上级确认。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("submit_plan", desc, func(ctx context.Context, in submitPlanInput) (string, error) {
			res, _ := r.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": in.TaskSummary, "plan": in.Plan})
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	if ct, ok := r.toolByName("review_plan"); ok {
		desc := "审批下级提交的计划。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("review_plan", desc, func(ctx context.Context, in reviewPlanInput) (string, error) {
			res, _ := r.Dispatch(ctx, "review_plan", map[string]any{"plan_id": in.PlanID, "verdict": in.Verdict, "feedback": in.Feedback})
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}); err == nil {
			toolsList = append(toolsList, t)
		}
	}
	// 暴露 create_role 工具（若已由 role.Registry.RegisterTools 安装到注册表）。
	// 仅 MetaAgent 白名单含此工具，运行时注册动态角色供 call_sub_agent 派发。
	if ct, ok := r.toolByName("create_role"); ok {
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
	if ct, ok := r.toolByName("list_roles"); ok {
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
	// 动态注册工具兜底（热插拔插件等）：上述显式块已覆盖全部内置/安装工具，
	// 其余按注册顺序暴露——保证插件工具在 Schema 中出现且顺序稳定（LLM 工具列表稳定性）。
	// 仅实现 SchemaSource 的工具进入此路径（自带描述与入参 schema）；无 schema 的
	// 注册工具保持现状不暴露，避免 ask_user 等"注册但按需可见"内置工具行为漂移。
	covered := map[string]bool{
		"ReadFile": true, "WriteFile": true, "EditFile": true, "ListDir": true,
		"RunCommand": true, "SearchInFiles": true, "HTTPGet": true, "HTTPPost": true,
		"GitDiff": true, "GitStatus": true, "GitLog": true, "GitBlame": true,
		"RefreshProjectDoc": true, "WriteSharedMemory": true, "WriteSpec": true,
		"call_sub_agent": true, "send_message": true, "create_role": true, "list_roles": true,
		"submit_plan": true, "review_plan": true,
	}
	r.mu.RLock()
	order := append([]string(nil), r.schemaOrder...)
	r.mu.RUnlock()
	for _, name := range order {
		if covered[name] {
			continue
		}
		t, ok := r.toolByName(name)
		if !ok {
			continue
		}
		src, ok := t.(SchemaSource)
		if !ok {
			continue
		}
		desc := src.Description()
		if desc == "" {
			desc = "动态注册工具。"
		}
		opts := []tools.Option{}
		if s := src.InputSchema(); s != nil {
			opts = append(opts, tools.WithInputSchema(s))
		}
		f := tools.NewTool(name, desc, tools.HandleFunc(func(ctx context.Context, raw string) (string, error) {
			// LLM 下发的参数 JSON 反序列化为 map 后走统一 Dispatch 路径。
			var args map[string]any
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return "", err
			}
			res, err := r.Dispatch(ctx, name, args)
			if err != nil {
				return "", err
			}
			b, _ := marshalNoHTMLEscape(res)
			return string(b), nil
		}), opts...)
		toolsList = append(toolsList, f)
	}
	// 返回收集到的所有 blades 工具定义。
	return toolsList
}

// submitPlanInput 是 submit_plan 工具的入参结构（计划确认机制）。
type submitPlanInput struct {
	TaskSummary string `json:"task_summary" description:"一行任务简介"`
	Plan        string `json:"plan" description:"计划详情：执行步骤、涉及文件、验收标准。批准后本调用才返回并放行执行"`
}

// reviewPlanInput 是 review_plan 工具的入参结构（计划确认机制）。
type reviewPlanInput struct {
	PlanID   string `json:"plan_id" description:"计划 ID（取自【计划审批请求】消息首行）"`
	Verdict  string `json:"verdict" description:"approve=批准；reject=驳回"`
	Feedback string `json:"feedback" description:"驳回时的具体修改意见（reject 必填，下级按意见修订重提）"`
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
	return t.exec.readFile(ctx, args)
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

// editFileTool 是 EditFile 工具的封装。
type editFileTool struct{ exec *Executor }

// Name 返回工具标准名称 EditFile。
func (t *editFileTool) Name() string { return "EditFile" }

// Aliases 返回 EditFile 的别名列表。
func (t *editFileTool) Aliases() []string { return []string{"edit_file", "editFile"} }

// Destructive 标记 EditFile 为破坏性操作（文件内容不可逆修改）：
// 生产工作目录下触发用户确认，与 WriteFile 同边界（TODO #17 P1）。
func (t *editFileTool) Destructive() bool { return true }

// Execute 调用 Executor 的 editFile 方法完成局部替换。
func (t *editFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.editFile(ctx, args)
}

// restoreFileTool 是 RestoreFile 工具的封装。
type restoreFileTool struct{ exec *Executor }

// Name 返回工具标准名称 RestoreFile。
func (t *restoreFileTool) Name() string { return "RestoreFile" }

// Aliases 返回 RestoreFile 的别名列表。
func (t *restoreFileTool) Aliases() []string { return []string{"restore_file", "restoreFile"} }

// Destructive 标记 RestoreFile 为破坏性操作（覆盖目标文件当前内容）：
// 生产工作目录下触发用户确认，与 WriteFile/EditFile 同边界（TODO #17 P1）。
func (t *restoreFileTool) Destructive() bool { return true }

// Execute 调用 Executor 的 restoreFile 方法完成快照恢复。
func (t *restoreFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.restoreFile(ctx, args)
}

// listDirTool 是 ListDir 工具的封装。
type listDirTool struct{ exec *Executor }

// Name 返回工具标准名称 ListDir。
func (t *listDirTool) Name() string { return "ListDir" }

// Aliases 返回 ListDir 的别名列表。
func (t *listDirTool) Aliases() []string { return []string{"list_dir", "listDir"} }

// Execute 调用 Executor 的 listDir 方法完成目录列出。
func (t *listDirTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.listDir(ctx, args)
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
	return t.exec.searchInFiles(ctx, args)
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
	return t.exec.gitDiff(ctx, args)
}

// gitStatusTool 是 GitStatus 工具的封装。
type gitStatusTool struct{ exec *Executor }

// Name 返回工具标准名称 GitStatus。
func (t *gitStatusTool) Name() string { return "GitStatus" }

// Aliases 返回 GitStatus 的别名列表。
func (t *gitStatusTool) Aliases() []string { return []string{"git_status", "gitStatus"} }

// Execute 调用 Executor 的 gitStatus 方法查看状态。
func (t *gitStatusTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitStatus(ctx, args)
}

// gitLogTool 是 GitLog 工具的封装。
type gitLogTool struct{ exec *Executor }

// Name 返回工具标准名称 GitLog。
func (t *gitLogTool) Name() string { return "GitLog" }

// Aliases 返回 GitLog 的别名列表。
func (t *gitLogTool) Aliases() []string { return []string{"git_log", "gitLog"} }

// Execute 调用 Executor 的 gitLog 方法查看提交历史。
func (t *gitLogTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitLog(ctx, args)
}

// gitBlameTool 是 GitBlame 工具的封装。
type gitBlameTool struct{ exec *Executor }

// Name 返回工具标准名称 GitBlame。
func (t *gitBlameTool) Name() string { return "GitBlame" }

// Aliases 返回 GitBlame 的别名列表。
func (t *gitBlameTool) Aliases() []string { return []string{"git_blame", "gitBlame"} }

// Execute 调用 Executor 的 gitBlame 方法查看行级 blame 信息。
func (t *gitBlameTool) Execute(ctx context.Context, args map[string]any) *Result {
	return t.exec.gitBlame(ctx, args)
}
