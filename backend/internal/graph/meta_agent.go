package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/soul"
	"github.com/blockmemory/agent/backend/internal/watchdog"
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
	archiveStore    DomainArchiveStore    // domainAgent 归档存储（特性4：跨会话复用与清理）
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
		state.NextAction = types.ActionFinish
		state.Reason = "all blocks completed"

	// 调用中：当前块存在且调用栈非空 → 继续图循环（让被调用者执行）
	case state.CurrentBlockID != "" && state.IsCalling():
		state.NextAction = types.ActionContinue

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
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// drainCommandQueue 处理用户指令队列（特性6）。
//
// 行为：
//   - IntentInterrupt：清空 ActiveBlocks / CallStack / CurrentBlockID 等上下文，
//     把新指令作为 DomainGoal，置 ActionContinue 让 Invoke 下一 tick 进入 handleInitial。
//     返回 true 表示已重置 state，调用方应立即返回。
//   - IntentEnqueue：把指令作为 user 消息追加到 state.Messages，继续当前任务。
//     返回 false。
//
// 没有队列或队列为空时返回 false。
func (n *MetaAgentNode) drainCommandQueue(ctx context.Context, state *types.ThreeLayerState) bool {
	items := n.rt.CmdQueue.Drain(state.SessionID)
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		switch it.Intent {
		case 1: // cmdqueue.IntentInterrupt
			n.emit(ctx, "intend", "抢占中断：清空当前上下文，按新指令重新启动")
			// 清空图状态，保留 SessionID 与 Messages 中的历史对话
			// 注意：必须重置 ActiveBlocks/CallStack/CurrentBlockID 三件套，
			//       否则下一 tick 仍可能跳进旧 DomainAgent 的子任务路径
			state.ActiveBlocks = make(map[string]*types.SessionBlock)
			state.CompletedBlocks = nil
			state.CallStack = make([]*types.CallRequest, 0)
			state.CurrentBlockID = ""
			state.CurrentDomain = ""
			state.DomainGoal = it.Content // 新指令覆盖原 goal，下一 tick 由 handleInitial 重新拆分
			state.CurrentAssistantID = ""
			state.TargetRoleID = ""
			state.DirectExecute = false
			state.PendingClarify = nil // 同步丢弃旧的澄清请求，避免恢复后误挂起
			state.NextAction = types.ActionContinue
			state.Reason = "interrupted by user"
			// 追加为最新用户消息：LLM 在新 handleInitial 中会读到这条消息作为输入
			state.Messages = append(state.Messages, types.ChatMessage{
				Role: enums.ChatRoleUser, Content: it.Content, Timestamp: time.Now(),
			})
			return true
		default: // IntentEnqueue
			// 仅追加消息，不重置状态；当前 tick 继续，下个 tick 起各 Agent 会读到新消息
			n.emit(ctx, "intend", "队列注入：追加用户指令到当前上下文")
			state.Messages = append(state.Messages, types.ChatMessage{
				Role: enums.ChatRoleUser, Content: it.Content, Timestamp: time.Now(),
			})
		}
	}
	// 多条 enqueue 都处理完不中断，调用方继续原 tick 流程
	return false
}

// runWatchdog 评估当前活跃 Agent 的上下文规模并视情况触发动作。
//
// 职责：
//   - 取当前块的目标 + 任务结果作为粗略上下文规模代理
//   - 调 Watchdog.Check 评估等级
//   - Evict 级别：仅记录警告（不再强制结束会话）
//   - Compress 级别：推送"建议压缩"提示
//   - Warn 级别：静默
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 设计意图：原实现 Evict 级别会注入 EventEscalation，导致会话被强制结束。
// 实际场景下（如读大文件）一次工具输出就可能超硬阈值，强制结束会让
// 写文件等关键动作来不及执行。现在 Evict 仅记录警告事件，不中断会话；
// 真正的上下文压缩留待后续按 LevelCompress 实现。
func (n *MetaAgentNode) runWatchdog(ctx context.Context, state *types.ThreeLayerState) {
	// Runtime 或 Watchdog 缺失则跳过
	if n.rt == nil || n.rt.Watchdog == nil {
		return
	}
	// 取当前活跃块；无块则跳过
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		return
	}
	// 拼接粗略上下文：领域 + 目标 + 各任务结果
	var ctxBuf strings.Builder
	ctxBuf.WriteString(block.Domain) // 写入领域名
	ctxBuf.WriteString("\n")         // 换行
	ctxBuf.WriteString(block.Goal)   // 写入领域目标
	ctxBuf.WriteString("\n")         // 换行
	for k, v := range block.TaskResults {
		ctxBuf.WriteString(k)   // 写入任务名
		ctxBuf.WriteString(": ") // 分隔符
		ctxBuf.WriteString(v)   // 写入任务结果
		ctxBuf.WriteString("\n") // 换行
	}
	// 调 Watchdog 评估
	d := n.rt.Watchdog.Check(state.CurrentBlockID, ctxBuf.String())
	// 按等级处理
	switch d.Level {
	case watchdog.LevelEvict:
		// 不再强制升级结束会话；仅记录警告，让当前任务继续完成。
		n.emitDetail(ctx, "wait",
			fmt.Sprintf("Watchdog 触发 EVICT（上下文 %d tokens 超硬阈值），已降级为警告，不中断会话: %s", d.Tokens, d.Reason), "")
	case watchdog.LevelCompress:
		// 接近软阈值：推送建议压缩提示，并尝试对当前块主 Agent 执行 Episode 压缩
		n.emit(ctx, "think",
			fmt.Sprintf("Watchdog 提示上下文接近软阈值 (%d tokens)，建议后续压缩: %s", d.Tokens, d.Reason))
		if n.compressor != nil {
			if block != nil && len(block.Agents) > 0 {
				for _, agentID := range block.Agents {
					if err := n.compressor.Compress(ctx, agentID, state.SessionID); err == nil {
						n.emit(ctx, "think", fmt.Sprintf("已对 Agent %s 执行 Episode 压缩", agentID))
					}
				}
			}
		}
	case watchdog.LevelWarn:
		// 静默
	}
}

// processMailbox 把广播邮件按目标 domain 转给具体 DomainAgent 实例。
//
// 职责：
//   - 从邮箱拉取所有广播桶消息
//   - 按 payload.target_domain 在活跃块中匹配领域
//   - 调 Mailbox.Forward 转交目标实例
//   - 升级类消息（MsgEscalate）同时注入块事件
//
// 参数：
//   - state：图全局状态（原地修改块的 Events）
//
// 副作用：可能向 block.Events 追加 EventEscalation。
func (n *MetaAgentNode) processMailbox(state *types.ThreeLayerState) {
	// Runtime 或邮箱缺失则跳过
	if n.rt == nil || n.rt.Mailbox == nil {
		return
	}
	// 拉取并清空广播桶
	bcasts := n.rt.Mailbox.DrainBroadcast()
	if len(bcasts) == 0 {
		return
	}
	for _, msg := range bcasts {
		// 取目标领域提示（可选）
		var domainHint string
		if v, ok := msg.Payload["target_domain"].(string); ok {
			domainHint = v
		}
		// 在活跃块里寻找匹配领域
		for _, b := range state.ActiveBlocks {
			// 无 hint 或领域匹配
			if domainHint == "" || b.Domain == domainHint {
				// 块内有 Agent 则转发
				if len(b.Agents) > 0 {
					_ = msg.From // 显式忽略 From（保留语义占位）
					n.rt.Mailbox.Forward(msg.ID, b.Agents[0])
					// 升级类消息：注入块事件，由 handleBlockEvents 处理
					if msg.Type == mailbox.MsgEscalate {
						b.Events = append(b.Events, &types.Event{
							ID:        msg.ID,                  // 事件ID
							Type:      types.EventEscalation,   // 升级事件
							Payload:   map[string]any{"reason": msg.Subject}, // 载荷含原因
							Priority:  msg.Priority,            // 优先级
							CreatedAt: msg.CreatedAt,           // 创建时间
							Status:    types.EventPending,      // 待处理
						})
					}
					break // 一个目标只转发一次
				}
			}
		}
	}
}

// loadHistorySection 读取最近 N 条会话历史，拼成可注入 prompt 的中文段落。
//
// 职责：
//   - 从 HistoryStore 读取最近 5 条历史
//   - 拼成"已知历史"段落，含目标/结果/工具调用路径
//   - 末尾提示 LLM 在用户提到指代词时优先结合历史作答
//
// 参数：
//   - ctx：请求上下文
//
// 返回：拼好的段落；失败/无数据返回空串，不影响主流程。
//
// 副作用：2s 超时读取历史，避免历史查询拖垮主流程。
func (n *MetaAgentNode) loadHistorySection(ctx context.Context) string {
	// 未注入历史读取器则返回空
	if n.history == nil {
		return ""
	}
	// 2s 超时读取，避免历史查询阻塞主流程
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	entries, err := n.history.RecentSessionHistories(hctx, 5)
	if err != nil || len(entries) == 0 {
		return ""
	}
	// 拼接段落
	var b strings.Builder
	b.WriteString("\n已知历史（最近会话，倒序）:\n")
	for i, e := range entries {
		// 每条历史：序号 + 会话ID + 目标 + 结果摘要（截断 300 字）
		b.WriteString(fmt.Sprintf("%d. [%s] 目标: %s\n   结果: %s\n", i+1, e.SessionID, e.Goal, truncateStr(e.Summary, 300)))
		// 列出该会话中有意义的工具调用（特别是 WriteFile/RunCommand 的 path）
		for _, tr := range e.ToolResults {
			tool, _ := tr["tool"].(string) // 取工具名
			path, _ := tr["path"].(string) // 取路径
			// 只展示有 path 的工具调用
			if path == "" {
				continue
			}
			// 输出 "工具 -> 路径" 行
			b.WriteString(fmt.Sprintf("   - %s -> %s\n", tool, path))
		}
	}
	// 末尾提示：指代类问题优先结合历史
	b.WriteString("\n当用户提到指代词（在哪/刚才/上次/那个文件）时，请优先结合上述历史作答或检索。\n")
	return b.String()
}

// truncateStr 把字符串截断到 n 个 rune 并加 "..." 后缀。
// 用于摘要展示，避免过长的 LLM 输出污染 prompt。
// 按 rune 截断而非字节，避免在 UTF-8 多字节字符（如中文，每字 3 字节）中间切断产生无效 UTF-8（H3）。
func truncateStr(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "..."
}

// loadMessagesSection 从 state.Messages 构建对话历史段落，注入 prompt。
//
// 职责：把当前会话的对话消息拼成"对话历史"段落，每条消息截断 300 字。
//
// 参数：
//   - state：图全局状态（取 Messages）
//
// 返回：拼好的段落；无消息返回空串。
func (n *MetaAgentNode) loadMessagesSection(state *types.ThreeLayerState) string {
	// 无消息则返回空
	if len(state.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n对话历史:\n")
	for _, msg := range state.Messages {
		// 每条消息：角色 + 内容（截断 300 字）
		b.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, truncateStr(msg.Content, 300)))
	}
	b.WriteString("\n")
	return b.String()
}

// handleInitial 首次启动处理。
//
// 职责：
//   - 加载跨会话历史与对话历史作为上下文
//   - 简单问题（寒暄/自我介绍）直接调 LLM 回答并结束
//   - 复杂问题调 analyzeDomains 拆分领域
//   - 初始化 TaskBoard，把领域名作为顶层子任务
//   - 为每个领域创建 DomainAgent 实例与 SessionBlock
//   - 切换到第一个块交控制权给 DomainAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 返回：更新后的 state；所有领域创建失败则 ActionFinish。
//
// 副作用：创建 DomainAgent 实例；写入 ActiveBlocks、TaskBoard；更新 SessionSummary。
func (n *MetaAgentNode) handleInitial(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	n.emit(ctx, "think", "分析用户目标，决定是否需要拆分领域")

	// 简单问题直接回答，不拆分
	messagesSection := n.loadMessagesSection(state)

	// 查询/搜索/分析类简单任务：标记 DirectExecute，DomainAgent 将跳过子任务拆解，
	// 直接把 goal 作为单任务交给一个 Assistant（用 HTTPGet/Web 搜索类工具，而非写脚本）
	if n.shouldDirectExecute(state.DomainGoal) {
		state.DirectExecute = true
		n.emit(ctx, "intend", "判定为查询/搜索类任务，直接派发单助手执行（跳过子任务拆解）")
	}

	// 特性7：LLM 智能路由 — 关键词规则未命中时，用 LLM 判断 goal 复杂度，
	// 命中"simple"则直接调 LLM 回答，命中"query"则走 DirectExecute，命中"complex"则派发子 Agent。
	// 仅在规则路径未决出 DirectExecute / simple 且模型可用时触发，避免增加无谓 LLM 调用。
	// 注意：classifier 返回 ""（LLM 不可用/超时/未识别）时不进入任何 case，
	//       直接 fall-through 到下方 analyzeDomains 走原规则路径，保证无回归。
	if !state.DirectExecute && n.modelFactory != nil && !n.isSimpleQuestion(state.DomainGoal) {
		switch n.classifyComplexityLLM(ctx, state.DomainGoal) {
		case "simple":
			// 直接调主 LLM 生成回答并结束会话，跳过领域拆分与子 Agent 派发
			n.emit(ctx, "intend", "LLM 路由判定为简单问题，直接调用 LLM 回答")
			answer, err, timedOut := n.callLLM(ctx, fmt.Sprintf(
				`你是BlockMemoryAgent，一个基于大语言模型的本地AI开发助手。
请直接回答用户的简单问题，保持简洁友好。回答请控制在 2000 字以内，确保核心结论完整。
%s
用户问题：%s

你的回答：`, fmtEnvSection(), state.DomainGoal))
			// LLM 软超时：写入超时提示并 Finish，避免阻塞会话
			if timedOut {
				state.SessionSummary = "LLM调用超时，请稍后重试"
				state.NextAction = types.ActionFinish
				state.Reason = "llm timeout on smart-route simple"
				return state, nil
			}
			// 成功：写回答到 SessionSummary 并 Finish；err!=nil 或空回答时 fall-through 到常规拆分流程
			if err == nil && answer != "" {
				state.SessionSummary = answer
				state.NextAction = types.ActionFinish
				state.Reason = "smart-route direct answer"
				return state, nil
			}
		case "query":
			// 标记 DirectExecute，由下方 handleInitial 流程派发单助手 + 工具执行（HTTPGet 等）
			state.DirectExecute = true
			n.emit(ctx, "intend", "LLM 路由判定为查询类任务，标记 DirectExecute 由单助手执行")
		case "complex":
			// 仅打点，不修改状态；fall-through 后由 analyzeDomains 拆分领域并派发子 Agent
			n.emit(ctx, "intend", "LLM 路由判定为复杂任务，进入领域拆分流程")
		}
	}

	if n.modelFactory != nil && n.isSimpleQuestion(state.DomainGoal) {
		n.emit(ctx, "intend", "判定为简单问题，直接调用 LLM 回答")
		// 调 LLM 回答简单问题（含环境/历史/对话段落）
		answer, err, timedOut := n.callLLM(ctx, fmt.Sprintf(
			`你是BlockMemoryAgent，一个基于大语言模型的本地AI开发助手，使用多Agent智能编排架构。
你可以帮助用户：分析代码、操作文件、执行命令、搜索代码、编写程序等。

请直接回答用户的简单问题，保持简洁友好。回答请控制在 2000 字以内，确保核心结论完整。
%s
%s
用户问题：%s

你的回答：`, fmtEnvSection(), messagesSection, state.DomainGoal))
		// LLM 超时：写入超时提示并结束
		if timedOut {
			state.SessionSummary = "LLM调用超时，请稍后重试" // 写入超时摘要
			state.NextAction = types.ActionFinish           // 结束会话
			state.Reason = "llm timeout on simple question" // 记录原因
			return state, nil
		}
		// LLM 成功：写入回答并结束
		if err == nil && answer != "" {
			state.SessionSummary = answer                      // 写入 LLM 回答
			state.NextAction = types.ActionFinish              // 结束会话
			state.Reason = "direct answer for simple question" // 记录原因
			return state, nil
		}
	}

	// 复杂问题：调 LLM 拆分领域
	domains := n.analyzeDomains(ctx, state)
	if len(domains) == 0 {
		// 无领域返回：若启用人机对话（特性5），向用户请求澄清而非直接结束
		if n.humanClarifyEnabled() {
			// 构造澄清请求：ID 用 SessionID+纳秒时间戳保证唯一；Context 回放原始 goal 便于用户对照
			clr := &types.ClarifyRequest{
				ID:        fmt.Sprintf("clarify_%s_%d", state.SessionID, time.Now().UnixNano()),
				Question:  "无法从目标中识别出可执行的领域，请补充说明你希望完成的具体任务或目标。",
				Context:   fmt.Sprintf("原始目标: %s", state.DomainGoal),
				AgentID:   "MetaAgent",
				CreatedAt: time.Now(),
			}
			// 挂起图循环：PendingClarify 非空 + ActionWait 触发 server 把 session 置 awaiting_clarify
			state.PendingClarify = clr
			state.NextAction = types.ActionWait
			state.Reason = "awaiting human clarification"
			n.emit(ctx, "wait", "已向用户请求澄清: "+clr.Question)
			return state, nil
		}
		// 未启用：推送错误事件，下方继续走 Finish 流程
		n.emit(ctx, "error", "领域分析未返回任何领域，将结束会话")
	} else {
		// 推送拆分结果
		names := make([]string, 0, len(domains))
		for _, d := range domains {
			// 收集领域名用于事件展示
			names = append(names, d.Name)
		}
		n.emit(ctx, "intend", fmt.Sprintf("拆分出 %d 个领域: %s", len(domains), strings.Join(names, ", ")))
	}

	// 初始化 TaskBoard（v3 §7.1）：把领域名作为顶层子任务
	if n.rt != nil && n.rt.Boards != nil {
		// 获取或创建本会话的看板
		bd := n.rt.Boards.GetOrCreate(state.SessionID, state.DomainGoal)
		for _, d := range domains {
			// 每个领域作为顶层子任务
			bd.AddSubTask(d.Name + " - " + d.Goal)
		}
	}

	// 为每个领域创建 DomainAgent 与 SessionBlock
	for _, domain := range domains {
		// 达到最大并发块数则停止
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		n.emit(ctx, "intend", fmt.Sprintf("创建 DomainAgent: %s (目标: %s)", domain.Name, domain.Goal))
		// 二次检查（防御性）
		if len(state.ActiveBlocks) >= n.maxBlocks {
			break
		}
		// 创建 DomainAgent 实例
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, domain.Name, domain.Goal, "")
		if err != nil {
			// 创建失败：打印日志并跳过该领域
			log.Printf("[MetaAgent] 创建领域 Agent %s 失败: %v\n", domain.Name, err)
			continue
		}
		// 推送 Agent 创建调试事件
		roleDef := n.registry.GetRoleDef(inst.RoleDefID) // 取角色定义
		agentName := domain.Name + "负责人"               // 默认名称
		if roleDef != nil {
			// 有角色定义则用其名称
			agentName = roleDef.Name
		}
		n.emitDetail(ctx, "agent_created", fmt.Sprintf("创建 DomainAgent: %s (领域: %s)", agentName, domain.Name),
			fmt.Sprintf("instID=%s roleDefID=%s goal=%s", inst.ID, inst.RoleDefID, domain.Goal))

		// 构造会话块
		block := &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(domain.Name), len(state.ActiveBlocks)), // 块ID（领域名+序号）
			SessionID:   state.SessionID,        // 所属会话
			Domain:      domain.Name,             // 领域名
			Goal:        domain.Goal,             // 领域目标
			Status:      "active",                // 初始状态活跃
			Agents:      []string{inst.ID},       // 关联的 DomainAgent 实例
			Events:      make([]*types.Event, 0), // 事件队列
			TaskResults: make(map[string]string), // 任务结果
		}
		// 写入活跃块表
		state.ActiveBlocks[block.ID] = block
	}

	// 所有领域创建失败，直接结束
	if len(state.ActiveBlocks) == 0 {
		state.NextAction = types.ActionFinish              // 结束会话
		state.Reason = "failed to create any domain agent" // 记录原因
		return state, nil
	}

	// 切换到第一个块（map 迭代顺序不固定，但只取一个）
	for blockID := range state.ActiveBlocks {
		state.CurrentBlockID = blockID       // 设为当前块
		block := state.ActiveBlocks[blockID] // 取块引用
		state.CurrentDomain = block.Domain   // 更新当前领域
		state.DomainGoal = block.Goal        // 更新领域目标
		state.NextAction = types.ActionSwitch // 切换到 DomainAgent
		state.TargetRoleID = block.Agents[0]  // 路由目标
		break                                 // 只取第一个
	}

	// 更新会话摘要
	n.updateSessionSummary(state)
	return state, nil
}

// handleBlockEvents 处理会话块事件。
//
// 职责：遍历块的事件队列，按类型分派：
//   - EventCrossModify：转交跨域请求处理器
//   - EventEscalation：设置 ActionEscalate 并返回
//   - 其他：标记为已完成
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - block：当前会话块
//
// 返回：更新后的 state。
func (n *MetaAgentNode) handleBlockEvents(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock) (*types.ThreeLayerState, error) {
	for _, ev := range block.Events {
		// 跳过非待处理事件
		if ev.Status != types.EventPending {
			continue
		}

		// 按事件类型分派
		switch ev.Type {
		case types.EventCrossModify:
			// 跨域修改请求：交给跨域处理器
			return n.handleCrossDomainRequest(ctx, state, ev)

		case types.EventEscalation:
			// 升级事件：标记完成避免重复处理（H7），设置 ActionEscalate 交 EscalationHandler 仲裁
			ev.Status = types.EventDone                       // 先标记完成，防止 handleBlockEvents 下轮重复触发
			state.NextAction = types.ActionEscalate           // 设置升级动作
			state.Reason = getString(ev.Payload, "reason")    // 记录升级原因
			return state, nil

		default:
			// 未知类型：直接标记完成
			ev.Status = types.EventDone
		}
	}

	// 无待处理事件或已处理完：继续图循环
	state.NextAction = types.ActionContinue
	return state, nil
}

// handleCrossDomainRequest 处理跨领域请求。
//
// 职责：
//   - 从事件载荷取目标领域
//   - 在活跃块中查找匹配领域；找不到则创建新块（受 maxBlocks 限制）
//   - 切换到目标块，交控制权给对应 DomainAgent
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//   - ev：跨域请求事件
//
// 返回：更新后的 state；达到最大块数或创建失败则 ActionEscalate。
func (n *MetaAgentNode) handleCrossDomainRequest(ctx context.Context, state *types.ThreeLayerState, ev *types.Event) (*types.ThreeLayerState, error) {
	// 取目标领域；为空则标记事件完成并继续
	targetDomain := getString(ev.Payload, "target_domain")
	if targetDomain == "" {
		ev.Status = types.EventDone            // 标记事件完成
		state.NextAction = types.ActionContinue // 继续图循环
		return state, nil
	}

	// 在活跃块中查找匹配领域
	var targetBlock *types.SessionBlock
	for _, b := range state.ActiveBlocks {
		// 领域名匹配
		if b.Domain == targetDomain {
			targetBlock = b
			break
		}
	}

	// 未找到目标块：创建新块
	if targetBlock == nil {
		// 达到最大块数：升级处理
		if len(state.ActiveBlocks) >= n.maxBlocks {
			state.NextAction = types.ActionEscalate // 升级处理
			state.Reason = fmt.Sprintf("max blocks reached, cannot create domain %s", targetDomain)
			return state, nil
		}

		// 创建新 DomainAgent 实例
		inst, err := n.factory.CreateDomainAgent(ctx, state.SessionID, targetDomain,
			getString(ev.Payload, "goal"), "")
		if err != nil {
			// 创建失败：升级处理
			state.NextAction = types.ActionEscalate // 升级处理
			state.Reason = fmt.Sprintf("failed to create domain agent: %v", err)
			return state, nil
		}

		// 构造新会话块
		targetBlock = &types.SessionBlock{
			ID:          fmt.Sprintf("block_%s_%d", sanitizeID(targetDomain), len(state.ActiveBlocks)), // 块ID（领域名+序号）
			SessionID:   state.SessionID,        // 所属会话
			Domain:      targetDomain,            // 领域名
			Goal:        getString(ev.Payload, "goal"), // 领域目标
			Status:      "active",                // 初始状态活跃
			Agents:      []string{inst.ID},       // 关联的 DomainAgent 实例
			Events:      make([]*types.Event, 0), // 事件队列
			TaskResults: make(map[string]string), // 任务结果
		}
		// 写入活跃块表
		state.ActiveBlocks[targetBlock.ID] = targetBlock
	}

	// 切换到目标块
	state.CurrentBlockID = targetBlock.ID       // 设为当前块
	state.CurrentDomain = targetBlock.Domain    // 更新当前领域
	state.DomainGoal = targetBlock.Goal         // 更新领域目标
	state.NextAction = types.ActionSwitch       // 切换到 DomainAgent
	state.TargetRoleID = targetBlock.Agents[0]  // 路由目标
	ev.Status = types.EventDone                 // 标记事件已处理

	return state, nil
}

// switchToNextBlock 切换到下一个会话块。
//
// 职责：
//   - 汇总当前块结果到 SessionSummary
//   - 把当前块移入 CompletedBlocks 并从 ActiveBlocks 删除
//   - 切换到下一个活跃块；无活跃块则结束会话
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 返回：更新后的 state；无活跃块时调 finalizeSession 生成最终回答。
func (n *MetaAgentNode) switchToNextBlock(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 收尾当前块
	if state.CurrentBlockID != "" {
		// 汇总当前block的结果到SessionSummary
		block := state.ActiveBlocks[state.CurrentBlockID]
		if block != nil {
			n.collectBlockResult(state, block)
		}
		// 移入已完成列表
		state.CompletedBlocks = append(state.CompletedBlocks, state.CurrentBlockID)
		delete(state.ActiveBlocks, state.CurrentBlockID)
	}

	// 2. 切换到下一个活跃块
	for blockID, block := range state.ActiveBlocks {
		state.CurrentBlockID = blockID        // 设为当前块
		state.CurrentDomain = block.Domain    // 更新当前领域
		state.DomainGoal = block.Goal         // 更新领域目标
		state.NextAction = types.ActionSwitch // 切换到 DomainAgent
		state.TargetRoleID = block.Agents[0]  // 路由目标
		return state, nil                     // 只取第一个
	}

	// 3. 所有block完成，生成最终回答
	n.finalizeSession(ctx, state)               // 生成最终回答
	state.CurrentBlockID = ""                   // 清空当前块
	state.CurrentDomain = ""                    // 清空当前领域
	state.DomainGoal = ""                       // 清空领域目标
	state.NextAction = types.ActionFinish       // 结束会话
	state.Reason = "all session blocks completed" // 记录原因
	return state, nil
}

// collectBlockResult 收集block结果到session summary。
//
// 职责：把块的领域名与各任务结果拼成段落，追加到 SessionSummary。
//
// 参数：
//   - state：图全局状态（原地修改 SessionSummary）
//   - block：当前会话块
//
// 副作用：在 SessionSummary 末尾追加段落（用换行分隔）。
func (n *MetaAgentNode) collectBlockResult(state *types.ThreeLayerState, block *types.SessionBlock) {
	var parts []string
	// 段首加领域名标签
	if block.Domain != "" {
		parts = append(parts, fmt.Sprintf("【%s】", block.Domain))
	}
	// 拼接各任务结果
	for task, result := range block.TaskResults {
		// 跳过空结果
		if result != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", task, result))
		}
	}
	// 有内容则追加到 SessionSummary
	if len(parts) > 0 {
		// 已有摘要则先加换行
		if state.SessionSummary != "" {
			state.SessionSummary += "\n"
		}
		// 追加本块结果段落
		state.SessionSummary += strings.Join(parts, "\n")
	}
}

// finalizeSession 会话结束，生成最终回答。
//
// 职责：
//   - SessionSummary 为空时调 updateSessionSummary 生成默认摘要
//   - 有 modelFactory 时调 LLM 把各助手结果润色为最终回答
//   - LLM 超时/失败则保留原始结果
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改 SessionSummary）
//
// 副作用：可能用 LLM 重写 SessionSummary。
func (n *MetaAgentNode) finalizeSession(ctx context.Context, state *types.ThreeLayerState) {
	// 无摘要则生成默认摘要
	if state.SessionSummary == "" {
		n.updateSessionSummary(state)
		return
	}
	// 有模型工厂且未触发 LLM 跳过：调 LLM 润色最终回答
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是BlockMemoryAgent，一个本地AI开发助手。请基于以下各助手的执行结果，生成一个清晰、完整的最终回答给用户。

各助手执行结果：
%s

请直接输出最终回答，不要加任何前缀或总结性语句。回答请控制在 2000 字以内，保留关键结论与必要细节。`, state.SessionSummary))
		// 成功：替换为润色后的回答
		if !timedOut && err == nil && resp != "" {
			state.SessionSummary = resp // 替换为润色后的回答
			return
		}
		// 超时：保留原始结果并打印警告
		if timedOut {
			log.Printf("[MetaAgent] LLM 调用超时(最终汇总阶段)，保留原始结果。%s\n", n.llmTracker.StatsString())
		}
	}
}

// updateSessionSummary 更新会话总结。
//
// 职责：用步数/已完成块/活跃块数/当前领域拼一个简短摘要，写入 SessionSummary。
//
// 参数：
//   - state：图全局状态（原地修改 SessionSummary）
//
// 用途：定期被 Invoke 调用（按 summaryInterval），以及 finalizeSession 的回退路径。
func (n *MetaAgentNode) updateSessionSummary(state *types.ThreeLayerState) {
	var parts []string
	// 拼接各维度信息
	parts = append(parts, fmt.Sprintf("会话[%s]已执行%d步", state.SessionID, n.stepCount)) // 会话ID+步数
	parts = append(parts, fmt.Sprintf("完成领域: %v", state.CompletedBlocks))             // 已完成块列表
	parts = append(parts, fmt.Sprintf("活跃领域: %d个", len(state.ActiveBlocks)))          // 活跃块数量
	if state.CurrentDomain != "" {
		// 有当前领域则追加
		parts = append(parts, fmt.Sprintf("当前领域: %s", state.CurrentDomain))
	}
	// 用分号连接写入 SessionSummary
	state.SessionSummary = strings.Join(parts, "; ")
}

// shouldDirectExecute 判断是否为"查询/搜索/分析"类简单任务，
// 这类任务应跳过 DomainAgent 子任务拆解，直接把 goal 作为单个子任务交给
// 一个 Assistant 用 HTTPGet / Web 搜索类工具完成，而非写脚本。
//
// 判定规则：命中查询/搜索/资讯关键词，且未命中"创建/编写/运行/实现"等
// 明确需要落盘或执行代码的动作词。
func (n *MetaAgentNode) shouldDirectExecute(goal string) bool {
	if goal == "" {
		return false
	}
	goalLower := strings.ToLower(goal)

	// 查询/搜索/资讯类关键词
	queryPatterns := []string{
		"查询", "查一下", "查下", "了解", "获取", "搜集", "收集",
		"搜索", "搜一下", "搜索一下", "检索", "查找",
		"新闻", "资讯", "行情", "股价", "股票", "汇率", "天气",
		"最新", "近期", "最近", "今天", "昨日", "当前",
		"原因", "为什么", "为何", "怎么样", "如何看",
		"分析", "解读", "总结", "汇总",
		"是什么", "什么是", "介绍一下", "解释",
		"news", "search", "query", "lookup", "find", "latest", "recent", "today",
	}
	hitQuery := false
	for _, p := range queryPatterns {
		if strings.Contains(goalLower, p) {
			hitQuery = true
			break
		}
	}
	if !hitQuery {
		return false
	}

	// 明确需要写代码/运行程序的动作词：命中则不视为直接执行类
	actionPatterns := []string{
		"写一个", "写个", "编写", "实现", "开发", "创建一个", "创建个",
		"生成", "制作", "搭建", "部署",
		"运行", "执行", "启动", "跑一下",
		"修改", "重构", "优化代码", "修复",
		"贪吃蛇", "小游戏", "游戏",
	}
	for _, p := range actionPatterns {
		if strings.Contains(goalLower, p) {
			return false
		}
	}
	return true
}

// isSimpleQuestion 判断是否为简单直接问题（仅寒暄/自我介绍类）。
//
// 职责：
//   - 命中指代/历史/操作类关键词 → 非简单问题（需走完整 Graph）
//   - 命中明确寒暄/自我介绍模式 → 简单问题
//   - 极短且纯 ASCII（如 "ping"）→ 简单问题
//   - 中文短句一律不视为简单问题
//
// 参数：
//   - goal：用户目标
//
// 返回：是简单问题返回 true。
//
// 设计意图：之前的实现用 len(goal) < 30 字节判定，对中文极不靠谱——
// "贪吃蛇小游戏在哪" 这种指代类问题（8 汉字 = 24 字节）会被误判成
// 简单问题，直接跳过 Graph 走 LLM 一问一答，既不读历史也不调工具，
// 表现为"Agent 失忆"。
//
// 现在：必须命中明确的寒暄模式，并且不含任何指代/查找/操作词。
func (n *MetaAgentNode) isSimpleQuestion(goal string) bool {
	// 转小写做大小写不敏感匹配
	goalLower := strings.ToLower(goal)

	// 指代/历史/操作类关键词：命中即视为非简单问题，需走完整 Graph
	referencePatterns := []string{
		"在哪", "哪里", "刚才", "上次", "之前", "上次", "之前", "那个",
		"这个", "刚才", "记得", "记忆", "历史", "之前",
		"文件", "代码", "目录", "项目", "找", "查找", "搜索",
		"写", "创建", "修改", "删除", "运行", "执行",
	}
	for _, p := range referencePatterns {
		// 命中任一指代词即视为非简单问题
		if strings.Contains(goalLower, p) {
			return false
		}
	}

	// 仅在命中明确寒暄/自我介绍模式时才视为简单问题。
	// hello/hi/hey 必须作为整词（前后非字母数字）匹配，避免 "创建hello.txt"
	// / "hey-check 工具" 这类实际任务被误判为寒暄。
	simplePatterns := []string{
		"你是什么", "你是谁", "什么模型", "你好",
		"叫什么名字", "介绍自己", "自我介绍", "能做什么", "有什么功能",
	}
	for _, p := range simplePatterns {
		// 命中寒暄模式即视为简单问题
		if strings.Contains(goalLower, p) {
			return true
		}
	}
	// 整句就是英文寒暄词（如 "hello" / "hi there"）
	if isGreetingOnly(goalLower) {
		return true
	}

	// 极短且纯 ASCII（如 "ping"）仍视为简单；中文短句一律不在此列
	if utf8.RuneCountInString(goal) <= 6 && isASCII(goal) {
		return true
	}
	return false
}

// isGreetingOnly 判断 goal 是否整句就是一个英文寒暄词（可带标点/空格）。
//
// 例如 "hello" / "hi there" / "hey!"。
// 避免 "创建hello.txt" 这种实际任务被当作寒暄。
//
// 参数：
//   - goal：用户目标（已转小写）
//
// 返回：是纯寒暄返回 true。
func isGreetingOnly(goal string) bool {
	// 去首尾空白并转小写
	g := strings.TrimSpace(strings.ToLower(goal))
	greetings := []string{"hello", "hi", "hey", "hi there", "hey there"}
	for _, gr := range greetings {
		// 整句匹配
		if g == gr {
			return true
		}
		// 允许尾部标点
		if strings.HasPrefix(g, gr) {
			rest := strings.TrimSpace(g[len(gr):])
			// 尾部为空或全是标点
			if rest == "" || allPunct(rest) {
				return true
			}
		}
	}
	return false
}

// allPunct 判断字符串是否全由标点符号组成。
// 覆盖 ASCII 标点与中文常见标点（！。？，）。
//
// 参数：
//   - s：待判断的字符串
//
// 返回：全标点且非空返回 true。
func allPunct(s string) bool {
	for _, r := range s {
		// 检查是否在 ASCII 标点区间或中文标点
		if !((r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~') ||
			r == '！' || r == '。' || r == '？' || r == '，') {
			return false
		}
	}
	return s != ""
}

// isASCII 判断字符串是否全为 ASCII 字符（码点 <= 127）。
// 用于区分纯英文短句与中文短句。
//
// 参数：
//   - s：待判断的字符串
//
// 返回：全 ASCII 返回 true。
func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// callLLM 统一的LLM调用入口。
// 职责：以 "MetaAgent" 身份调用 callLLMAs。
// 带 自适应超时 + 人格注入 + 温度调节 + Prompt/Token 日志。
func (n *MetaAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	return n.callLLMAs(ctx, "MetaAgent", prompt)
}

// callLLMAs 以指定调用者身份执行 LLM 调用。
//
// 职责：
//   - 取 Meta 模型
//   - 注入人格（soul.md）
//   - 推送 prompt 调试事件
//   - 用 0 温度（路由/总结场景）+ 自适应超时调用 LLM
//   - 推送最近一次调用的 Token 消耗
//
// 参数：
//   - ctx：请求上下文
//   - caller：调用者标识，用于事件展示
//   - prompt：发送给 LLM 的完整 prompt
//
// 返回：(响应文本, 错误, 是否超时)。
//
// 副作用：通过 emitDetail 推送 prompt 与 token_usage 事件。
func (n *MetaAgentNode) callLLMAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	// 取 Meta 模型；失败则直接返回错误
	llm, err := n.modelFactory.GetMetaModel(ctx)
	if err != nil {
		return "", err, false
	}

	// 注入人格（soul.md 内容拼到 prompt 前部）
	if n.rt != nil && n.rt.Soul != nil {
		prompt = n.rt.Soul.Inject(prompt)
	}

	// 发送 prompt 调试事件（含 token 估算与 500 字摘要）
	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// MetaAgent 主要做"路由 / 总结"决策，使用 0 温度
	var resp string       // LLM 响应文本
	var callErr error     // 调用错误
	var timedOut bool     // 是否超时
	// 解析 LLM 软/硬超时：默认 30s/90s，可被 AgentCfg 覆盖（特性2）
	softTimeout := 30 * time.Second
	hardTimeout := 90 * time.Second
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.LLMSoftTimeoutSec > 0 {
			softTimeout = time.Duration(n.rt.AgentCfg.LLMSoftTimeoutSec) * time.Second
		}
		if n.rt.AgentCfg.LLMHardTimeoutSec > 0 {
			hardTimeout = time.Duration(n.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
		}
	}
	// 若 LLM 支持 TemperatureAware，用温度包装器叠加 0 温度
	if t, ok := llm.(model.TemperatureAware); ok {
		desired := soul.Temperature(soul.KindRouting, 0)                          // 路由场景温度
		wrapped := &temperatureWrappedLLM{base: llm, t: t, temperature: desired}  // 包装器
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, wrapped, prompt, caller,
			softTimeout, hardTimeout)
	} else {
		// 不支持温度控制：直接调用
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller,
			softTimeout, hardTimeout)
	}

	// 发送 token_usage 调试事件（从 tracker 最新记录读取）
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1] // 取最新一条记录
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	// 发送 LLM 响应摘要调试事件（500 字截断），便于排查决策依据
	if resp != "" {
		n.emitDetail(ctx, "llm_response",
			fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
			model.SummarizePrompt(resp, 500))
	}

	return resp, callErr, timedOut
}

// temperatureWrappedLLM 在 LLMClient 外层叠加 per-call temperature。
// 设计意图：让不支持运行时改温度的 ChatModel 也能按场景（路由/创作）
// 设置不同温度，而不修改 modelFactory 缓存的实例。
type temperatureWrappedLLM struct {
	base        model.LLMClient         // 被包装的底层客户端
	t           model.TemperatureAware  // 温度感知接口
	temperature float64                 // 本次调用使用的温度
}

// humanClarifyEnabled 返回是否启用人机对话（特性5）。
// 默认 true；可被 AgentCfg.HumanClarifyEnabled 关闭。
func (n *MetaAgentNode) humanClarifyEnabled() bool {
	if n.rt == nil || n.rt.AgentCfg == nil {
		return true // 默认启用
	}
	return n.rt.AgentCfg.HumanClarifyEnabled
}

// classifyComplexityLLM 用 LLM 判断 goal 复杂度，作为关键词规则的补充（特性7）。
//
// 返回值：
//   - "simple"：寒暄/常识/定义类，主 Agent 直接回答即可
//   - "query"  ：查询/搜索/资讯类，单助手 + 工具即可（DirectExecute）
//   - "complex"：需要拆分领域派发多 Agent 协作
//   - ""       ：LLM 不可用或调用失败，调用方应回退到原规则路径
//
// 设计意图：shouldDirectExecute 与 isSimpleQuestion 覆盖典型关键词，
// 但中文表述多变时容易漏判。此处用一次轻量 LLM 调用作兜底，避免把
// "帮我分析下这段代码有什么问题"这类需要 ReAct 的任务误派给单助手。
func (n *MetaAgentNode) classifyComplexityLLM(ctx context.Context, goal string) string {
	// 模型未装配或 llmTracker 判定应跳过（如连续失败熔断中）时回退空串
	if n.modelFactory == nil || n.llmTracker.ShouldSkipLLM() {
		return ""
	}
	// 空目标无法分类，直接回退
	if strings.TrimSpace(goal) == "" {
		return ""
	}
	prompt := fmt.Sprintf(`判断以下用户目标的复杂度，只回答一个单词：simple、query 或 complex。

- simple：寒暄、自我介绍、常识/定义类问题，无需调工具，主 Agent 直接回答即可
- query  ：查询/搜索/资讯/行情类，需要用 HTTPGet 等工具抓取，但单助手即可完成
- complex：需要写代码/创建文件/多步执行/多领域协作，必须拆分领域派发多 Agent

用户目标: %s

只回答 simple / query / complex 三者之一，不要其他文字:`, goal)
	// callLLMAs：以 MetaAgent 身份调用，自带软/硬超时与重试；任一异常都视作未判定
	resp, err, timedOut := n.callLLMAs(ctx, "MetaAgent/复杂度判定", prompt)
	if timedOut || err != nil || resp == "" {
		return ""
	}
	// LLM 可能输出 "simple." 或 "Complex\n" 等变体，统一小写后用 Contains 子串匹配
	resp = strings.ToLower(strings.TrimSpace(resp))
	switch {
	case strings.Contains(resp, "simple"):
		return "simple"
	case strings.Contains(resp, "query"):
		return "query"
	case strings.Contains(resp, "complex"):
		return "complex"
	}
	// 未匹配三者时回退空串，调用方按原规则路径处理
	return ""
}

// Generate 实现 model.LLMClient 接口，转发到带温度选项的生成方法。
func (w *temperatureWrappedLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return w.t.GenerateWithOptions(ctx, prompt, w.temperature)
}

// DomainInfo 领域信息。
// 由 analyzeDomains 产出，描述一个待创建的领域。
type DomainInfo struct {
	Name string // 领域名（简短，2-6 字）
	Goal string // 领域目标
}

// analyzeDomains 分析用户目标，确定需要的领域（优先LLM，回退规则）。
//
// 职责：
//   - 取领域目标（优先 DomainGoal，回退 SessionSummary）
//   - 加载历史与对话段落
//   - 调 LLM 分析领域（输出 JSON 数组）
//   - 解析失败/超时则回退规则
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 返回：领域列表；LLM 不可用或无目标时回退规则。
func (n *MetaAgentNode) analyzeDomains(ctx context.Context, state *types.ThreeLayerState) []DomainInfo {
	// 取目标；DomainGoal 为空则用 SessionSummary
	goal := state.DomainGoal
	if goal == "" {
		goal = state.SessionSummary
	}

	messagesSection := n.loadMessagesSection(state)

	// 尝试使用LLM分析领域
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		n.emit(ctx, "llm", "调用 LLM 进行领域分析...")
		// 构造分析 prompt：要求 JSON 数组，含指代词时优先创建"检索历史与文件"领域
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是一个多Agent系统的领域分析器。请分析以下用户目标，确定需要哪些业务领域来协作完成。

用户目标: %s
%s
%s
要求:
- 每个领域名称简短（2-6个字），禁止使用"通用"作为领域名——必须根据目标语义给出具体领域名（如"AI股票分析"、"贪吃蛇游戏"、"数据库检查"）
- 领域之间应该尽量独立
- 若用户目标涉及"查找/刚才/上次"等指代词，应优先创建一个"检索历史与文件"领域
- 简单查询/搜索类目标只需一个领域即可，不要强行拆分
- 输出JSON数组格式: [{"name":"领域名","goal":"该领域需要完成的目标"}]
- 只输出JSON数组，不要代码块标记，不要任何解释文字

领域列表:`, goal, fmtEnvSection(), messagesSection))
		if !timedOut && err == nil && resp != "" {
			n.emit(ctx, "think", "LLM 返回领域分析结果，正在解析")
			// 解析 JSON 为领域列表
			if domains := n.parseDomainsFromLLM(resp); len(domains) > 0 {
				return domains // 返回 LLM 解析的领域
			}
			// 解析失败：推送详情
			n.emitDetail(ctx, "think", "LLM 返回内容无法解析为领域列表", truncateStr(resp, 200))
		}
		// 超时或失败：推送事件并回退规则
		if timedOut {
			n.emit(ctx, "error", "领域分析 LLM 调用超时，回退到规则")
			log.Printf("[MetaAgent] LLM 调用超时(领域分析阶段)，使用规则兜底。%s\n", n.llmTracker.StatsString())
		} else if err != nil {
			n.emitDetail(ctx, "error", "领域分析 LLM 调用失败: "+err.Error(), "")
		}
	}

	// 回退规则
	n.emit(ctx, "think", "回退到规则方式分析领域")
	return n.analyzeDomainsByRules(goal)
}

// parseDomainsFromLLM 从LLM响应解析领域列表。
//
// 职责：
//   - 用 extractJSON 抽取 JSON 片段
//   - 反序列化为 [{name, goal}] 数组
//   - 过滤空 name 的条目
//
// 参数：
//   - resp：LLM 返回的原始文本
//
// 返回：领域列表；解析失败或为空返回 nil。
func (n *MetaAgentNode) parseDomainsFromLLM(resp string) []DomainInfo {
	// 抽取 JSON 片段（LLM 可能附带多余文本）
	jsonStr := extractJSON(resp)
	// 兜底：若 extractJSON 返回单个对象，包成数组再解析
	trimmed := strings.TrimSpace(jsonStr)
	if strings.HasPrefix(trimmed, "{") {
		jsonStr = "[" + trimmed + "]"
	}
	// 反序列化为匿名结构数组
	var rawDomains []struct {
		Name string `json:"name"` // 领域名
		Goal string `json:"goal"` // 领域目标
	}
	if err := json.Unmarshal([]byte(jsonStr), &rawDomains); err != nil || len(rawDomains) == 0 {
		// 解析失败或为空：返回 nil 触发回退
		return nil
	}

	// 过滤空 name 并转换为 DomainInfo
	var domains []DomainInfo
	for _, d := range rawDomains {
		// 跳过空 name 的条目
		if d.Name != "" {
			domains = append(domains, DomainInfo{Name: d.Name, Goal: d.Goal})
		}
	}
	return domains
}

// analyzeDomainsByRules 基于关键词规则的领域分析。
//
// 职责：LLM 不可用时的回退，按目标关键词匹配预设领域模板。
//
// 参数：
//   - goal：领域目标
//
// 返回：领域列表；无匹配时返回单元素领域（名称从 goal 推断，不再一律"通用"）。
func (n *MetaAgentNode) analyzeDomainsByRules(goal string) []DomainInfo {
	var domains []DomainInfo

	// 按关键词匹配预设领域
	if strings.Contains(goal, "商城") || strings.Contains(goal, "页面") {
		domains = append(domains, DomainInfo{Name: "商城页面", Goal: goal}) // 商城/页面领域
	}
	if strings.Contains(goal, "购物车") || strings.Contains(goal, "购买") {
		domains = append(domains, DomainInfo{Name: "购物模块", Goal: goal}) // 购物模块领域
	}
	if strings.Contains(goal, "订单") {
		domains = append(domains, DomainInfo{Name: "订单模块", Goal: goal}) // 订单模块领域
	}
	if strings.Contains(goal, "用户") || strings.Contains(goal, "登录") {
		domains = append(domains, DomainInfo{Name: "用户模块", Goal: goal}) // 用户模块领域
	}

	// 无匹配：从 goal 截取前若干字符作为领域名，避免一律叫"通用"
	if len(domains) == 0 {
		name := inferDomainName(goal)
		domains = append(domains, DomainInfo{Name: name, Goal: goal}) // 兜底单领域
	}

	return domains
}

// inferDomainName 从 goal 文本启发式推断领域名，避免一律用"通用"。
// 取前若干个有意义的字符（中文按 rune 计，最多 6 字），去除常见动词前缀。
func inferDomainName(goal string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		return "通用"
	}
	// 去掉常见动词前缀，让领域名更贴近主题
	prefixes := []string{"完成", "请", "帮我", "帮助我", "实现", "做", "写", "创建", "查询", "查一下", "查下", "搜索", "搜一下"}
	for _, p := range prefixes {
		if strings.HasPrefix(g, p) {
			g = strings.TrimSpace(strings.TrimPrefix(g, p))
			break
		}
	}
	if g == "" {
		return "通用"
	}
	// 按 rune 截取前 6 个字符
	rs := []rune(g)
	if len(rs) > 6 {
		rs = rs[:6]
	}
	return string(rs)
}
