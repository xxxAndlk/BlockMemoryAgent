package workflow

// escalate.go 升级通道（开发文档 §5.6）：unit 重摇全耗尽且配方声明 escalate 段时，
// 派真子 Agent 处理例外——进 Agent 树/编排页/邮箱语义，占 max_concurrent_sub_agents 池。
// 实现走 subagent.Dispatcher.DispatchProgrammatic 薄封装（内部调 dispatchOne，
// 权限/spec 门/RBAC/并发池准入沿用既有语义）。
// v1 升级 Agent 的产物回收放 M3：派发后只把升级 Agent ID 记入 check_result.upgrade，
// unit 仍标 needs_review/failed 由人审。

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// Escalator 是升级通道能力面（*subagent.Dispatcher 满足；测试用假实现）。
type Escalator interface {
	DispatchProgrammatic(ctx context.Context, spec subagent.ProgrammaticSpec) (string, *tool.Result)
}

// UpgradeInfo 是升级记录（落 unit.check_result.upgrade）。
type UpgradeInfo struct {
	AgentID string `json:"agent_id,omitempty"` // 升级出的子 Agent ID（派发成功时）
	Role    string `json:"role,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Task    string `json:"task,omitempty"`
	Error   string `json:"error,omitempty"` // 派发失败原因（best-effort，不阻断）
}

// buildEscalationTask 组装升级 Agent 的自包含任务简报（件/阶段/失败原因/产物/验收口径）。
func buildEscalationTask(recipe *Recipe, item RosterItem, stage *Stage, unit *Unit, workdir string) string {
	var b strings.Builder
	name := item.Name
	if name == "" {
		name = item.ID
	}
	fmt.Fprintf(&b, "工作流量产升级处理：单元「%s（%s）· 阶段 %s」连续 %d 次尝试未通过校验。\n\n", name, item.ID, stage.ID, unit.AttemptNo)
	fmt.Fprintf(&b, "工作区：%s\n", workdir)
	if unit.ArtifactPath != "" {
		fmt.Fprintf(&b, "最近产物：%s（未通过校验，可参考其缺陷）\n", unit.ArtifactPath)
	}
	if unit.RejectReason != "" {
		fmt.Fprintf(&b, "失败原因：%s\n", truncateStr(unit.RejectReason, 500))
	}
	b.WriteString("\n配方不变量：\n")
	if inv := strings.TrimSpace(recipe.Invariants); inv != "" {
		b.WriteString(inv + "\n")
	} else {
		b.WriteString("（未声明）\n")
	}
	deliv := recipe.EffectiveDeliverable(stage)
	fmt.Fprintf(&b, "\n交付契约：type=%s frames=%d cell=%s format=%s path=%s\n",
		deliv.Type, deliv.Frames, deliv.Cell, deliv.Format, deliv.Path)
	if len(recipe.Check.Judge) > 0 {
		b.WriteString("\n质量判定清单：\n")
		for _, it := range recipe.Check.Judge {
			b.WriteString("- " + it + "\n")
		}
	}
	b.WriteString("\n任务：用你可用的一切手段产出一版满足上述契约与判定清单的产物，落盘到交付路径模板展开后的位置，并给出产物路径与做法说明。")
	return b.String()
}

// maybeEscalate 升级通道入口（配方声明 escalate 且接线了 Escalator 才走）。
// best-effort：派发失败只记录，不影响 unit 的 needs_review/failed 终态。
func (e *Engine) maybeEscalate(ctx context.Context, run *Run, recipe *Recipe, item RosterItem, stage *Stage, unit *Unit) {
	if recipe.Escalate == nil || e.escalate == nil {
		return
	}
	info := UpgradeInfo{Role: recipe.Escalate.RoleID, Domain: recipe.Escalate.Domain}
	info.Task = buildEscalationTask(recipe, item, stage, unit, run.Workdir)
	sctx := tool.WithSessionID(ctx, run.SessionID)
	sctx = tool.WithAgentID(sctx, run.SessionID)
	sctx = tool.WithWorkDir(sctx, run.Workdir)
	sctx = tool.WithStopContext(sctx, e.stopCtx)
	agentID, res := e.escalate.DispatchProgrammatic(sctx, subagent.ProgrammaticSpec{
		RoleID:     recipe.Escalate.RoleID,
		Domain:     recipe.Escalate.Domain,
		Task:       info.Task,
		Mode:       "",
		VerifyKind: "none",
	})
	if res != nil && strings.TrimSpace(res.Error) != "" {
		info.Error = res.Error
		e.logError(ctx, fmt.Sprintf("[workflow] unit %s 升级派发失败", unit.ID), fmt.Errorf("%s", res.Error))
	} else {
		info.AgentID = agentID
		e.logInfo(ctx, fmt.Sprintf("[workflow] unit %s 升级为子 Agent %s（角色 %s）", unit.ID, agentID, recipe.Escalate.RoleID))
	}
	// 回写 check_result.upgrade（合并既有结果；乐观迁移 from=[failed,needs_review]）。
	cur, err := e.store.GetUnit(ctx, unit.ID)
	if err != nil {
		return
	}
	cr := decodeCheckResult(cur.CheckResult)
	cr.Upgrade = &info
	crj := MarshalCheckResult(cr)
	if crj == "" {
		return
	}
	if _, err := e.store.TransitionUnit(ctx, unit.ID, []string{UnitStatusFailed, UnitStatusNeedsReview}, cur.Status, UnitPatch{CheckResult: &crj}); err != nil {
		e.logError(ctx, fmt.Sprintf("[workflow] unit %s 升级记录回写失败", unit.ID), err)
	}
}
