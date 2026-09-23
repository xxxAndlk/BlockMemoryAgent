package subagent

// decision_gates.go 决策层四个派发侧切入点（TODO #23③-2/3/4/5）。
// 统一纪律：硬规则（failureKindOf errors.Is / checkRoleTaskFit / checkTaskPathsFit /
// hasSubstantiveChange / salvage 防污染闸）保持 rules-first 不动；决策层只补灰区建议。
// 影子期（默认）只落 agent_events type=decision_shadow 对拍行，零行为变化；
// enforce 点（config agent.decision_points.<point>.mode=enforce 且置信达标）才生效。
// provider 故障/超时/解析失败一律 fail-open 走现状行为（Ask 返回 nil 即无建议）。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/decision"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// decisionGateFailureDisposition ②失败处置路由（runSubAgent 失败消息组装处）。
// failureKindOf/retryable 硬规则不动（rules-first）；决策层出 Choice(retry/redelegate/
// escalate/suspend) 建议：影子期对拍（actual=auto-retry 实际是否发生，父 LLM 后续
// 选择离线经 sub_agent_dispatch 事件关联补对拍）；enforce 点把建议附进失败消息，
// 父 LLM 可见可采纳（"只做灰区判断与建议"，不替父做分支选择）。
// 返回可追加进失败消息的建议段（空=无建议/影子期）。
func (d *Dispatcher) decisionGateFailureDisposition(ctx context.Context, subAgentID string, roleDef types.RoleDefinition, kind FailureKind, retryable, retried bool, failText, partial string) string {
	if d.decisionLayer == nil {
		return ""
	}
	summary := fmt.Sprintf("role=%s kind=%s retryable=%t retried=%t\nerror=%s\npartial=%s",
		roleDef.ID, kind, retryable, retried, compactForDecision(failText, 1500), compactForDecision(partial, 800))
	q := decision.FailureDispositionQuestion(summary)
	// 同步 Ask：失败路径本就离热路径远，且 enforce 需要结果拼建议。
	ans := d.decisionLayer.Ask(ctx, summary, q)
	if ans == nil {
		return ""
	}
	// 对拍值：auto-retry 实际发生记 retry；否则父 LLM 尚未选择（pending，离线补对拍）。
	actual := ""
	if retried {
		actual = decision.DispRetry
	}
	d.decisionLayer.Observe(ctx, subAgentID, actual, *ans)
	if !d.decisionLayer.ShouldEnforce(decision.PointFailureDisp, ans.Confidence) {
		return ""
	}
	return fmt.Sprintf("\n\n【处置建议】%s（置信 %.2f）——决策层建议，父 Agent 可自行裁量", ans.Value, ans.Confidence)
}

// decisionGateDispatchGap ③派发门灰区补充（dispatchOne 两条硬规则之后）。
// checkRoleTaskFit/checkTaskPathsFit 高置信硬拒保持不动；本 gate 只补规则正则
// 覆盖不到的需求信号（needs_browser/needs_mcp/needs_vision）。影子期只落对拍行
// （actual=none，现状=规则放行）；enforce 点命中缺口且置信达标时返回拦截消息
// （validation_rejected 同款语义），空串=放行。
func (d *Dispatcher) decisionGateDispatchGap(ctx context.Context, parentID, roleID, task string) string {
	if d.decisionLayer == nil {
		return ""
	}
	q := decision.DispatchGapQuestion(roleID, task)
	req := decision.Request{State: "派发前灰区需求信号检查", Questions: []decision.Question{q}}
	resp, err := d.decisionLayer.Decide(ctx, req)
	if err != nil || len(resp.Answers) == 0 {
		return "" // fail-open：决策层故障不拦派发
	}
	ans := resp.Answers[0]
	// 对拍值=none（两条硬规则已放行，现状路径无缺口拦截）。
	d.decisionLayer.Observe(ctx, parentID, decision.GapNone, ans)
	if !d.decisionLayer.ShouldEnforce(decision.PointDispatchGap, ans.Confidence) {
		return ""
	}
	if ans.Value == decision.GapNone {
		return ""
	}
	return fmt.Sprintf("派发门灰区拦截（决策层，置信 %.2f）：任务疑似需要 %s 而规则未覆盖——补工具面/换角色后再派", ans.Confidence, ans.Value)
}

// decisionGateUptakeScore ④黑板摄取相关性打分（Dispatcher 侧入口，播种召回用）。
func (d *Dispatcher) decisionGateUptakeScore(ctx context.Context, agentID, task string, recs []*types.KnowledgeRecord) []*types.KnowledgeRecord {
	return gateUptakeScore(ctx, d.decisionLayer, agentID, task, recs)
}

// gateUptakeScore ④黑板摄取相关性打分（siblingUptake Assemble Query 后
// rank 前 + injectScopedRecall 播种召回同款）。现排序规则（outcome/复用/新近）不动；
// 决策层逐候选 Score：影子期对拍（actual=yes，现状=topK 全保留注入），enforce 点
// 低于 score_floor 的候选剔除（省上下文）。返回过滤后切片（影子期/故障=原样返回）。
func gateUptakeScore(ctx context.Context, layer *decision.Layer, agentID, task string, recs []*types.KnowledgeRecord) []*types.KnowledgeRecord {
	if layer == nil || len(recs) == 0 {
		return recs
	}
	contents := make([]string, len(recs))
	for i, r := range recs {
		contents[i] = r.Content
	}
	req := decision.Request{
		State:     "黑板摄取相关性筛选",
		Questions: decision.UptakeScoreQuestions(contents, task),
	}
	resp, err := layer.Decide(ctx, req)
	if err != nil {
		return recs // fail-open
	}
	floor := layer.ScoreFloor(decision.PointUptakeScore)
	kept := make([]*types.KnowledgeRecord, 0, len(recs))
	for i, r := range recs {
		ans, ok := resp.AnswerOf("rec:" + itoaDec(i))
		if !ok {
			kept = append(kept, r) // 缺答案 fail-open 保留
			continue
		}
		// 对拍值=yes：现状管线对 topK 候选全保留（rank 只排序不丢弃）。
		layer.Observe(ctx, agentID, "yes", ans)
		if layer.ShouldEnforce(decision.PointUptakeScore, ans.Confidence) && ans.Score < floor {
			continue // enforce 点：低相关剔除
		}
		kept = append(kept, r)
	}
	if len(kept) != len(recs) {
		log.Printf("[subagent] decision uptake filter: %d -> %d (agent=%s)", len(recs), len(kept), agentID)
	}
	return kept
}

// decisionGateExtractWorth ⑤沉淀提取预判（saveBlockMemory hasSubstantiveChange 硬门
// 之后、factExtractor.Extract 之前）。规则门保留为硬底；本 Noul 预判省无效 LLM
// 提取调用。返回 (skip, ans)：skip=true 时调用方跳过提取；ans 供调用方在提取后
// 补 actual 对拍（影子期必须照常提取拿真值）。
//   - 影子期：恒不 skip（照常提取），调用方提取后 Observe(ans, actual=事实数>0)；
//   - enforce 点：判 no 且置信达标 → skip（省调用），actual 留 pending（未提取无真值）。
func (d *Dispatcher) decisionGateExtractWorth(ctx context.Context, subAgentID, goal, roleID, content string) (bool, *decision.Answer) {
	if d.decisionLayer == nil {
		return false, nil
	}
	q := decision.ExtractWorthQuestion(goal, roleID)
	req := decision.Request{State: compactForDecision(content, 2500), Questions: []decision.Question{q}}
	resp, err := d.decisionLayer.Decide(ctx, req)
	if err != nil || len(resp.Answers) == 0 {
		return false, nil
	}
	ans := resp.Answers[0]
	if d.decisionLayer.ShouldEnforce(decision.PointExtractWorth, ans.Confidence) && ans.Value == "no" {
		d.decisionLayer.Observe(ctx, subAgentID, "", ans) // 未提取无真值 → pending
		return true, &ans
	}
	return false, &ans
}

// decisionGateSalvageWorth ⑤打捞提取预判（salvageFailure salvageExtractor 前）。
// kill 场景（History nil）跳过已是规则版先例保持不动；此处管有历史场景的提取预判。
// 语义同 decisionGateExtractWorth：影子期照常提取补对拍，enforce 点判 no 省调用。
func (d *Dispatcher) decisionGateSalvageWorth(ctx context.Context, subAgentID, roleID, text string) (bool, *decision.Answer) {
	if d.decisionLayer == nil {
		return false, nil
	}
	q := decision.SalvageWorthQuestion(roleID)
	req := decision.Request{State: compactForDecision(text, 2500), Questions: []decision.Question{q}}
	resp, err := d.decisionLayer.Decide(ctx, req)
	if err != nil || len(resp.Answers) == 0 {
		return false, nil
	}
	ans := resp.Answers[0]
	if d.decisionLayer.ShouldEnforce(decision.PointSalvageWorth, ans.Confidence) && ans.Value == "no" {
		d.decisionLayer.Observe(ctx, subAgentID, "", ans)
		return true, &ans
	}
	return false, &ans
}

// observeExtractOutcome 沉淀/打捞提取后补对拍真值（影子期数据源）。
// ans 为 nil（决策层未跑）时零操作。
func (d *Dispatcher) observeExtractOutcome(ctx context.Context, subAgentID string, ans *decision.Answer, extracted bool) {
	if d.decisionLayer == nil || ans == nil {
		return
	}
	actual := "no"
	if extracted {
		actual = "yes"
	}
	d.decisionLayer.Observe(ctx, subAgentID, actual, *ans)
}

// compactForDecision 决策上下文截断（超长丢中段）。
func compactForDecision(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "...(截断)"
}

// itoaDec 零依赖整数转十进制字符串。
func itoaDec(n int) string {
	return fmt.Sprintf("%d", n)
}
