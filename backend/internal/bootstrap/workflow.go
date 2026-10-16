package bootstrap

// workflow.go 工作流量产引擎的装配与轨迹适配（2026-10-16，开发文档 §2/§5）。
// 依赖注入：*tool.Registry、*model.ModelFactory、embed.Embedder、store（PG）、
// *subagent.Dispatcher（升级薄封装）、ReactService（确认通道/事件流）。
// bootstrap.go 只留接线点（构造/注册工具/Start/Stop/Handler），细节集中在此。

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/internal/workflow"
)

// buildWorkflowEngine 装配工作流量产引擎。
//   - 持久化 = PG（workflow_runs / workflow_units，权威全在表）；
//   - 生成工具面 = toolRegistry（程序化 Dispatch，ctx 带 session/workdir）；
//   - 评委/参数化 = ModelFactory（CallLightweightWithRetry）；
//   - 漂移检测 = embedder（可缺省，缺省降级跳过）；
//   - 升级通道 = Dispatcher.DispatchProgrammatic（真子 Agent，进 Agent 树）；
//   - 确认通道 = ReactService.WorkflowConfirmHook（askUser 槽位，预算/草案确认卡）；
//   - 事件流 = ReactService.ForwardLiveEvent（kind=workflow_unit 落 session_events）；
//   - 终态外呼 = Notifier.NotifyRunFinal（webhook，nil 安全）。
func buildWorkflowEngine(
	cfg *config.Config,
	pgStore *store.PostgresStore,
	toolRegistry *tool.Registry,
	modelFactory *model.ModelFactory,
	embedder embed.Embedder,
	dispatcher *subagent.Dispatcher,
	svc *agent.ReactService,
	notifier *agent.Notifier,
	log *logger.Logger,
) *workflow.Engine {
	eng := workflow.NewEngine(workflow.Config{
		MaxActiveRuns:      cfg.Workflow.MaxActiveRuns,
		DefaultConcurrency: cfg.Workflow.DefaultConcurrency,
		BudgetCapDefault:   cfg.Workflow.BudgetCapDefault,
		JudgeTimeout:       time.Duration(cfg.Workflow.JudgeTimeoutMS) * time.Millisecond,
	}, workflow.NewPGStore(pgStore.DB()), toolRegistry, modelFactory)
	eng.SetLogger(log)
	if embedder != nil {
		eng.WithEmbedder(embedder)
	}
	if dispatcher != nil {
		eng.WithEscalator(dispatcher)
	}
	eng.WithTraceLoader(&workflowTraceLoader{db: pgStore.DB()})
	if svc != nil {
		eng.SetConfirmHook(svc.WorkflowConfirmHook())
		eng.WithEmitter(func(sessionID, text, detailJSON string) {
			svc.ForwardLiveEvent(sessionID, agent.LiveEvent{
				Kind:    agent.LiveEventWorkflowUnit,
				Agent:   "Workflow",
				Text:    text,
				Detail:  detailJSON,
				Success: true,
			})
		})
	}
	eng.WithRunFinalHook(func(run *workflow.Run) {
		// Notifier 为 nil（webhook 未配置）时方法本身 nil 安全。
		notifier.NotifyRunFinal(run.ID, run.SessionID, run.Status, run.RecipeID, run.PauseReason)
	})
	return eng
}

// workflowTraceLoader 从 agent_messages 读主对话完整历史，抽取生成调用轨迹
// （workflow_freeze 的数据源）：assistant 消息的 tool_calls（工具名 + 入参 map）+
// 配对 tool 消息的结果（ToolResultJSON：success/output）。workflow 包不得 import agent，
// 在此适配成 workflow.TraceCall。
type workflowTraceLoader struct{ db *sql.DB }

// LoadTrace 实现 workflow.TraceLoader。
func (l *workflowTraceLoader) LoadTrace(ctx context.Context, sessionID string) ([]workflow.TraceCall, error) {
	msgs, err := agent.NewPostgresMessagesStore(l.db).LoadMessages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// pending：tool_call_id → 轨迹下标（tool 结果回填用）。
	pending := map[string][]int{}
	var out []workflow.TraceCall
	seq := 0
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			seq++
			out = append(out, workflow.TraceCall{Tool: tc.Name, Args: tc.Input, Seq: seq})
			if tc.ID != "" {
				pending[tc.ID] = append(pending[tc.ID], len(out)-1)
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			for _, idx := range pending[m.ToolCallID] {
				out[idx].Output = m.Content
				out[idx].Success = traceToolSuccess(m.Content)
			}
			delete(pending, m.ToolCallID)
		}
	}
	// 未配对结果的调用按"无结果"处理（Success 默认 false，freeze 会跳过）。
	return out, nil
}

// traceToolSuccess 解析工具结果消息（ToolResultJSON）的 success 字段；解析失败按错误计。
func traceToolSuccess(content string) bool {
	t := strings.TrimSpace(content)
	start := strings.Index(t, "{")
	end := strings.LastIndex(t, "}")
	if start < 0 || end <= start {
		return false
	}
	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(t[start:end+1]), &payload); err != nil {
		return false
	}
	return payload.Success
}
