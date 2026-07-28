package tool

// 导入所需标准库与项目内部包。
import (
	"context"       // context 用于传递上下文与取消信号
	"encoding/json" // encoding/json 解析 SharedEntry 做失效反查
	"fmt"           // fmt 用于格式化错误信息
	"path/filepath" // filepath 用于规范化文件路径
	"strings"       // strings 用于拼接已读文件列表
	"sync"          // sync 提供互斥锁保护并发状态

	"github.com/blockmemory/agent/backend/internal/config" // config 包提供 Agent 阈值配置
	"github.com/go-kratos/blades/tools"                    // blades tools 包提供对外暴露的工具定义
)

// maxConsecutiveFailures 定义单个工具连续失败的最大次数，
// 超过此次数将触发循环退出，避免无限重试。
const maxConsecutiveFailures = 3

// maxReadFilePerTask 已废弃：ReadFile 不再限制不同文件数量，仅拦截同一文件重读。

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
type failureCounter struct {
	// mu 保护 counts 的读写锁，避免并发竞争。
	mu sync.Mutex
	// counts 记录每个工具名称对应的连续失败次数。
	counts map[string]int
}

// newFailureCounter 创建一个新的失败计数器，内部 map 已经初始化。
func newFailureCounter() *failureCounter {
	return &failureCounter{counts: make(map[string]int)}
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

// reset 将指定工具的连续失败次数清零（从 map 中删除）。
func (f *failureCounter) reset(name string) {
	// 加锁保护 counts 的并发修改。
	f.mu.Lock()
	// 函数退出时释放锁。
	defer f.mu.Unlock()
	// 删除该工具的计数记录，表示失败状态已恢复。
	delete(f.counts, name)
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
	// readMu 保护 readFiles map，防止并发读写。
	readMu sync.Mutex
	// readFiles 按 sessionID 记录本任务已读文件路径，用于 ReadFile 预算控制。
	// session 维度隔离：不同会话不分享已读列表；新用户消息进入时 ResetReadHistory 清空。
	readFiles map[string][]string
	// writtenFiles 按 sessionID 记录本任务内 WriteFile 成功写过的路径。
	// 这些路径允许重复 ReadFile：LLM 写完文件后常需重读以验证修改/定位 syntax 错误，
	// 简单的"已读过即拦截"会卡住修复循环。该集合在 WriteFile 成功时写入，readCheck 命中即放行。
	writtenFiles map[string]map[string]bool
	// sharedMemory 是 WriteSharedMemory 工具的 KV 后端，由 bootstrap 注入。
	// 为 nil 时 WriteSharedMemory 注册但不生效，调用返回 store 未配置错误。
	sharedMemory SharedMemoryStore
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
		exec:      exec,
		progress:  progress,
		failures:  newFailureCounter(),
		tools:     make(map[string]Tool),
		aliases:   make(map[string]string),
		readFiles:     make(map[string][]string),
		writtenFiles: make(map[string]map[string]bool),
	}
	// 注册系统内置的默认工具列表。
	r.registerDefaults()
	// 注册 WriteSharedMemory 工具；store 在 SetSharedMemory 注入后生效。
	// 注册始终发生，使 Schema 中可见；调用时若 store 未注入返回错误。
	r.Register(&writeSharedMemoryTool{})
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

// SetProgressCallback 在构造完成后替换进度回调函数。
// 这允许引导流程先创建注册表，随后由 agent service 注入自身回调。
func (r *Registry) SetProgressCallback(cb ProgressCallback) {
	// 直接覆盖注册表中的 progress 字段。
	r.progress = cb
}

// SetSharedMemory 注入 WriteSharedMemory 工具的 KV 后端。
// bootstrap 在创建 sharedKV 后调用；为 nil 时 WriteSharedMemory 调用返回未配置错误。
// 同时把 store 写入已注册的 writeSharedMemoryTool 实例，使其立即可用。
func (r *Registry) SetSharedMemory(store SharedMemoryStore) {
	r.sharedMemory = store
	if t, ok := r.tools["WriteSharedMemory"].(*writeSharedMemoryTool); ok {
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

	// ReadFile 预算控制：防止同一任务中重复读取或读取过多文件。
	if name == "ReadFile" {
		// 从参数中读取目标文件路径。
		path, _ := args["path"].(string)
		// 将路径解析为绝对路径，便于后续统一比较。
		normalizedPath := r.exec.resolvePath(path)
		// 检查当前读取是否被预算规则拦截。
		if blocked := r.checkReadFileBudget(ctx, normalizedPath); blocked != "" {
			// 被拦截时构造一个带有错误信息的结果。
			result := &Result{Tool: "ReadFile", Path: normalizedPath, Error: blocked}
			// 填充 SessionID、ArgsJSON 等通用字段。
			r.fillResult(ctx, result, args)
			// 不再 LoopExit：重复读只返回错误提示，让模型改用 SearchInFiles 或继续其他路径。
			// LoopExit 会直接终止 ReAct 循环，误伤正常多任务流（如修 bug 时需重读已改文件）。
			// 发送结果事件后返回。
			r.emitResult(ctx, result)
			return result, nil
		}
	}

	// 确保上下文中携带 SessionID，供后续结果填充和日志关联。
	ctx = WithSessionID(ctx, SessionIDFromContext(ctx))
	// 调用工具实现获取执行结果。
	result := t.Execute(ctx, args)

	// ReadFile 成功读取后，记录已读文件并在输出末尾追加已读清单提示。
	if name == "ReadFile" && result.Success && result.Path != "" {
		// 将本次成功读取的文件路径加入 session 级任务记录。
		r.recordReadFile(ctx, result.Path)
		// 生成已读文件清单提示文本。
		if hint := r.readListHint(ctx); hint != "" {
			// 将提示追加到结果输出中，提醒模型不要重复读取。
			result.Output = result.Output + "\n" + hint
		}
	}

	// WriteFile 成功后失效引用该 path 的共享记忆 entry（Layer 2 缓存一致性）。
	// 防止子 Agent 改文件后，父 Agent 下次派发仍把旧摘要注入新子 Agent task 导致幻觉。
	// 同时清掉该 path 的已读记录：文件已被改写，旧 ReadFile 缓存的 tool_result 不再新鲜，
	// 必须允许后续 ReadFile 重读，否则 LLM 只能看到 history 里的旧内容（脏数据）。
	if name == "WriteFile" && result.Success && result.Path != "" {
		r.invalidateSharedMemoryForPath(ctx, result.Path)
		r.clearReadHistoryForPath(ctx, result.Path)
		r.recordWrittenFile(ctx, result.Path)
	}

	// 填充结果的通用字段，如 SessionID、ArgsJSON 以及执行器回调。
	r.fillResult(ctx, result, args)

	// 根据执行成功与否更新失败计数。
	if result.Success {
		// 成功则重置该工具的连续失败计数。
		r.failures.reset(name)
	} else {
		// 失败则累加连续失败计数。
		n := r.failures.fail(name)
		// 达到阈值时通知 blades 退出循环，避免无效重试。
		if n >= maxConsecutiveFailures {
			if tc, ok := tools.FromContext(ctx); ok {
				tc.SetAction(tools.ActionLoopExit, true)
			}
		}
	}

	// 发送工具执行结果进度事件。
	r.emitResult(ctx, result)
	// 返回执行结果和错误（工具内部错误已封装在 result 中，此处 error 通常为 nil）。
	return result, nil
}

// invalidateSharedMemoryForPath 遍历 sharedKV，删除引用指定 path 的 entry（Layer 2 缓存一致性）。
// 在 WriteFile 成功后调用，防止子 Agent 改文件后父 Agent 下次派发仍注入旧摘要。
// 失败静默（仅影响缓存，不影响 WriteFile 主路径）；path 规范化为绝对路径比较。
// 旧格式 value（非 JSON）无法判断引用关系，保留不删，由 Layer 3 stat 校验兜底。
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
		var entry SharedEntry
		if json.Unmarshal([]byte(val), &entry) != nil {
			// 旧格式（纯字符串 content）：无法判断引用关系，保留。
			continue
		}
		for fp := range entry.Files {
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

// checkReadFileBudget 检查指定路径是否允许在当前 session 任务中读取。
// 返回空字符串表示允许；否则返回拦截原因。
// sessionID 从 ctx 取；空 sessionID 时放行（无隔离维度无法判重）。
func (r *Registry) checkReadFileBudget(ctx context.Context, path string) string {
	// 空路径无需拦截，直接放行。
	if path == "" {
		return ""
	}
	// 取 sessionID；为空时无法判重，放行。
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return ""
	}
	// 规范化路径，用于统一比较。
	cleanPath := filepath.Clean(path)
	// 加锁保护 readFiles 的读取。
	r.readMu.Lock()
	// 函数退出时释放锁。
	defer r.readMu.Unlock()
	// 遍历该 session 的已读文件列表，若发现重复路径则拦截。
	for _, p := range r.readFiles[sessionID] {
		if filepath.Clean(p) == cleanPath {
			// 例外：该路径在本 session 内被 WriteFile 写过则放行。
			// LLM 写完文件后常需重读以验证修改或定位 syntax 错误，硬拦截会卡住修复循环。
			if r.writtenFiles[sessionID] != nil && r.writtenFiles[sessionID][cleanPath] {
				return ""
			}
			// 返回中文提示，告知模型已读过并应使用 SearchInFiles 定位。
			return fmt.Sprintf("该文件本任务已读过（%s），禁止重读。复用已返回内容推进任务；如需看其他段落请用 SearchInFiles 精确定位。", cleanPath)
		}
	}
	// 不同文件数量不再设上限；允许读取。
	return ""
}

// recordReadFile 将成功读取的文件路径记录到 session 级列表中。
func (r *Registry) recordReadFile(ctx context.Context, path string) {
	// 取 sessionID；为空时不记录（无隔离维度）。
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}
	// 加锁保护 readFiles 的并发修改。
	r.readMu.Lock()
	// 函数退出时释放锁。
	defer r.readMu.Unlock()
	// 规范化路径后存入列表。
	cleanPath := filepath.Clean(path)
	// 检查列表中是否已存在该路径，避免重复记录。
	for _, p := range r.readFiles[sessionID] {
		if filepath.Clean(p) == cleanPath {
			return
		}
	}
	// 追加到该 session 的已读列表。
	r.readFiles[sessionID] = append(r.readFiles[sessionID], cleanPath)
}

// readListHint 生成当前 session 已读文件清单的提示文本。
func (r *Registry) readListHint(ctx context.Context) string {
	// 取 sessionID；为空时返回空串。
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return ""
	}
	// 加锁读取 readFiles。
	r.readMu.Lock()
	// 函数退出时释放锁。
	defer r.readMu.Unlock()
	files := r.readFiles[sessionID]
	// 没有已读文件时返回空字符串，避免在输出中追加无意义提示。
	if len(files) == 0 {
		return ""
	}
	// 返回格式化的中文提示，包含数量与路径列表。
	return fmt.Sprintf("[已读文件清单 (%d): %s — 禁止重读]",
		len(files), strings.Join(files, ", "))
}

// ResetReadHistory 清空指定 session 的已读文件记录。
// 在新用户消息进入时调用，使重复读限制为单任务级而非整个 session 级。
// 设计意图：原始事故是单任务内反复读同一文件；任务完成后用户提新需求（如修 bug）
// 需重读已改文件，不应被历史记录卡死。
func (r *Registry) ResetReadHistory(sessionID string) {
	if sessionID == "" {
		return
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()
	delete(r.readFiles, sessionID)
	delete(r.writtenFiles, sessionID)
}

// clearReadHistoryForPath 从当前 session 的已读列表中移除指定 path 的记录。
// 用于 WriteFile 成功后：文件已被改写，旧 ReadFile 缓存的 tool_result 不再反映最新内容，
// 必须允许后续 ReadFile 重新读取，否则 LLM 只能看到 history 里的旧内容（脏数据）。
// 仅清当前 session（子 Agent 派发经 WithSessionID 继承同一 sessionID，覆盖主/子 Agent 协作场景）。
func (r *Registry) clearReadHistoryForPath(ctx context.Context, path string) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}
	cleanPath := filepath.Clean(path)
	r.readMu.Lock()
	defer r.readMu.Unlock()
	files := r.readFiles[sessionID]
	if len(files) == 0 {
		return
	}
	out := files[:0]
	for _, p := range files {
		if filepath.Clean(p) != cleanPath {
			out = append(out, p)
		}
	}
	r.readFiles[sessionID] = out
}

// recordWrittenFile 把 path 加入本 session 的已写集合，使后续 ReadFile 跳过"已读过"拦截。
// 用于 WriteFile 成功后：LLM 写完文件常需重读以验证修改或定位 syntax 错误，
// 硬拦截会卡住修复循环。集合在 session 维度隔离，ResetReadHistory 一并清空。
func (r *Registry) recordWrittenFile(ctx context.Context, path string) {
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return
	}
	cleanPath := filepath.Clean(path)
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.writtenFiles[sessionID] == nil {
		r.writtenFiles[sessionID] = make(map[string]bool)
	}
	r.writtenFiles[sessionID][cleanPath] = true
}


// 供外部框架（如 blades）动态发现和调用工具。
func (r *Registry) Schema() []tools.Tool {
	// 初始化空列表，用于收集所有工具定义。
	var toolsList []tools.Tool
	// 注册 ReadFile 工具：读取文件内容。
	if t, err := tools.NewFunc("ReadFile", "读取文件内容。path 为相对或绝对路径。建议先用 SearchInFiles/ListDir 定位再精读；同一 session 中已读过的文件会返回提示不复读；单次最多 4000 字符，需看其他段落用 SearchInFiles 精确定位。", func(ctx context.Context, in readFileInput) (string, error) {
		// 通过 Dispatch 调用内部 ReadFile 工具，忽略 Dispatch 返回的 error。
		res, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": in.Path})
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
	if t, err := tools.NewFunc("SearchInFiles", "在文件中搜索文本（大小写不敏感）。pattern 为待搜索文本，dir 为起始目录（默认当前工作目录）；适合先定位再精读，返回匹配行及上下文。", func(ctx context.Context, in searchInFilesInput) (string, error) {
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
	// 暴露 call_sub_agent 工具（若已由 subagent.Dispatcher 安装到注册表）。
	// 该工具不在 registerDefaults 中注册，而是启动期由调度器按需安装；
	// 描述文本由工具实现通过 Description() 提供（含当前可调用角色的动态清单）。
	if ct, ok := r.tools["call_sub_agent"]; ok {
		desc := "将子任务派发给指定角色的子 Agent 异步执行。"
		if d, ok := ct.(interface{ Description() string }); ok {
			desc = d.Description()
		}
		if t, err := tools.NewFunc("call_sub_agent", desc, func(ctx context.Context, in callSubAgentInput) (string, error) {
			res, _ := r.Dispatch(ctx, "call_sub_agent", map[string]any{"role_id": in.RoleID, "task": in.Task, "domain": in.Domain, "responsibility": in.Responsibility})
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
