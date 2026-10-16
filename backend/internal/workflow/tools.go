package workflow

// tools.go 主 Agent 能力面（开发文档 §7）：workflow_freeze / workflow_run / workflow_status。
// 注册方式仿 subagent.Dispatcher.RegisterCallTool：bootstrap 在 RegisterCallTool 之后调
// RegisterWorkflowTools(r, engine)。meta 角色白名单两处同步（role/registry.go + roles.yaml）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/google/jsonschema-go/jsonschema"
)

// RegisterWorkflowTools 注册工作流三工具到工具注册表。
func RegisterWorkflowTools(r *tool.Registry, eng *Engine) {
	if r == nil || eng == nil {
		return
	}
	r.Register(&workflowFreezeTool{eng: eng})
	r.Register(&workflowRunTool{eng: eng})
	r.Register(&workflowStatusTool{eng: eng})
}

// ---------------------------------------------------------------------------
// workflow_freeze

type workflowFreezeTool struct{ eng *Engine }

func (t *workflowFreezeTool) Name() string      { return "workflow_freeze" }
func (t *workflowFreezeTool) Aliases() []string { return nil }

func (t *workflowFreezeTool) Description() string {
	return "把本会话里已打样确认的生成做法冻结成工作流配方（.bma/workflows/*.yaml）：从会话轨迹" +
		"提取成功且被采纳的生成调用，参数化为不变量 + 变量槽 + 阶段 + 锚点，生成 YAML 草案后" +
		"向用户弹确认卡过目（用户可直接回复修改后的 YAML），确认才落盘。配方=实际被确认过的那次做法，" +
		"不是模型凭空重写。用户说「就按这个标准量产/存成工作流」时调用。"
}

func (t *workflowFreezeTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"title":     {Type: "string", Description: "配方标题（如「异兽动作量产线」）"},
			"recipe_id": {Type: "string", Description: "配方 ID（英文小写下划线；缺省从标题生成）"},
			"source":    {Type: "string", Description: "轨迹来源，目前仅支持 recent_adopted（默认）"},
			"overrides": {Type: "string", Description: "可选覆盖段（YAML/JSON 顶层键），人工修正 deliverable/anchors/invariants/judge 等"},
		},
		Required: []string{"title"},
	}
}

func (t *workflowFreezeTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	title, _ := args["title"].(string)
	recipeID, _ := args["recipe_id"].(string)
	overrides, _ := args["overrides"].(string)
	sid := tool.SessionIDFromContext(ctx)
	workdir := tool.WorkDirFromContext(ctx)
	if strings.TrimSpace(sid) == "" || strings.TrimSpace(workdir) == "" {
		return &tool.Result{Tool: t.Name(), Error: "workflow_freeze 需要会话与工作目录上下文"}
	}
	draftYAML, info, err := t.eng.FreezeDraft(ctx, FreezeSpec{
		SessionID: sid, Workdir: workdir, Title: title, RecipeID: recipeID, Overrides: overrides,
	})
	if err != nil {
		return &tool.Result{Tool: t.Name(), Error: err.Error()}
	}
	// 过目确认（可编辑）：确认卡正文放 YAML 草案原文；用户答复可能是手改 YAML。
	finalYAML := draftYAML
	answer, approved, cerr := t.eng.confirmAsk(ctx, tool.WorkflowConfirmRequest{
		Title: "配方冻结确认",
		Question: fmt.Sprintf("已从本会话打样轨迹提取配方草案（%d 次生成调用 → %d 阶段 / %d 槽位 / 锚点 %s）：\n\n```yaml\n%s```",
			info.SourceCalls, len(info.Stages), info.SlotCount, strings.Join(info.Anchors, ","), draftYAML),
		Options: []tool.WorkflowConfirmOption{
			{ID: "confirm", Label: "确认落盘", Description: "写入 .bma/workflows/" + info.RecipeID + ".yaml"},
			{ID: "reject", Label: "放弃", Description: "不落盘，继续打样"},
		},
		EditHint: "如需修改：直接回复修改后的完整 YAML（以 id: 开头）；回复「确认」按草案落盘。",
	})
	if cerr != nil {
		return &tool.Result{Tool: t.Name(), Error: "确认流程中断: " + cerr.Error()}
	}
	if looksLikeRecipeYAML(answer) {
		finalYAML = answer // 用户手改稿优先
	} else if !approved {
		return &tool.Result{Tool: t.Name(), Success: true, Output: "用户未确认，配方未落盘（继续打样打磨后再说「存成工作流」）。"}
	}
	recipe, err := SaveRecipeYAML(workdir, finalYAML)
	if err != nil {
		return &tool.Result{Tool: t.Name(), Error: "落盘失败（可修正后重试）: " + err.Error()}
	}
	var stageIDs []string
	for _, st := range recipe.Stages {
		stageIDs = append(stageIDs, st.ID)
	}
	notes := ""
	if len(info.Notes) > 0 {
		notes = "\n注意：" + strings.Join(info.Notes, "；")
	}
	return &tool.Result{Tool: t.Name(), Success: true, Output: fmt.Sprintf(
		"配方已落盘：.bma/workflows/%s.yaml（v%d，工具 %s，阶段 [%s]，%d 个锚点）。后续用 workflow_run 按此配方发起量产。%s",
		recipe.ID, recipe.Version, recipe.Tool, strings.Join(stageIDs, ","), len(recipe.Anchors), notes)}
}

// ---------------------------------------------------------------------------
// workflow_run

type workflowRunTool struct{ eng *Engine }

func (t *workflowRunTool) Name() string      { return "workflow_run" }
func (t *workflowRunTool) Aliases() []string { return nil }

func (t *workflowRunTool) Description() string {
	return "按配方发起一次量产运行（名册 × 阶段的有界流水线，异步执行立即返回）。调用前会向用户" +
		"展示估算并等审批确认，确认后才建 run。发起后用 workflow_status 查进度；批量级人审（联系表）" +
		"在工作流页面做。roster 传内联 JSON 数组（一行一件：id/name/element/family/stages 等）或" +
		".bma/workflows/ 下名册文件路径。"
}

func (t *workflowRunTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"recipe_id":   {Type: "string", Description: "配方 ID（.bma/workflows/<id>.yaml）"},
			"roster":      {Type: "string", Description: "名册：内联 JSON 数组字符串（[{\"id\":\"ice_dragon\",\"name\":\"冰霜巨龙\",...}]），或名册文件路径"},
			"budget_cap":  {Type: "integer", Description: "轻量调用次数上限（缺省用配置默认；超限自动暂停，可提额续跑）"},
			"concurrency": {Type: "integer", Description: "件间并发（缺省配方 params.concurrency / 配置默认 2）"},
			"fail_fast":   {Type: "boolean", Description: "早期熔断（默认 true：前 5 个完成单元中失败 ≥3 即停 run 回打样期）"},
		},
		Required: []string{"recipe_id", "roster"},
	}
}

func (t *workflowRunTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	sid := tool.SessionIDFromContext(ctx)
	workdir := tool.WorkDirFromContext(ctx)
	if strings.TrimSpace(sid) == "" || strings.TrimSpace(workdir) == "" {
		return &tool.Result{Tool: t.Name(), Error: "workflow_run 需要会话与工作目录上下文"}
	}
	recipeID, _ := args["recipe_id"].(string)
	roster, _ := args["roster"].(string)
	spec := CreateSpec{
		SessionID: sid, Workdir: workdir,
		RecipeID: strings.TrimSpace(recipeID), RosterRaw: roster,
		RoleID: tool.RoleIDFromContext(ctx),
	}
	if v, ok := numberArg(args, "budget_cap"); ok {
		spec.BudgetCap = v
	}
	if v, ok := numberArg(args, "concurrency"); ok {
		spec.Concurrency = v
	}
	if v, ok := args["fail_fast"].(bool); ok {
		spec.FailFast = &v
	}
	recipe, _, est, err := t.eng.EstimateRun(spec)
	if err != nil {
		return &tool.Result{Tool: t.Name(), Error: "估算失败: " + err.Error()}
	}
	// 审批门（估算 + cap + 件数），确认才建 run。
	_, approved, cerr := t.eng.confirmAsk(ctx, tool.WorkflowConfirmRequest{
		Title: "工作流量产审批",
		Question: fmt.Sprintf("即将发起量产：\n- 配方：%s v%d（%s）\n- 名册：%d 件 × 阶段 = %d 个执行单元\n- 估算轻量调用 ≤ %d 次（生成 ≤%d + 评委 ≤%d，含重摇）\n- 预算上限：%d 次（超限自动暂停，可提额续跑）\n- 熔断：前 5 个单元失败 ≥3 自动停（回打样期修配方）\n确认发起？",
			recipe.ID, recipe.Version, recipe.Title, est.ItemCount, est.UnitCount,
			est.EstimatedCalls, est.GenerateCalls, est.JudgeCalls, est.BudgetCap),
		Options: []tool.WorkflowConfirmOption{
			{ID: "confirm", Label: "发起量产", Description: "建 run 并开始批量执行"},
			{ID: "reject", Label: "取消", Description: "不建 run"},
		},
	})
	if cerr != nil {
		return &tool.Result{Tool: t.Name(), Error: "审批流程中断: " + cerr.Error()}
	}
	if !approved {
		return &tool.Result{Tool: t.Name(), Success: true, Output: "用户未确认，量产未发起。可调整名册/预算后再说。"}
	}
	run, est2, err := t.eng.CreateRun(ctx, spec)
	if err != nil {
		return &tool.Result{Tool: t.Name(), Error: "建 run 失败: " + err.Error()}
	}
	return &tool.Result{Tool: t.Name(), Success: true, Output: fmt.Sprintf(
		"量产已发起：run_id=%s（%d 件 × 阶段 = %d 单元，预算上限 %d）。引擎异步执行中——用 workflow_status 查进度（run_id=%s），批量人审在工作流页面。",
		run.ID, est2.ItemCount, est2.UnitCount, run.BudgetCap, run.ID)}
}

// ---------------------------------------------------------------------------
// workflow_status

type workflowStatusTool struct{ eng *Engine }

func (t *workflowStatusTool) Name() string      { return "workflow_status" }
func (t *workflowStatusTool) Aliases() []string { return nil }

func (t *workflowStatusTool) Description() string {
	return "查询工作流量产运行的状态与进度：unit 总数/完成/待审/失败、失败原因聚合、预算消耗。" +
		"run_id 缺省查本会话最近一次运行。run 到达终态（done/failed/aborted）时也用它看摘要。"
}

func (t *workflowStatusTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"run_id": {Type: "string", Description: "运行 ID（wf-...）；缺省查本会话最近一次"},
		},
	}
}

func (t *workflowStatusTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	runID, _ := args["run_id"].(string)
	runID = strings.TrimSpace(runID)
	if runID == "" {
		sid := tool.SessionIDFromContext(ctx)
		runs, err := t.eng.ListRuns(ctx, RunFilter{SessionID: sid, Limit: 1})
		if err != nil || len(runs) == 0 {
			return &tool.Result{Tool: t.Name(), Success: true, Output: "本会话还没有工作流运行。用 workflow_run 发起量产。"}
		}
		runID = runs[0].ID
	}
	sum, err := t.eng.RunSummaryOf(ctx, runID)
	if err != nil {
		return &tool.Result{Tool: t.Name(), Error: "查询失败: " + err.Error()}
	}
	lines := []string{
		fmt.Sprintf("运行 %s：%s%s", sum.RunID, sum.Status, pauseReasonText(sum.PauseReason)),
		fmt.Sprintf("配方 %s v%d；单元 %d 个：完成 %d / 待审 %d / 失败 %d / 跳过 %d / 运行中 %d / 排队 %d",
			sum.RecipeID, sum.RecipeVersion, sum.Total,
			sum.Counts[UnitStatusDone], sum.Counts[UnitStatusNeedsReview], sum.Counts[UnitStatusFailed],
			sum.Counts[UnitStatusSkipped], sum.Counts[UnitStatusRunning], sum.Counts[UnitStatusPending]),
		fmt.Sprintf("预算：已用 %d / 上限 %d", sum.Spent, sum.BudgetCap),
	}
	if len(sum.FailReasons) > 0 {
		lines = append(lines, "主要失败原因：")
		for _, r := range sum.FailReasons {
			lines = append(lines, "  - "+truncateStr(r, 120))
		}
	}
	if sum.Status == RunStatusDone && sum.Counts[UnitStatusNeedsReview] > 0 {
		lines = append(lines, "有待审单元：请在工作流页面用联系表横向对比后 accept / reject / reroll。")
	}
	if sum.PauseReason == PauseBudgetExceeded {
		lines = append(lines, "因预算暂停：在工作流页面提额续跑，或让用户确认后 resume。")
	}
	return &tool.Result{Tool: t.Name(), Success: true, Output: strings.Join(lines, "\n")}
}

// ---------------------------------------------------------------------------
// 小工具

// confirmAsk 走会话层确认回调（未接线时返回错误，工具侧如实提示）。
func (e *Engine) confirmAsk(ctx context.Context, req tool.WorkflowConfirmRequest) (string, bool, error) {
	if e.confirm == nil {
		return "", false, fmt.Errorf("确认通道未接线（bootstrap 需注入 WorkflowConfirmHook）")
	}
	res, err := e.confirm(ctx, req)
	if err != nil {
		return "", false, err
	}
	return res.Answer, res.Approved, nil
}

func pauseReasonText(reason string) string {
	switch reason {
	case PauseBudgetExceeded:
		return "（预算超限暂停）"
	case PauseCircuitBreaker:
		return "（熔断）"
	case PauseUser:
		return "（用户暂停）"
	}
	return ""
}

// numberArg 读数值参数（JSON 数字解出 float64；字符串数字容错）。
func numberArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}
