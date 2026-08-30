package subagent

// plan_confirm.go 计划确认机制：下级 Agent 遇到中大型任务先出计划给上级确认，批准后才执行。
//
// 统一入口 submit_plan(task_summary, plan)，按调用者 agentID 的层级自动路由
// （agentID 形如 "session-N/domain-2/xxx-5"，以 "/" 分隔即父子链）：
//   - 顶层（agentID 无 "/"，即 MetaAgent）→ 上级是用户：复用 ask_user 链路，
//     把计划渲染成问题 + 选项走既有 session 澄清面板（TUI/SSE 零新增适配层）；
//   - 下级（agentID 含 "/"）→ 上级是父 Agent：向 parentID 发邮箱 MsgRequest
//     （Body 含 plan_id + 提交者 + 任务简介 + 计划详情 + 审批指引），随后阻塞等待
//     上级调用 review_plan(plan_id, verdict, feedback) 回传结论；等待期间周期性
//     PingActivity 防心跳巡检误杀（与 ask_user 等待保活同款）。
//
// 结论语义：approve → 按计划执行；reject → 返还 feedback，下级修订后重新提交，
// 循环直到批准为止（未经批准禁止执行；plan_max_revisions>0 时达上限转升级仲裁仍不放行）；
// 等待超时 / 上级不可达 → fail-open 按计划继续（与 ask_user 超时"自行决策"语义一致）。

import (
	"context" // context 用于取消信号传递
	"fmt"     // fmt 用于格式化 plan_id 与结果文案
	"log"     // log 用于记录审批不可达等可观测事件
	"strings" // strings 用于结论关键词判定与层级拆分
	"sync"    // sync 保护 planWaits / revisions 计数
	"time"    // time 用于等待超时与保活 tick

	"github.com/blockmemory/agent/backend/internal/agent" // agent 包提供 AgentIDFromContext
	"github.com/blockmemory/agent/backend/internal/domain/tool" // tool 包提供 Result 类型与校验拒绝类别
	"github.com/blockmemory/agent/backend/internal/mailbox"     // mailbox 包用于下级向上级投递计划审批请求
)

// 计划确认默认值：bootstrap 未注入（WithPlanConfirmation 未调用）或零值时生效。
const (
	defaultPlanConfirmTimeout = 10 * time.Minute // 审批等待超时默认值，超时 fail-open
	planPingInterval          = 30 * time.Second // 等待审批期间的保活 tick 周期
)

// planVerdict 是上级对一份计划的审批结论。
type planVerdict struct {
	approved bool   // true=批准按计划执行；false=驳回需修订
	feedback string // 驳回时的修改意见；批准时为空
}

// planWaiter 是一次提交的等待者：submit_plan 注册后阻塞在 ch 上等 review_plan 投递结论。
type planWaiter struct {
	agentID  string // 提交者 Agent ID
	parentID string // 计划的直接上级（唯一合法审批人）
	ch       chan planVerdict
}

// planConfirmState 是计划确认的会话级状态：等待者注册表 + 每 Agent 驳回计数。
type planConfirmState struct {
	mu        sync.Mutex
	seq       uint64                // plan_id 递增序号
	waits     map[string]*planWaiter // planID -> 等待者
	revisions map[string]int         // agentID -> 已被驳回次数
}

func newPlanConfirmState() *planConfirmState {
	return &planConfirmState{
		waits:     make(map[string]*planWaiter),
		revisions: make(map[string]int),
	}
}

func (s *planConfirmState) register(agentID, parentID string) (string, *planWaiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	planID := fmt.Sprintf("plan-%s-%d", agentID, s.seq)
	w := &planWaiter{agentID: agentID, parentID: parentID, ch: make(chan planVerdict, 1)}
	s.waits[planID] = w
	return planID, w
}

func (s *planConfirmState) remove(planID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.waits, planID)
}

func (s *planConfirmState) get(planID string) (*planWaiter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.waits[planID]
	return w, ok
}

// bumpRevisions 递增并返回该 Agent 的累计驳回次数。
func (s *planConfirmState) bumpRevisions(agentID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions[agentID]++
	return s.revisions[agentID]
}

// submitPlanTool 实现 submit_plan 工具：提交计划并阻塞等待上级（或用户）确认。
type submitPlanTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *submitPlanTool) Name() string { return "submit_plan" }

// Aliases 返回工具别名。
func (t *submitPlanTool) Aliases() []string { return []string{"plan_submit"} }

// Description 返回 LLM 可见的工具描述。
func (t *submitPlanTool) Description() string {
	return "提交执行计划给上级确认（任何任务动手前必调，无大小豁免）：task_summary 为一行任务简介，" +
		"plan 为计划详情（步骤、涉及文件、验收标准）。顶层 Agent 的计划呈给用户确认；" +
		"子 Agent 的计划呈给父 Agent 审批。批准后本调用才返回并放行执行，未经批准禁止动手；" +
		"被驳回时返回上级修改意见，修订后重新提交，循环直到批准为止" +
		"（配置了 plan_max_revisions 上限时，达上限转升级仲裁，仍不放行）。" +
		"等待超时或上级不可达时按计划继续（fail-open）。"
}

// Execute 提交计划：解析层级路由（用户 / 父 Agent），阻塞等待结论。
func (t *submitPlanTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	agentID := agent.AgentIDFromContext(ctx)
	if agentID == "" {
		return &tool.Result{Tool: "submit_plan", Error: "missing caller agent context"}
	}
	taskSummary, _ := args["task_summary"].(string)
	plan, _ := args["plan"].(string)
	if strings.TrimSpace(taskSummary) == "" || strings.TrimSpace(plan) == "" {
		return &tool.Result{
			Tool:     "submit_plan",
			Category: tool.ResultCategoryValidationRejected,
			Error:    "task_summary 与 plan 均必填：task_summary 是一行任务简介，plan 是计划详情（步骤/涉及文件/验收标准）。",
		}
	}
	// 开关未启用（或状态未初始化，如测试直构）：直通不阻塞。
	if d == nil || !d.planConfirmEnabled || d.planState == nil {
		return &tool.Result{Tool: "submit_plan", Success: true,
			Output: "计划确认机制未启用（plan_confirmation_enabled=false），请直接按当前理解执行。"}
	}

	planID, w := d.planState.register(agentID, parentAgentIDOf(agentID))
	defer d.planState.remove(planID)

	// 顶层（agentID 无 "/"）：上级是用户，走 ask_user 既有澄清链路。
	if w.parentID == "" || !strings.Contains(agentID, "/") {
		return d.submitPlanToUser(ctx, planID, taskSummary, plan)
	}
	return d.submitPlanToParent(ctx, w, planID, taskSummary, plan)
}

// submitPlanToUser 顶层路径：把计划渲染成 ask_user 问题（选项：批准开工 / 需要修改），
// 复用 session 澄清槽位 → SSE → TUI 面板，零新增适配层。
func (d *Dispatcher) submitPlanToUser(ctx context.Context, planID, taskSummary, plan string) *tool.Result {
	question := fmt.Sprintf("【计划确认】执行前请确认。\n任务简介: %s\n\n【计划详情】\n%s\n\n批准后立即开工；选\"需要修改\"可直接输入修改意见。", taskSummary, plan)
	res, err := d.tools.Dispatch(ctx, "ask_user", map[string]any{
		"question": question,
		"options": []any{
			map[string]any{"id": "approve", "label": "批准开工", "description": "按计划执行"},
			map[string]any{"id": "revise", "label": "需要修改", "description": "驳回计划并给出修改意见"},
		},
	})
	if err != nil || res == nil {
		// ask_user 未接线/派发失败：fail-open 与等待超时语义一致。
		log.Printf("[plan] submit_plan ask_user dispatch failed: plan_id=%s err=%v", planID, err)
		return &tool.Result{Tool: "submit_plan", Success: true,
			Output: "计划确认通道不可用，按计划继续执行（fail-open）。"}
	}
	answer := strings.TrimSpace(res.Output)
	// ask_user 超时文案"用户未答复，自行决策"→ 视为同意继续（fail-open）。
	if strings.Contains(answer, "未答复") {
		return &tool.Result{Tool: "submit_plan", Success: true,
			Output: "用户未及时答复计划（超时），按计划继续执行（fail-open）。"}
	}
	approved, feedback := parsePlanAnswer(answer)
	if approved {
		return &tool.Result{Tool: "submit_plan", Success: true,
			Output: "【计划已批准】用户批准了计划（plan_id=" + planID + "），按计划执行。"}
	}
	return &tool.Result{Tool: "submit_plan", Success: true,
		Output: "【计划被驳回】用户意见: " + feedback + "\n请修订后重新调用 submit_plan 提交。"}
}

// submitPlanToParent 下级路径：发邮箱审批请求给 parentID 并阻塞等待 review_plan 结论。
func (d *Dispatcher) submitPlanToParent(ctx context.Context, w *planWaiter, planID, taskSummary, plan string) *tool.Result {
	body := fmt.Sprintf("【计划审批请求】plan_id=%s\n提交者: %s\n任务简介: %s\n\n【计划详情】\n%s\n\n【审批指引】请及时调用 review_plan 工具审批："+
		"verdict=approve 批准；verdict=reject 且 feedback=具体修改意见 驳回。批准后提交者才会继续执行。",
		planID, w.agentID, taskSummary, plan)
	_, err := d.mailbox.Send(&mailbox.Message{
		From:     w.agentID,
		To:       w.parentID,
		Type:     mailbox.MsgRequest,
		Subject:  "计划审批请求: " + truncatePlanText(taskSummary, 60),
		Body:     body,
		ThreadID: planID,
		Priority: 10, // 高优先级：审批请求排在普通结果摘要之前
	})
	if err != nil {
		// 上级已销毁（Purge）等不可达场景：fail-open，避免下级永久挂起。
		log.Printf("[plan] submit_plan mailbox send failed: plan_id=%s from=%s to=%s err=%v", planID, w.agentID, w.parentID, err)
		return &tool.Result{Tool: "submit_plan", Success: true,
			Output: fmt.Sprintf("上级 %s 不可达（%v），按计划继续执行（fail-open）。", w.parentID, err)}
	}
	// 立刻唤醒父 Agent 的 waitForChildren 轮询（不等 30s tick）。
	d.pokeParent(w.parentID)

	timeout := d.planConfirmTimeout
	if timeout <= 0 {
		timeout = defaultPlanConfirmTimeout
	}
	maxRev := d.planMaxRevisions // <=0 表示不限制（循环直到批准）
	ticker := time.NewTicker(planPingInterval)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case v := <-w.ch:
			if v.approved {
				return &tool.Result{Tool: "submit_plan", Success: true,
					Output: "【计划已批准】上级已批准计划（plan_id=" + planID + "），按计划执行。"}
			}
			n := d.planState.bumpRevisions(w.agentID)
			if maxRev > 0 && n >= maxRev {
				// 达配置上限：仍不放行（未经批准禁止执行），转升级仲裁防无限循环。
				return &tool.Result{Tool: "submit_plan", Success: true,
					Output: fmt.Sprintf("【计划驳回达上限】计划已被驳回 %d 次（上限 %d），最近修改意见: %s\n"+
						"未经上级批准禁止执行。请用 send_message(to_agent_id=%s, message_type=escalate) "+
						"说明与上级的分歧请求仲裁，或终止本任务并在结论中说明计划未获批准。", n, maxRev, v.feedback, w.parentID)}
			}
			remaining := ""
			if maxRev > 0 {
				remaining = fmt.Sprintf("（还可提交 %d 次）", maxRev-n)
			}
			return &tool.Result{Tool: "submit_plan", Success: true,
				Output: fmt.Sprintf("【计划被驳回】上级修改意见: %s\n请按意见修订后重新调用 submit_plan 提交，直到批准为止%s。",
					v.feedback, remaining)}
		case <-ticker.C:
			// 保活：等待审批期间刷新活动时间并沿父链冒泡，防心跳巡检误判假死 kill。
			d.PingActivity(w.agentID)
		case <-timer.C:
			log.Printf("[plan] submit_plan wait timeout: plan_id=%s from=%s to=%s timeout=%s", planID, w.agentID, w.parentID, timeout)
			return &tool.Result{Tool: "submit_plan", Success: true,
				Output: "审批等待超时（" + timeout.String() + "），按计划继续执行（fail-open）。"}
		case <-ctx.Done():
			return &tool.Result{Tool: "submit_plan", Error: fmt.Sprintf("计划审批等待中断: %v", ctx.Err())}
		}
	}
}

// reviewPlanTool 实现 review_plan 工具：上级审批下级提交的计划并回传结论。
type reviewPlanTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *reviewPlanTool) Name() string { return "review_plan" }

// Aliases 返回工具别名。
func (t *reviewPlanTool) Aliases() []string { return []string{"plan_review"} }

// Description 返回 LLM 可见的工具描述。
func (t *reviewPlanTool) Description() string {
	return "审批下级提交的计划（收到【计划审批请求】邮箱消息后调用）：plan_id 取自消息首行，" +
		"verdict=approve 批准；verdict=reject 驳回（feedback 必填具体修改意见，下级会按意见修订重提）。" +
		"只有该计划的直接上级可审批。"
}

// Execute 审批计划：校验 plan_id 存活与调用者身份，投递结论唤醒等待中的提交者。
func (t *reviewPlanTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	planID, _ := args["plan_id"].(string)
	verdictRaw, _ := args["verdict"].(string)
	feedback, _ := args["feedback"].(string)
	if strings.TrimSpace(planID) == "" {
		return &tool.Result{Tool: "review_plan", Category: tool.ResultCategoryValidationRejected,
			Error: "plan_id 必填（取自【计划审批请求】消息首行）。"}
	}
	var approved bool
	switch {
	case verdictRaw == "approve" || verdictRaw == "批准":
		approved = true
	case verdictRaw == "reject" || verdictRaw == "驳回":
		if strings.TrimSpace(feedback) == "" {
			return &tool.Result{Tool: "review_plan", Category: tool.ResultCategoryValidationRejected,
				Error: "驳回（verdict=reject）必须附 feedback 具体修改意见，下级按意见修订后重提。"}
		}
		approved = false
	default:
		return &tool.Result{Tool: "review_plan", Category: tool.ResultCategoryValidationRejected,
			Error: fmt.Sprintf("verdict 仅支持 approve/reject（当前: %q）。", verdictRaw)}
	}
	if d == nil || d.planState == nil {
		return &tool.Result{Tool: "review_plan", Error: "计划确认机制未初始化"}
	}
	w, ok := d.planState.get(planID)
	if !ok {
		return &tool.Result{Tool: "review_plan", Category: tool.ResultCategoryValidationRejected,
			Error: "plan_id 不存在或已失效（可能已审批、已超时或从未提交过）。请核对【计划审批请求】消息首行的 plan_id。"}
	}
	// 身份校验：只有该计划的直接上级可审批，防止越级/兄弟误审。
	if caller := agent.AgentIDFromContext(ctx); caller != "" && caller != w.parentID {
		return &tool.Result{Tool: "review_plan", Error: fmt.Sprintf("只有该计划的直接上级 %s 可审批，当前调用者: %s。", w.parentID, caller)}
	}
	select {
	case w.ch <- planVerdict{approved: approved, feedback: feedback}:
	default:
		// 缓冲 1 且等待者取走前不会重复审批（审批后 waiter 即被移除），理论不可达；防御性忽略。
	}
	out := "已回传审批结果: 批准。"
	if !approved {
		out = "已回传审批结果: 驳回（意见已随结论送达提交者）。"
	}
	return &tool.Result{Tool: "review_plan", Success: true, Output: out}
}

// parsePlanAnswer 解析用户对计划的答复：先匹配驳回关键词（优先，防"不批准"误判为批准），
// 再匹配批准关键词；都未命中按驳回处理（原话作为修改意见，fail-closed）。
func parsePlanAnswer(answer string) (approved bool, feedback string) {
	a := strings.ToLower(answer)
	rejectHits := strings.Contains(a, "不批准") || strings.Contains(a, "不同意") || strings.Contains(a, "驳回") ||
		strings.Contains(a, "需要修改") || strings.Contains(a, "reject") || strings.Contains(a, "修改")
	if rejectHits {
		return false, answer
	}
	if strings.Contains(a, "批准") || strings.Contains(a, "同意") || strings.Contains(a, "通过") || strings.Contains(a, "approve") {
		return true, ""
	}
	// 自由文本（既无明确批准也无明确驳回）：视为修改意见驳回。
	return false, answer
}

// parentAgentIDOf 返回 agentID 的直接上级 ID：去掉最后一段 "/role-seq"。
// 顶层（无 "/"，agentID 即 sessionID）返回空串，表示上级是用户。
func parentAgentIDOf(agentID string) string {
	if i := strings.LastIndex(agentID, "/"); i > 0 {
		return agentID[:i]
	}
	return ""
}

// truncatePlanText 按 rune 截断文本用于 Subject 一行摘要。
func truncatePlanText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
