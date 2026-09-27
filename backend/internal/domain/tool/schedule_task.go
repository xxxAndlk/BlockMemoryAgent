package tool

// schedule_task.go 实现 schedule_task 工具：Agent（meta）直接创建/更新 BMA 自己的
// DAG 定时任务，到点由调度器自动派发会话执行（无人值守，禁 ask_user）。
//
// 背景（2026-09-27）：用户让 Agent "设置每天九点看 AI 新闻的定时任务"，MetaAgent
// 没有对应工具，绕去写了 Python 脚本 + 注册 Windows 计划任务——脱离了 BMA 的
// DAG 调度体系（定时任务页面不可见、无法管理、执行不走 Agent 会话）。本工具补上
// 这个入口；提示词侧同时钉死"定时需求一律用本工具，禁止操作系统级计划任务替代"。
//
// 工具本身只做参数解析与基本校验，持久化/合法性（cron 解析、upsert）由注入的
// hook 完成（bootstrap 接线到 dag.Store，调度器未启用时 hook 返回错误）。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// ScheduleTaskSpec 是 schedule_task 工具传给 hook 的入参。
// ID 为空 = 新建（hook 生成 id）；非空 = 更新已有任务（名称/调度/启用/单节点 goal）。
type ScheduleTaskSpec struct {
	ID      string // ID 已有任务 id（更新时传）；空 = 新建
	Name    string // Name 任务名称
	Cron    string // Cron 调度规则：标准 5 段 cron 或 Ns/Nm/Nh 相对间隔，空 = 仅手动触发
	Goal    string // Goal 任务描述（单节点 goal，到点作为会话任务派发执行）
	Enabled bool   // Enabled 是否启用（缺省 true）
}

// ScheduleTaskHookFunc 是 schedule_task 工具的持久化回调：
// 校验 cron 合法性并 upsert 到 dag_jobs，返回人类可读摘要（任务 id、调度、下次
// 触发时间等），错误直接作为工具错误返回给模型。
type ScheduleTaskHookFunc func(ctx context.Context, spec ScheduleTaskSpec) (string, error)

// scheduleTaskTool 实现 schedule_task 工具；未注入 hook 时返回未配置错误。
type scheduleTaskTool struct {
	hook ScheduleTaskHookFunc
}

// SetScheduleTaskHook 注入 schedule_task 工具的持久化回调。
// bootstrap 在 DAG 存储就绪后调用；nil 时工具返回未配置错误。
func (r *Registry) SetScheduleTaskHook(h ScheduleTaskHookFunc) {
	if r == nil {
		return
	}
	if t, ok := r.tools["schedule_task"].(*scheduleTaskTool); ok {
		t.hook = h
	}
}

// Name 返回工具名称。
func (t *scheduleTaskTool) Name() string { return "schedule_task" }

// Aliases 返回工具别名列表，当前无别名。
func (t *scheduleTaskTool) Aliases() []string { return nil }

// Description 返回 LLM 可见的工具描述。
func (t *scheduleTaskTool) Description() string {
	return "创建/更新 BMA 定时任务（DAG 调度）。凡是「定时/每天/每周/周期性做某事」的" +
		"需求一律用本工具——到点由调度器自动派发会话执行（无人值守），用户可在「定时任务」" +
		"页面查看/暂停/删除/手动触发。禁止使用操作系统计划任务、crontab、sleep 循环或后台" +
		"脚本来替代本工具。创建后把任务 id 与下次触发时间告知用户。" +
		"参数：name 任务名称（必填）；goal 任务描述（必填，到点作为会话任务派发，写清做什么、" +
		"产出放哪）；cron 调度规则：标准 5 段表达式（如 0 9 * * * = 每天 9:00、0 9 * * 1 = " +
		"每周一 9:00）或相对间隔 30m/24h，留空 = 仅手动触发；enabled 是否启用（缺省 true）；" +
		"id 可选：传已有任务 id 则更新其名称/调度/启用/单节点 goal，不传则新建。"
}

// InputSchema 返回入参 JSON Schema。
func (t *scheduleTaskTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":    {Type: "string", Description: "任务名称（如「每日 AI 新闻日报」）"},
			"goal":    {Type: "string", Description: "任务描述：到点作为会话任务派发执行，写清做什么、产出物与落盘位置"},
			"cron":    {Type: "string", Description: "调度规则：标准 5 段 cron（0 9 * * *=每天 9:00）或相对间隔 30m/24h；留空=仅手动触发"},
			"enabled": {Type: "boolean", Description: "是否启用（缺省 true）"},
			"id":      {Type: "string", Description: "已有任务 id：传入则更新该任务，不传则新建"},
		},
		Required: []string{"name", "goal"},
	}
}

// Execute 执行 schedule_task 工具调用：解析参数并委托 hook 持久化。
func (t *scheduleTaskTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.hook == nil {
		return &Result{Tool: "schedule_task", Error: "schedule_task 未接线（会话服务未注入 ScheduleTaskHook）"}
	}
	name, _ := args["name"].(string)
	goal, _ := args["goal"].(string)
	name, goal = strings.TrimSpace(name), strings.TrimSpace(goal)
	if name == "" {
		return &Result{Tool: "schedule_task", Error: "name is required"}
	}
	if goal == "" {
		return &Result{Tool: "schedule_task", Error: "goal is required"}
	}
	cronExpr, _ := args["cron"].(string)
	id, _ := args["id"].(string)
	// enabled 缺省 true（显式传 false 才停用）。
	enabled := true
	if v, ok := args["enabled"].(bool); ok {
		enabled = v
	}
	summary, err := t.hook(ctx, ScheduleTaskSpec{
		ID:      strings.TrimSpace(id),
		Name:    name,
		Cron:    strings.TrimSpace(cronExpr),
		Goal:    goal,
		Enabled: enabled,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return &Result{Tool: "schedule_task", Error: fmt.Sprintf("设置定时任务中断: %v", err)}
		}
		return &Result{Tool: "schedule_task", Error: err.Error()}
	}
	return &Result{Tool: "schedule_task", Success: true, Output: summary}
}
