package bootstrap

// goal.go 会话指令模式（goal 目标 / loop 循环）的装配（2026-10-18）。依赖注入：
// PG 存储（session_directive / goal_reminders）、轻量模型（完成度评估）、
// ReactService（唤醒/通知通道）、tool.Registry（schedule_followup 工具 hook）。
// bootstrap.go 只留接线点。

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/goal"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/store"
)

// buildGoalManager 装配指令模式管理器。
//   - 持久化 = PG（权威全在表，重启恢复 = 扫 due 表）；
//   - 评估器 = ModelFactory.CallLightweightWithRetry（照 workflow/judge.go 模式）；
//   - 唤醒 = ReactService.Send（运行中投邮箱 / 终态懒恢复续跑同一路径）；
//   - 通知 = ReactService.NotifyUserSystemMessage（纯通告，不触发新回合）。
// 存储不可用时返回 nil（端点 503、工具未接线，会话行为不变）。
func buildGoalManager(
	cfg *config.Config,
	pgStore *store.PostgresStore,
	modelFactory *model.ModelFactory,
	toolRegistry *tool.Registry,
	agentSvc *agent.ReactService,
	log *logger.Logger,
) *goal.Manager {
	goalStore, err := goal.NewPGStore(pgStore.DB())
	if err != nil {
		log.Error(nil, "[goal] 存储初始化失败，指令模式不可用", err)
		return nil
	}
	m := goal.NewManager(
		goal.Config{
			PollInterval:    time.Duration(cfg.Goal.PollIntervalSec) * time.Second,
			MinLoopInterval: time.Duration(cfg.Goal.MinLoopIntervalSec) * time.Second,
			JudgeTimeout:    time.Duration(cfg.Goal.JudgeTimeoutSec) * time.Second,
		},
		goalStore,
		modelFactory.CallLightweightWithRetry,
		func(ctx context.Context, sessionID, text string) error {
			return agentSvc.Send(ctx, sessionID, agent.Message{Role: "user", Content: text})
		},
		agentSvc.NotifyUserSystemMessage,
		func(format string, args ...any) { log.Info(nil, fmt.Sprintf(format, args...)) },
	)
	// 回合结束分流（runSession/resumeSession 成功分支回调）。
	agentSvc.SetDirectiveHook(m)
	// schedule_followup 工具接线：Agent 给当前会话自排回查（开店审核数日场景）。
	toolRegistry.SetFollowupHook(func(ctx context.Context, sessionID string, delayMin int, text string) (string, error) {
		return m.AddFollowup(ctx, sessionID, time.Duration(delayMin)*time.Minute, text, "agent")
	})
	return m
}
