package graph

import (
	"context"
	"log"
	"time"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)


// HistoryEntry 跨会话历史摘要。
// 与 store.SessionHistoryRecord 解耦，避免 graph 反向依赖 store 包。
type HistoryEntry struct {
	SessionID   string              // 历史会话ID
	Goal        string              // 历史会话目标
	Summary     string              // 历史会话结果摘要
	ToolResults []map[string]any    // 历史会话的工具调用结果（含 tool/path 等）
	CreatedAt   time.Time           // 历史会话创建时间
}

// HistoryStore 跨会话历史读取接口。
// 由 store 包实现并注入，graph 只依赖此接口避免反向依赖。
type HistoryStore interface {
	// RecentSessionHistories 返回最近 limit 条会话历史。
	RecentSessionHistories(ctx context.Context, limit int) ([]HistoryEntry, error)
}

// MetaAgentNode Layer 1: 主Agent / 会话调度器。
//
// 职责：
//   - 首次启动时分析用户目标、拆分领域、创建 DomainAgent 与 TaskBoard
//   - 每个 tick 跑 Watchdog 监控上下文规模、跑邮箱分发跨域消息
//   - 处理会话块事件（跨域请求/升级）
//   - 切换会话块、汇总结果、生成最终回答
//   - 定期更新会话摘要
//
// 并发安全：节点字段在构造后只读（stepCount 仅由 Invoke 串行递增）；
// 实例状态由 registry 内部锁保护。
type MetaAgentNode struct {
	name            string                // 节点名（固定 "MetaAgent"）
	registry        *RoleRegistry         // 角色注册表
	factory         *RoleFactory          // 动态角色工厂（创建 DomainAgent）
	modelFactory    *model.ModelFactory   // 模型工厂，取 Meta 模型
	llmTracker      *model.LLMCallTracker // LLM 调用追踪器（统计超时/Token）
	maxBlocks       int                   // 单会话最大并发块数
	summaryInterval int                   // 会话摘要更新间隔（步数）
	stepCount       int                   // 当前会话已执行步数
	rt              *runtime.Runtime      // Runtime 聚合体（板/邮箱/Watchdog/人格）
	history         HistoryStore          // 跨会话历史读取器
	progress        ProgressCallback      // 进度回调（推思考/意图/Token）
	toolCallback    ToolCallback          // 工具执行结果回调（推 UI），RouteDirectTool/Assistant 直接执行时用
	archiveStore    DomainArchiveStore    // domainAgent 归档存储（特性4：跨会话复用与清理）
	blockMemory     BlockMemoryStore      // 块记忆存储（特性3：switchToNextBlock 幂等兜底归档用）
	compressor      EpisodeCompressor     // Episode 压缩器（Watchdog 触发压缩时调用）
}

// NewMetaAgentNode 创建主Agent节点。
//
// 参数：
//   - registry：角色注册表
//   - factory：角色工厂
//   - maxBlocks：单会话最大并发块数
//   - summaryInterval：会话摘要更新间隔（步数，0 表示不定期更新）
//
// 返回：装配好的节点；modelFactory/runtime/history/progress 通过 Set* 后置注入。
func NewMetaAgentNode(registry *RoleRegistry, factory *RoleFactory, maxBlocks, summaryInterval int) *MetaAgentNode {
	return &MetaAgentNode{
		name:            "MetaAgent",                     // 节点名固定
		registry:        registry,                         // 注入注册表
		factory:         factory,                          // 注入工厂
		maxBlocks:       maxBlocks,                        // 最大并发块数
		summaryInterval: summaryInterval,                  // 摘要更新间隔
		stepCount:       0,                                // 步数清零
		llmTracker:      model.NewLLMCallTracker(),        // 新建 LLM 调用追踪器
	}
}

// SetModelFactory 设置模型工厂。
// 由图构建器在 Build 阶段注入。
func (n *MetaAgentNode) SetModelFactory(mf *model.ModelFactory) {
	n.modelFactory = mf
}

// SetToolCallback 设置工具执行结果回调。
// RouteDirectTool / RouteDirectAssistant 路径下 MetaAgent 直接跑工具循环时用。
func (n *MetaAgentNode) SetToolCallback(cb ToolCallback) {
	n.toolCallback = cb
}

// SetBlockMemoryStore 注入块记忆存储（switchToNextBlock 幂等兜底归档用，TODO #4）。
func (n *MetaAgentNode) SetBlockMemoryStore(s BlockMemoryStore) {
	n.blockMemory = s
}

// SetRuntime 注入运行时（看板/邮箱/Watchdog/人格）。
// 由图构建器注入；nil 时看板/邮箱/Watchdog/人格能力退化。
func (n *MetaAgentNode) SetRuntime(rt *runtime.Runtime) {
	n.rt = rt
}

// SetHistoryStore 注入跨会话历史读取器。
// 用于 handleInitial 加载"上次做过什么"，支持指代类问题（"那个文件在哪"）。
func (n *MetaAgentNode) SetHistoryStore(h HistoryStore) {
	n.history = h
}

// SetArchiveStore 注入 domainAgent 归档存储（特性4）。
// nil 时跳过归档清理与复用检索。
func (n *MetaAgentNode) SetArchiveStore(s DomainArchiveStore) {
	n.archiveStore = s
}

// SetEpisodeCompressor 注入 Episode 压缩器。
// nil 时 Watchdog 仅推送建议压缩提示，不执行压缩。
func (n *MetaAgentNode) SetEpisodeCompressor(c EpisodeCompressor) {
	n.compressor = c
}

// SetProgressCallback 注入进度回调。
// 用于推送思考/意图/LLM 调用/Token 消耗等事件到 UI。
func (n *MetaAgentNode) SetProgressCallback(cb ProgressCallback) {
	n.progress = cb
}

// emit 推送进度事件（nil 回调时无操作）。
//
// 参数：
//   - ctx：请求上下文（用于提取 SessionID）
//   - kind：事件类型
//   - message：事件摘要
func (n *MetaAgentNode) emit(ctx context.Context, kind, message string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 推送事件，Agent 名固定为 "MetaAgent"
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: "MetaAgent", Message: message})
}

// emitDetail 推送带详情的进度事件。
// 与 emit 的区别：附带 detail 字段，用于展示 Prompt 全文/Token 明细等调试信息。
func (n *MetaAgentNode) emitDetail(ctx context.Context, kind, message, detail string) {
	// 未注入回调则直接返回
	if n.progress == nil {
		return
	}
	// 推送带 detail 的事件
	n.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: "MetaAgent", Message: message, Detail: detail})
}

// Runtime 暴露运行时。
// 其他节点（如动态构造的 DomainAgent）通过此方法获取聚合 Runtime，
// 避免在图构建器里重复传递各组件指针。
func (n *MetaAgentNode) Runtime() *runtime.Runtime { return n.rt }

// TimeoutStats 获取超时统计（兼容原接口）。
// 返回值：callCount 调用次数 / timeoutCount 超时次数 / avgDur 平均耗时 / maxDur 最大耗时。
func (n *MetaAgentNode) TimeoutStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	return n.llmTracker.Stats()
}

// LLMTracker 暴露 LLM 调用追踪器。
// 供外部（如 SessionManager）读取详细统计与最近调用记录。
func (n *MetaAgentNode) LLMTracker() *model.LLMCallTracker {
	return n.llmTracker
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口。
func (n *MetaAgentNode) Name() string {
	return n.name
}

// Invoke 执行主Agent逻辑。
//
// 职责：
//   - 递增步数；按 summaryInterval 定期更新会话摘要
//   - 处理用户指令队列（特性6：抢占中断 / 队列注入）
//   - 清理过期实例
//   - 跑 Watchdog 监控上下文规模
//   - 跑邮箱分发跨域广播
//   - 按当前状态分派：首次启动 / 块完成 / 调用中 / 事件待处理 / 切换下一块
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改）
//
// 返回：更新后的 state。
//
// 副作用：修改 state.NextAction/TargetRoleID/ActiveBlocks/SessionSummary 等。
func (n *MetaAgentNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 递增步数
	n.stepCount++

	// 1.5 特性6：处理用户指令队列（抢占中断 / 队列注入）
	if n.rt != nil && n.rt.CmdQueue != nil {
		if stopped := n.drainCommandQueue(ctx, state); stopped {
			// 抢占中断已重置 state，让循环回到 handleInitial 重新起步
			return state, nil
		}
	}

	// 2. 按间隔更新会话摘要（避免每步都跑 LLM 摘要）
	if n.summaryInterval > 0 && n.stepCount%n.summaryInterval == 0 {
		n.updateSessionSummary(state)
	}

	// 3. 清理过期实例（registry 内部按 TTL 回收）
	n.registry.CleanupExpired()

	// 3.5 特性4：定期清理过期 domainAgent 归档（每 50 tick 跑一次）
	// 频率取 50 tick 是权衡：太频繁会反复扫表，太稀疏会让过期记录占据 pgvector 索引
	if n.stepCount%50 == 0 && n.archiveStore != nil {
		go func() {
			// 独立 ctx：清理不阻塞主路径，超时 5s 防止异常长 SQL
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if n, err := n.archiveStore.CleanupExpiredDomainArchives(bgCtx); err == nil && n > 0 {
				// 日志即可，不阻塞主路径
				log.Printf("[MetaAgent] 清理了 %d 个过期领域归档", n)
			}
		}()
	}

	// 4. Watchdog: 监控当前活跃 Agent 的上下文规模（v3 §4.4）
	n.runWatchdog(ctx, state)

	// 5. 邮箱：拉取广播桶里的消息并尝试转交（v3 §7.2）
	n.processMailbox(state)

	// 6. 按当前状态分派
	switch {
	// 首次启动：无活跃块且无当前块 → 进入 handleInitial 拆分领域
	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID == "":
		return n.handleInitial(ctx, state)

	// 所有块已完成：无活跃块但有当前块ID（已被清空）→ 结束会话
	case len(state.ActiveBlocks) == 0 && state.CurrentBlockID != "":
		state.NextAction = enums.ActionFinish
		state.Reason = "all blocks completed"

	// 调用中：当前块存在且调用栈非空 → 继续图循环（让被调用者执行）
	case state.CurrentBlockID != "" && state.IsCalling():
		state.NextAction = enums.ActionContinue

	// 当前块存在且无调用中：处理块事件或切换下一块
	case state.CurrentBlockID != "" && !state.IsCalling():
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil && len(block.Events) > 0 {
			// 有待处理事件 → 交给事件处理器
			return n.handleBlockEvents(ctx, state, block)
		}
		// 无事件 → 切换到下一块
		return n.switchToNextBlock(ctx, state)

	// 兜底：继续图循环
	default:
		state.NextAction = enums.ActionContinue
	}

	return state, nil
}
