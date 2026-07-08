package graph

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
)

// llmCallTracker 是 graph 包内部对 LLM 调用追踪器的最小依赖接口。
// 使用接口而非具体 *model.LLMCallTracker，便于单元测试注入 recording/mock tracker。
type llmCallTracker interface {
	CallWithTimeout(ctx context.Context, llm model.LLMClient, prompt, caller string, fastTimeout, slowTimeout time.Duration) (string, error, bool)
	Records() []model.CallRecord
	ShouldSkipLLM() bool
	SetRecordCallback(cb func(context.Context, model.CallRecord))
	RecordCall(ctx context.Context, dur time.Duration, err error, caller, promptSummary, prompt, response string, inputTokens, outputTokens int, timedOut bool)
	StatsString() string
	Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration)
	TokenTotals() (inputTokens, outputTokens int)
	TokenTotalsByAgent(callerSub string) (inputTokens, outputTokens int)
	LayerStatsSnapshot() []model.LayerStats
}

// BaseAgentNode 是 MetaAgentNode / DomainAgentNode / SubDomainAgentNode 的公共依赖与行为基类。
//
// 提取原本在三类节点中重复声明的 registry / factory / modelFactory / progress / logger /
// llmTracker / runtime / toolCallback 字段，以及对应的 Set* / emit / emitDetail /
// sessionLogger 方法。各节点通过嵌入 BaseAgentNode 复用这些能力，同时保留自身专有字段。
type BaseAgentNode struct {
	registry     *RoleRegistry
	factory      *RoleFactory
	modelFactory *model.ModelFactory
	progress     ProgressCallback
	logger       *logger.Logger
	llmTracker   llmCallTracker
	rt           *runtime.Runtime
	toolCallback ToolCallback

	// agentLabel 返回当前节点在进度事件与日志中应展示的 Agent 名称。
	// MetaAgent 返回固定名；DomainAgent / SubDomainAgent 可动态附加领域后缀。
	agentLabel func() string

	// emptySessionLogger 在 sessionID 为空时返回可选的退化 logger。
	// MetaAgent 原先在此场景返回裸 logger（不附加 Agent 标签），可通过设置此钩子保留旧行为；
	// 未设置时保持 Domain/SubDomain 的统一行为：返回 WithAgent(agentLabel)。
	emptySessionLogger func() *logger.Logger
}

// newBaseAgentNode 创建带有独立 LLM 调用追踪器的基类实例。
func newBaseAgentNode() BaseAgentNode {
	return BaseAgentNode{
		llmTracker: model.NewLLMCallTracker(),
	}
}

// SetRegistry 设置角色注册表。
func (b *BaseAgentNode) SetRegistry(r *RoleRegistry) { b.registry = r }

// SetFactory 设置动态角色工厂。
func (b *BaseAgentNode) SetFactory(f *RoleFactory) { b.factory = f }

// SetModelFactory 设置模型工厂。
func (b *BaseAgentNode) SetModelFactory(mf *model.ModelFactory) { b.modelFactory = mf }

// SetProgressCallback 设置进度回调。
func (b *BaseAgentNode) SetProgressCallback(pc ProgressCallback) { b.progress = pc }

// SetLogger 注入结构化日志器，并配置 LLM 调用追踪器的持久化回调。
func (b *BaseAgentNode) SetLogger(l *logger.Logger) {
	b.logger = l
	if l == nil {
		return
	}
	b.llmTracker.SetRecordCallback(func(ctx context.Context, r model.CallRecord) {
		sessionID := SessionIDFromContext(ctx)
		if sessionID == "" {
			return
		}
		level := "info"
		msg := "llm_call"
		if r.Err != nil {
			level = "error"
			msg = "llm_call_error: " + r.Err.Error()
		}
		agent := r.Caller
		if agent == "" && b.agentLabel != nil {
			agent = b.agentLabel()
		}
		l.WithSession(sessionID).WithAgent(agent).WithPhase("llm_call").
			Event(ctx, "llm_call", msg, map[string]any{
				"input_tokens":  r.InputTokens,
				"output_tokens": r.OutputTokens,
				"latency_ms":    int(r.Duration.Milliseconds()),
				"timed_out":     r.TimedOut,
				"prompt":        r.Prompt,
				"response":      r.Response,
				"level":         level,
			})
	})
}

// SetRuntime 注入运行时聚合体。
func (b *BaseAgentNode) SetRuntime(rt *runtime.Runtime) { b.rt = rt }

// SetToolCallback 设置工具执行结果回调。
func (b *BaseAgentNode) SetToolCallback(tc ToolCallback) { b.toolCallback = tc }

// emit 推送进度事件（nil 回调时无操作）。
func (b *BaseAgentNode) emit(ctx context.Context, kind, message string) {
	if b.progress == nil {
		return
	}
	agent := "Agent"
	if b.agentLabel != nil {
		agent = b.agentLabel()
		if agent == "" {
			agent = "Agent"
		}
	}
	b.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message})
}

// emitDetail 推送带详情的进度事件。
func (b *BaseAgentNode) emitDetail(ctx context.Context, kind, message, detail string) {
	if b.progress == nil {
		return
	}
	agent := "Agent"
	if b.agentLabel != nil {
		agent = b.agentLabel()
		if agent == "" {
			agent = "Agent"
		}
	}
	b.progress(ctx, ProgressEvent{SessionID: SessionIDFromContext(ctx), Kind: kind, Agent: agent, Message: message, Detail: detail})
}

// sessionLogger 返回按 session_id 绑定的 Logger；未注入时返回 nil-safe 的退化 logger。
func (b *BaseAgentNode) sessionLogger(ctx context.Context) *logger.Logger {
	if b.logger == nil {
		return logger.New(nil)
	}
	agent := "Agent"
	if b.agentLabel != nil {
		agent = b.agentLabel()
		if agent == "" {
			agent = "Agent"
		}
	}
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		if b.emptySessionLogger != nil {
			return b.emptySessionLogger()
		}
		return b.logger.WithAgent(agent)
	}
	return b.logger.WithSession(sessionID).WithAgent(agent)
}

// Emit 是 emit 的导出包装，用于外部调用点统一推送进度事件。
func (b *BaseAgentNode) Emit(ctx context.Context, kind, message string) {
	b.emit(ctx, kind, message)
}

// EmitDetail 是 emitDetail 的导出包装，用于外部调用点统一推送带详情的进度事件。
func (b *BaseAgentNode) EmitDetail(ctx context.Context, kind, message, detail string) {
	b.emitDetail(ctx, kind, message, detail)
}

// SessionLogger 是 sessionLogger 的导出包装。
func (b *BaseAgentNode) SessionLogger(ctx context.Context) *logger.Logger {
	return b.sessionLogger(ctx)
}

// TrackLLMCall 记录一次 LLM 调用到本节点的 LLM 调用追踪器。
// 主要用于非 llmTracker.CallWithTimeout 路径（如直接 Generate）的手动补录。
func (b *BaseAgentNode) TrackLLMCall(ctx context.Context, caller, prompt, response string, inTokens, outTokens, latencyMs int) {
	if b.llmTracker == nil {
		return
	}
	b.llmTracker.RecordCall(ctx, time.Duration(latencyMs)*time.Millisecond, nil, caller,
		model.SummarizePrompt(prompt, 500), prompt, response, inTokens, outTokens, false)
}
