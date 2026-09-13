package subagent

// 验收通过后的录播演示阶段（2026-09-13）：验收 PASS 后经 ask_user 询问用户是否
// 进入演示，确认则派发 test_assistant（demo 领域）模拟真人完整操作交付物并录制
// 演示视频，再经 ask_user 评审卡（artifacts 内嵌演示产物）请用户查验；打回意见
// 经 test_assistant 归因责任 Agent 后复用验收返工链路（reworkFailures）修复，
// 下一轮重新验收 + 重新确认演示（与验收共享 max_rounds 硬顶）。
// 全链路 fail-open：确认/录播/评审任一步骤不可用、失败或超时均跳过演示直接交付，
// 不阻塞主流程（评审超时也视为通过——与计划确认相反的默认值是有意的）。

import (
	"context"       // context 控制演示派发与等待的取消
	"fmt"           // fmt 格式化演示任务文本
	"log"           // log 记录派发失败（不影响主流程）
	"path/filepath" // filepath 拼接 .bma 相对路径
	"regexp"        // regexp 解析机读契约块（【演示产物】/【演示摘要】）
	"strings"       // strings 文本裁剪与判定

	"github.com/blockmemory/agent/backend/internal/agent"               // agent.WithAgentID 构造内部派发 ctx
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator" // orchestrator.Node/Tree 权威树读取
)

// demoDomain 演示节点的领域名（树/看板展示用；collectAcceptanceRoster 按角色排除
// 全部 test_assistant 节点，演示节点不承担实现责任，不进错误归因名单）。
const demoDomain = "demo"

var (
	// demoArtifactRe 提取演示节点答复末尾的【演示产物】路径（相对工作目录）。
	demoArtifactRe = regexp.MustCompile(`【演示产物】\s*(\S+)`)
	// demoSummaryRe 提取【演示摘要】操作流说明（单行）。
	demoSummaryRe = regexp.MustCompile(`【演示摘要】\s*(.+)`)
)

// demoStage 验收 PASS 后的演示阶段：确认（confirmDemo）→ 录播（runDemo）→ 评审（reviewDemo）。
// 返回 (approved, feedback, artifactPath)：
//   - approved=true：用户跳过演示 / 任一步骤降级跳过 / 演示评审通过，直接交付；
//     artifactPath 仅评审通过时非空（终答附演示视频路径）。
//   - approved=false：用户打回，feedback 为打回意见原文，由 run 主循环归因返工。
func (m *AcceptanceManager) demoStage(ctx context.Context, sessionID string, t *orchestrator.Tree, round int, reportPath string) (approved bool, feedback string, artifactPath string) {
	if !m.confirmDemo(ctx) {
		return true, "", ""
	}
	path, summary, ok := m.runDemo(ctx, sessionID, t, round, reportPath)
	if !ok {
		return true, "", ""
	}
	approved, feedback = m.reviewDemo(ctx, path, summary)
	if approved {
		m.notifyUser(sessionID, "演示查验通过，交付。")
		return true, "", path
	}
	return false, feedback, ""
}

// confirmDemo 询问用户是否进入演示环节（ask_user，计划确认 submitPlanToUser 同款链路）。
// 派发失败/超时（"未答复"）一律 false 跳过（fail-open：演示是增强环节，不阻塞交付）。
func (m *AcceptanceManager) confirmDemo(ctx context.Context) bool {
	res, err := m.d.tools.Dispatch(ctx, "ask_user", map[string]any{
		"question": "测试助手验收已通过。是否进入演示环节？演示将录制完整的操作演示视频供你查验。",
		"options": []any{
			map[string]any{"id": "demo", "label": "进入演示", "description": "录制演示视频"},
			map[string]any{"id": "skip", "label": "跳过演示", "description": "直接交付"},
		},
	})
	if err != nil || res == nil || !res.Success {
		// ask_user 未接线/派发失败：fail-open 与等待超时语义一致。
		log.Printf("[acceptance] demo confirm ask_user dispatch failed: err=%v", err)
		return false
	}
	answer := unwrapAskAnswer(res.Output)
	// ask_user 超时文案"用户未答复，自行决策"→ 跳过演示（fail-open）。
	if strings.Contains(answer, "未答复") {
		return false
	}
	return parseDemoConfirm(answer)
}

// unwrapAskAnswer 剥离 ask_user 工具结果包装（"用户答复:\n1. 答复: <原文>"）取答复原文；
// 无包装时原样返回裁剪文本。
func unwrapAskAnswer(output string) string {
	if i := strings.LastIndex(output, "答复: "); i >= 0 {
		return strings.TrimSpace(output[i+len("答复: "):])
	}
	return strings.TrimSpace(output)
}

// parseDemoConfirm 解析「是否进入演示」的答复：先匹配跳过词（优先，防"不需要演示"
// 误判为进入），再匹配进入词；自由文本默认跳过（fail-open：默认不录）。
func parseDemoConfirm(answer string) bool {
	a := strings.ToLower(answer)
	if strings.Contains(a, "跳过") || strings.Contains(a, "skip") || strings.Contains(a, "不用") ||
		strings.Contains(a, "不需要") || strings.Contains(a, "否") {
		return false
	}
	if strings.Contains(a, "进入") || strings.Contains(a, "演示") || strings.Contains(a, "开始") ||
		strings.Contains(a, "确认") || strings.Contains(a, "好") || strings.Contains(a, "yes") || strings.Contains(a, "ok") {
		return true
	}
	return false
}

// runDemo 派发 test_assistant（demo 领域）录制演示并等待终态（复用验收派发的参数模式
// 与墙钟）。派发失败/节点失败取消/缺【演示产物】均 notifyUser 警告后降级跳过（不阻塞交付）。
// 返回 (产物相对路径, 演示摘要, ok)。
func (m *AcceptanceManager) runDemo(ctx context.Context, sessionID string, t *orchestrator.Tree, round int, reportPath string) (string, string, bool) {
	m.notifyUser(sessionID, "正在录制演示（模拟真人完整操作交付物），完成后请你查验……")
	task := buildDemoTask(sessionID, round, reportPath)
	// 内部派发：以 Meta（agentID=sessionID）为父走 dispatchOne 同链路，墙钟复用验收派发预算。
	dctx := agent.WithAgentID(ctx, sessionID)
	demoID, errRes := m.d.dispatchOne(dctx, "test_assistant", demoDomain, task, "", "", "",
		nil, nil, acceptanceTesterWallClock, "", "")
	if errRes != nil {
		log.Printf("[acceptance] dispatch demo failed: session=%s round=%d err=%s", sessionID, round, errRes.Error)
		m.notifyUser(sessionID, "演示录制未能启动（"+errRes.Error+"），跳过演示直接交付。")
		return "", "", false
	}
	node, ok := m.waitNodeTerminal(ctx, t, demoID)
	if !ok {
		return "", "", false // ctx 取消（会话停止）：静默降级
	}
	// 演示节点自身失败（超时/被杀/崩溃）：带警告跳过，不重试。
	if node.Status == orchestrator.StatusFailed || node.Status == orchestrator.StatusCancelled {
		reason := strings.TrimSpace(node.Err)
		if reason == "" {
			reason = "演示 Agent 异常终止"
		}
		m.notifyUser(sessionID, "演示录制未完成（"+reason+"），跳过演示直接交付。")
		return "", "", false
	}
	artifactPath, summary := parseDemoOutput(node.Summary)
	if artifactPath == "" {
		m.notifyUser(sessionID, "演示节点未按契约输出【演示产物】，跳过演示直接交付。")
		return "", "", false
	}
	return artifactPath, summary, true
}

// buildDemoTask 构造演示录制的派发任务文本（仿 buildAcceptanceTask 结构）。
// 【接力理由】标记：dispatcher 接力熔断（relay.go）对同 (parent, domain) 第 4 代起
// 拒派无声明任务，演示录制属合法代际，恒带声明防误拒。
func buildDemoTask(sessionID string, round int, reportPath string) string {
	if reportPath == "" {
		reportPath = filepath.Join(".bma", "acceptance", fmt.Sprintf("%s-r%d.md", sessionID, round))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【演示任务】你是交付演示员：第 %d 轮验收已通过，请录制完整的操作演示视频。\n", round)
	b.WriteString("【接力理由】验收通过后的录播演示为独立阶段，属同一交付闭环的合法代际。\n\n")
	b.WriteString("【验收报告】先阅读验收报告了解交付物与操作路径：" + reportPath + "\n\n")
	b.WriteString("【演示要求】\n")
	b.WriteString("1. 模拟人类完整操作交付物：逐页/逐功能的真实操作流（点击、输入、跳转），不是静态展示。\n")
	b.WriteString("2. 录制演示视频，落盘到 .bma/demo/ 目录（相对工作目录）。\n")
	b.WriteString("3. 录像手段按优先级：web 交付物优先用 Playwright recordVideo（npx playwright，webm/mp4 均可）；" +
		"系统有 ffmpeg 则用 ffmpeg gdigrab 录屏 mp4；都不行则用 host_computer_use 截图序列 + " +
		"自生成一个 HTML 回放页（.bma/demo/replay.html）。\n")
	b.WriteString("4. 所需插件工具按需 tool_catalog/tool_mount 挂载。\n\n")
	b.WriteString("【机读契约（硬约束）】最终答复末尾必须逐字输出：\n【演示产物】<相对工作目录路径>\n【演示摘要】<一段操作流说明>\n")
	return b.String()
}

// parseDemoOutput 解析演示节点最终答复末尾的机读契约块【演示产物】/【演示摘要】。
func parseDemoOutput(text string) (artifactPath, summary string) {
	if m := demoArtifactRe.FindStringSubmatch(text); m != nil {
		artifactPath = m[1]
	}
	if m := demoSummaryRe.FindStringSubmatch(text); m != nil {
		summary = strings.TrimSpace(m[1])
	}
	return artifactPath, summary
}

// reviewDemo 演示评审卡：artifacts 内嵌演示产物请用户查验（.html 回放页 kind=html，
// 其余按视频 kind=video）。派发失败/超时/未答复 fail-open 视为通过（与计划确认相反的
// 默认值是有意的：演示是增强环节，不阻塞交付）。
func (m *AcceptanceManager) reviewDemo(ctx context.Context, artifactPath, summary string) (approved bool, feedback string) {
	kind := "video"
	if strings.HasSuffix(strings.ToLower(artifactPath), ".html") {
		kind = "html"
	}
	res, err := m.d.tools.Dispatch(ctx, "ask_user", map[string]any{
		"question": "演示已完成，请查验演示视频。",
		"detail":   summary,
		"artifacts": []any{
			map[string]any{"kind": kind, "path": artifactPath, "title": "演示视频"},
		},
		"options": []any{
			map[string]any{"id": "pass", "label": "演示通过", "description": "验收+演示均通过，交付"},
			map[string]any{"id": "reject", "label": "演示打回", "description": "演示暴露问题，打回修复"},
		},
	})
	if err != nil || res == nil || !res.Success {
		log.Printf("[acceptance] demo review ask_user dispatch failed: err=%v", err)
		return true, ""
	}
	answer := unwrapAskAnswer(res.Output)
	if answer == "" || strings.Contains(answer, "未答复") {
		return true, "" // fail-open：超时/未答复视为通过
	}
	approved, feedback = parseDemoReview(answer)
	if approved {
		return true, ""
	}
	// 答案只是选项文案无实质意见：追加一次纯文本提问收打回意见原文。
	fb := strings.TrimSpace(feedback)
	if fb == "" || fb == "演示打回" || strings.EqualFold(fb, "reject") {
		fb = m.askDemoFeedback(ctx)
	}
	return false, fb
}

// parseDemoReview 解析演示评审答复：驳回词优先（防"不通过"误判通过），再匹配批准词；
// 自由文本按打回处理（原话作意见，parsePlanAnswer 同款语义）。
func parseDemoReview(answer string) (approved bool, feedback string) {
	a := strings.ToLower(answer)
	if strings.Contains(a, "打回") || strings.Contains(a, "不通过") ||
		strings.Contains(a, "reject") || strings.Contains(a, "有问题") {
		return false, answer
	}
	if strings.Contains(a, "通过") || strings.Contains(a, "pass") || strings.Contains(a, "approve") ||
		strings.Contains(a, "ok") || strings.Contains(a, "可以") || strings.Contains(a, "满意") {
		return true, ""
	}
	return false, answer
}

// askDemoFeedback 追加纯文本提问收打回意见原文；派发失败/超时/空白回落占位文案（不阻塞流程）。
func (m *AcceptanceManager) askDemoFeedback(ctx context.Context) string {
	res, err := m.d.tools.Dispatch(ctx, "ask_user", map[string]any{
		"question": "请说明打回意见（演示暴露了什么问题？将派回责任 Agent 修复）。",
	})
	if err != nil || res == nil || !res.Success {
		log.Printf("[acceptance] demo feedback ask_user dispatch failed: err=%v", err)
		return "用户打回演示（未说明具体意见）"
	}
	ans := unwrapAskAnswer(res.Output)
	if ans == "" || strings.Contains(ans, "未答复") {
		return "用户打回演示（未说明具体意见）"
	}
	return ans
}

// attributeDemoRejection 派发 test_assistant（acceptance 领域）把演示打回意见归因到
// 责任 Agent（机读块与验收同款，复用 parseAcceptanceReport 解析）。
// 归因不出条目时兜底：把意见作为单条错误派给名单里最近完成的执行节点；
// 名单为空返回 nil（由调用方记未验证项交付）。
func (m *AcceptanceManager) attributeDemoRejection(ctx context.Context, sessionID string, t *orchestrator.Tree, feedback, reportPath string) []acceptanceError {
	roster, _ := collectAcceptanceRoster(t)
	task := buildAttributionTask(feedback, reportPath, roster)
	dctx := agent.WithAgentID(ctx, sessionID)
	attrID, errRes := m.d.dispatchOne(dctx, "test_assistant", acceptanceDomain, task, "", "", "",
		nil, nil, acceptanceTesterWallClock, "", "")
	if errRes != nil {
		log.Printf("[acceptance] dispatch demo attribution failed: session=%s err=%s", sessionID, errRes.Error)
	} else if node, ok := m.waitNodeTerminal(ctx, t, attrID); ok &&
		node.Status != orchestrator.StatusFailed && node.Status != orchestrator.StatusCancelled {
		if _, _, items := parseAcceptanceReport(node.Summary); len(items) > 0 {
			return items
		}
	}
	// 兜底：归因不出条目，派给名单里最近完成的执行节点（名单按启动时间升序）。
	for i := len(roster) - 1; i >= 0; i-- {
		if roster[i].Status == orchestrator.StatusDone.String() {
			return []acceptanceError{{AgentID: roster[i].ID, Desc: "演示打回：" + feedback}}
		}
	}
	return nil
}

// buildAttributionTask 构造演示打回归因任务文本：含用户打回意见原文 + 验收报告路径 +
// 执行 Agent 名单，要求定位责任 Agent 输出与验收相同的机读块（复用 parseAcceptanceReport 解析）。
// 【接力理由】标记防 dispatcher 接力熔断误拒（同 buildAcceptanceTask 注释）。
func buildAttributionTask(feedback, reportPath string, roster []acceptanceRosterEntry) string {
	var b strings.Builder
	b.WriteString("【归因任务】用户查验演示视频后打回，请根据打回意见定位责任 Agent。\n")
	b.WriteString("【接力理由】演示评审打回后的错误归因，属同一交付闭环的合法代际。\n\n")
	b.WriteString("【用户打回意见原文】\n" + feedback + "\n\n")
	if reportPath != "" {
		b.WriteString("【验收报告】此前验收报告（含交付物与操作路径）：" + reportPath + "\n\n")
	}
	b.WriteString("【执行 Agent 名单】（错误归因用，[agent:id] 照抄下列 ID）\n")
	if len(roster) == 0 {
		b.WriteString("- （无子 Agent 记录，交付物由 Meta 自执行产出，错误归属写 [agent:meta]）\n")
	}
	for _, e := range roster {
		label := e.ID
		if e.Domain != "" {
			label += "（领域：" + e.Domain + "）"
		}
		fmt.Fprintf(&b, "- %s 角色 %s，状态 %s\n  任务：%s\n  结果摘要：%s\n", label, e.Role, e.Status, e.Task, e.Summary)
	}
	b.WriteString("\n【机读契约（硬约束）】最终答复末尾必须逐字输出：\n【验收结论】FAIL\n【错误清单】\n1. [agent:<agentID>] <问题描述>\n")
	return b.String()
}
