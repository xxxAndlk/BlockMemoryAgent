package bootstrap

// decision.go 决策层装配（TODO #23）：provider 组装（llm 兜底 / 外部 HTTP 可插拔）+
// 影子记录器（agent_events type=decision_shadow，零 DDL——type 自由文本）。
//
// 影子行字段映射（MemoryEvent → agent_events）：role=决策点名、content=答案 JSON、
// input=实际路径取值、output=对照结果（match/mismatch/pending）。调用日志走
// session_logs Meta.layer=decision（ModelFactory.logDecisionCall 同款 schema）。
// 晋级机制：影子对拍满 7 天或 ≥200 样本、准确率≥该点阈值后，人工在 config.yaml
// agent.decision_points.<point>.mode 切 enforce（逐点独立晋级，不做全量开关）；
// 统计 SQL 见 doc/变更.md 任务 161 验证段。

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/decision"
	"github.com/blockmemory/agent/backend/internal/model"
)

// decisionShadowRecorder 把影子对拍行写进 agent_events（复用 MemoryPipeline.Write，
// 与 mailbox 留痕同通道同清理语义——随会话删除级联）。best-effort：写失败仅上抛，
// 调用方（decision.Layer.Observe）丢行不惊扰主流程。
type decisionShadowRecorder struct {
	pipeline agent.MemoryPipeline
}

// RecordShadow 实现 decision.ShadowRecorder。
func (r *decisionShadowRecorder) RecordShadow(ctx context.Context, ev decision.ShadowEvent) error {
	if r == nil || r.pipeline == nil || ev.AgentID == "" {
		return nil
	}
	return r.pipeline.Write(ev.AgentID, agent.MemoryEvent{
		Type:     "decision_shadow",
		AgentID:  ev.AgentID,
		Role:     ev.Point,
		Content:  ev.AnswerJSON,
		Input:    ev.Actual,
		Output:   ev.Compare,
		Occurred: time.Now(),
	})
}

// newDecisionLayer 装配决策层。provider 选择：
//   - decision.provider=http 且 http_url 非空 → HTTPProvider（TypeSafe 兼容面），
	//     失败自动回退 LLM 兜底；
//   - 其余（默认 llm）→ LLMProvider（CallDecisionWithRetry：decision_model
//     未配置回退 lightweight，白拿缓存/绑定/热更新/日志）。
//
// 行为参数（影子开关/超时/per-点阈值）从 agent.decision_* 映射。
func newDecisionLayer(cfg *config.Config, factory *model.ModelFactory, pipeline agent.MemoryPipeline) *decision.Layer {
	llm := decision.NewLLMProvider(decision.CallerFunc(factory.CallDecisionWithRetry))
	var provider decision.Provider = llm
	if cfg.Decision.Provider == "http" && cfg.Decision.HTTPURL != "" {
		provider = decision.NewHTTPProvider(
			cfg.Decision.HTTPURL,
			cfg.Decision.HTTPAPIKey,
			time.Duration(cfg.Decision.HTTPTimeoutSec)*time.Second,
			llm,
		)
	}
	opts := decision.Options{
		ShadowEnabled: cfg.Agent.DecisionShadowEnabled == nil || *cfg.Agent.DecisionShadowEnabled,
		Timeout:       time.Duration(cfg.Agent.DecisionTimeoutSec) * time.Second,
		Points:        make(map[string]decision.PointPolicy, len(cfg.Agent.DecisionPoints)),
	}
	for name, p := range cfg.Agent.DecisionPoints {
		opts.Points[name] = decision.PointPolicy{
			Mode:          p.Mode,
			MinConfidence: p.MinConfidence,
			ScoreFloor:    p.ScoreFloor,
		}
	}
	return decision.NewLayer(provider, &decisionShadowRecorder{pipeline: pipeline}, opts)
}
