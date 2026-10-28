package agent

// decision_axes_test.go 覆盖多轴决策账本（2026-10-24 P1，计划 §5.1/§8.1）：
//   - 同轴互斥、跨轴同时有效（"重新 AI 生图"不撤销动画路线/视角/审批门）；
//   - answer_only 只改执行模式，不产生新路线；
//   - "继续" 继承当前状态；
//   - "幅度太小" 是产物反馈，不否决整个技术路线；
//   - 决策块超预算时仍保留当前各轴与最新指令（确定性裁剪）。

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/store"
)

func newAxisTestService() *ReactService {
	svc := &ReactService{}
	svc.SetMemoryWriteMode(MemoryWriteModeShadow)
	return svc
}

// activeAxes 返回 axis → active route 内容（测试断言用）。
func activeAxes(sess *reactInternalSession) map[string]string {
	out := map[string]string{}
	for _, d := range sess.decisions.items {
		if d.Kind == store.DecisionKindRoute && d.Status == store.DecisionStatusActive {
			out[d.Axis] = d.Content
		}
	}
	return out
}

// TestAxisRegenerateDoesNotClearOtherAxes 「重新 AI 生图」只改 asset_source，
// 不撤销动画方式、视角与锚点审批（会话 #65/#66 的直接事故形态）。
func TestAxisRegenerateDoesNotClearOtherAxes(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-axis", Goal: "做正面 45 度视角的异兽奔跑"}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "动画用 16 帧图集序列帧", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "视角就按正面 45 度", "user_message")
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindDirective, store.AxisApprovalAnchor,
		approvalContent(true, "assets/img/anchor.png"), "asset_gate")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "重新 AI 生图，不要代码在那调试了", "user_message")

	axes := activeAxes(sess)
	if _, ok := axes[store.AxisAnimationMethod]; !ok {
		t.Fatalf("重新 AI 生图不得撤销动画路线，axes=%v", axes)
	}
	if got := axes[store.AxisView]; !strings.Contains(got, "45") {
		t.Fatalf("重新 AI 生图不得撤销视角，axes=%v", axes)
	}
	if _, ok := axes[store.AxisAssetSource]; !ok {
		t.Fatalf("重新 AI 生图应登记素材来源轴，axes=%v", axes)
	}
	state := ""
	for _, d := range sess.decisions.items {
		if d.Axis == store.AxisApprovalAnchor && d.Status == store.DecisionStatusActive {
			state = approvalStateOf(d.Content)
		}
	}
	if state != "approved" {
		t.Fatalf("重新 AI 生图不得隐式清除锚点审批，state=%q", state)
	}
	block := svc.decisionBlock(sess)
	for _, want := range []string{"【现行决策·按轴】", "动画方式", "视角", "【审批门】"} {
		if !strings.Contains(block, want) {
			t.Fatalf("账本应含 %q:\n%s", want, block)
		}
	}
}

// TestAxisAnswerOnlyKeepsRoute 「你不用开始工作，告诉我这个 skill 干嘛用的」
// 只改执行模式，不撤销动画路线，也不生成任何新路线。
func TestAxisAnswerOnlyKeepsRoute(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-answer", Goal: "做 16 帧图集奔跑"}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective,
		"你不用开始工作，你告诉我这个 skill 干嘛用的", "user_message")

	axes := activeAxes(sess)
	if _, ok := axes[store.AxisAnimationMethod]; !ok {
		t.Fatalf("回答类指令不得撤销动画路线，axes=%v", axes)
	}
	if got := axes[store.AxisExecutionMode]; !strings.Contains(got, "不用开始工作") {
		t.Fatalf("应登记执行模式轴，axes=%v", axes)
	}
	block := svc.decisionBlock(sess)
	if !strings.Contains(block, "执行模式") {
		t.Fatalf("账本应展示执行模式轴:\n%s", block)
	}
}

// TestAxisContinueInherits 「继续」不新增路线，继承当前状态。
func TestAxisContinueInherits(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-continue", Goal: "做 16 帧图集奔跑"}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	before := activeAxes(sess)
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "继续", "user_message")
	after := activeAxes(sess)
	if len(after) != len(before) {
		t.Fatalf("\"继续\"不得新增/替换路线，before=%v after=%v", before, after)
	}
	if after[store.AxisAnimationMethod] != before[store.AxisAnimationMethod] {
		t.Fatalf("\"继续\"应继承当前路线，before=%v after=%v", before, after)
	}
	for _, d := range sess.decisions.items {
		if d.Kind == store.DecisionKindRoute && d.Content == "继续" {
			t.Fatalf("\"继续\"不应被登记为路线行: %+v", d)
		}
	}
}

// TestAxisRouteSwitchOnlySameAxis A→B→非视频：只有 animation_method 轴被替换，
// 其他轴（视角/审批/验收）保持有效。
func TestAxisRouteSwitchOnlySameAxis(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-switch", Goal: "异兽奔跑"}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "用短视频抽帧来生成奔跑动画", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "视角保持侧视", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "改用 16 帧图集逐帧生成", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "不走视频抽帧了，用已有素材", "user_message")

	axes := activeAxes(sess)
	if got := axes[store.AxisAnimationMethod]; !strings.Contains(got, "不走视频抽帧") {
		t.Fatalf("animation_method 轴应被最新指令替换，axes=%v", axes)
	}
	if got, ok := axes[store.AxisView]; !ok || !strings.Contains(got, "侧视") {
		t.Fatalf("视角轴不得被动画方式变更撤销，axes=%v", axes)
	}
	// 旧动画路线应保留为 superseded（审计可查，不物理删）。
	superseded := 0
	for _, d := range sess.decisions.items {
		if d.Kind == store.DecisionKindRoute && d.Status == store.DecisionStatusSuperseded {
			superseded++
		}
	}
	if superseded < 2 {
		t.Fatalf("两条旧动画路线应标 superseded（保留历史），got %d", superseded)
	}
}

// TestAxisSizeFeedbackIsNotRejection 「不对，幅度太小」只是产物反馈，
// 不把整条技术路线标否决。
func TestAxisSizeFeedbackIsNotRejection(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-feedback", Goal: "做 16 帧图集奔跑"}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "不对，幅度太小", "user_message")

	if isNegativeFeedback("不对，幅度太小") {
		t.Fatal("\"幅度太小\"不应判为用户否决")
	}
	for _, d := range sess.decisions.items {
		if d.Kind == store.DecisionKindRejection {
			t.Fatalf("幅度反馈不应写否决行: %+v", d)
		}
	}
	axes := activeAxes(sess)
	if _, ok := axes[store.AxisAnimationMethod]; !ok {
		t.Fatalf("幅度反馈不得否决整条路线，axes=%v", axes)
	}
	block := svc.decisionBlock(sess)
	if strings.Contains(block, "已否决") {
		t.Fatalf("幅度反馈不应出现已否决段:\n%s", block)
	}
}

// TestAxisNewReferenceInvalidatesApproval 换参考图才让旧锚点审批失效（§5.1 规则 6）。
func TestAxisNewReferenceInvalidatesApproval(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-ref", Goal: "画锚点"}
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindDirective, store.AxisApprovalAnchor,
		approvalContent(true, `D:\proj\assets\anchor-a.png`), "asset_gate")
	if got := gateStateFrom(sess, store.AxisApprovalAnchor); got != "approved" {
		t.Fatalf("初始审批应为 approved，got %q", got)
	}
	// 同一张参考图重复确认：批准保留。
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "锚点还是用 `D:\\proj\\assets\\anchor-a.png` 这张", "user_message")
	if got := gateStateFrom(sess, store.AxisApprovalAnchor); got != "approved" {
		t.Fatalf("同参考图不应使审批失效，got %q", got)
	}
	// 换参考图：旧批准失效。
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "改用 `D:\\proj\\assets\\anchor-b.png` 做锚点图", "user_message")
	if got := gateStateFrom(sess, store.AxisApprovalAnchor); got != "pending" {
		t.Fatalf("换参考图后旧批准应失效（pending），got %q", got)
	}
}

func gateStateFrom(sess *reactInternalSession, axis string) string {
	state := "pending"
	for _, d := range sess.decisions.items {
		if d.Kind == store.DecisionKindDirective && d.Axis == axis && d.Status == store.DecisionStatusActive {
			state = approvalStateOf(d.Content)
		}
	}
	return state
}

// TestDecisionBlockBudgetKeepsAxesAndLatest 超预算时必留段（各轴+最新指令）仍在，
// 且不出现 "(truncated)"。
func TestDecisionBlockBudgetKeepsAxesAndLatest(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-budget", Goal: strings.Repeat("长任务书段落，", 200)}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "视角按正面 45 度", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "动画用 16 帧图集", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "素材来源用已有素材", "user_message")
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindDirective, store.AxisApprovalAnchor,
		approvalContent(true, "assets/img/anchor.png"), "asset_gate")
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindCriteria, "",
		strings.Repeat("验收：邻帧不抖、无缺帧。", 40), "plan_approved")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "最新指令：先把锚点图给我看", "user_message")

	block := svc.decisionBlock(sess)
	if n := utf8.RuneCountInString(block); n > decisionBlockMaxRunes+40 {
		t.Fatalf("账本超预算：%d runes > %d", n, decisionBlockMaxRunes)
	}
	if strings.Contains(block, "(truncated)") {
		t.Fatalf("账本不得出现 truncated 字样:\n%s", block)
	}
	for _, want := range []string{"【现行决策·按轴】", "动画方式", "视角", "【审批门】", "先把锚点图给我看"} {
		if !strings.Contains(block, want) {
			t.Fatalf("超预算后仍应保留 %q:\n%s", want, block)
		}
	}
}

// TestClassifyDecisionAxis 轴分类单测（判错轴比判错路线轻，但基础场景必须对）。
func TestClassifyDecisionAxis(t *testing.T) {
	cases := map[string]string{
		"你不用开始工作，你告诉我这个 skill 干嘛用的": store.AxisExecutionMode,
		"重新 AI 生图，不要代码在那调试了":        store.AxisAssetSource,
		"用短视频抽帧吧":                   store.AxisAnimationMethod,
		"不走视频抽帧了":                   store.AxisAnimationMethod,
		"视角改成正面 45 度":               store.AxisView,
		"用 D 版":                     store.AxisIdentity,
		"用 D 版，但是还需要微调":             store.AxisIdentity,
		"这一版不要写测试":                  store.AxisGeneral, // 2026-10-24：裸“版”不再当身份轴（否则普通任务句会误撤角色合同）
		"继续":                        store.AxisGeneral,
		"幅度太小了":                     store.AxisGeneral,
	}
	for content, want := range cases {
		if got := classifyDecisionAxis(content); got != want {
			t.Errorf("classifyDecisionAxis(%q) = %q, want %q", content, got, want)
		}
	}
}

// TestParseAssetGateClarify 资产审批提问解析（2026-10-24）：只有固定题头 + 「产物：」
// 行才视为审批门登记；自由提问不硬造审批。
func TestParseAssetGateClarify(t *testing.T) {
	pc := &ClarifyRequest{
		Question: "【资产审批·anchor】锚点是否通过？",
		Detail:   "角色合同：墨黑兽体+赤褐云纹\n产物：assets/img/beast-anchor.png\n",
	}
	stage, ref, ok := parseAssetGateClarify(pc)
	if !ok || stage != "anchor" || ref != "assets/img/beast-anchor.png" {
		t.Fatalf("解析失败: stage=%q ref=%q ok=%v", stage, ref, ok)
	}
	pc2 := &ClarifyRequest{Question: "【资产审批·keyframe】关键姿态可以吗？", Detail: "产物：assets/img/kf-1.png"}
	if stage, ref, ok := parseAssetGateClarify(pc2); !ok || stage != "keyframe" || ref != "assets/img/kf-1.png" {
		t.Fatalf("keyframe 解析失败: %q %q %v", stage, ref, ok)
	}
	if _, _, ok := parseAssetGateClarify(&ClarifyRequest{Question: "随便问一句", Detail: "产物：x.png"}); ok {
		t.Fatal("非资产审批提问不应被解析为审批门")
	}
	if _, _, ok := parseAssetGateClarify(nil); ok {
		t.Fatal("nil 提问不应被解析")
	}
}

// TestApprovalAxisSingleActive 审批门同轴只留最新一条：重新审批/改判会把旧审批标
// superseded（append-only，不物理删），CurrentMediaGateState 取最新状态。
func TestApprovalAxisSingleActive(t *testing.T) {
	svc := newAxisTestService()
	sess := &reactInternalSession{ID: "sess-gate", Goal: "画锚点"}
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindDirective, store.AxisApprovalAnchor,
		approvalContent(true, "assets/img/a.png"), "asset_gate")
	svc.recordDecisionAxis(t.Context(), sess, store.DecisionKindDirective, store.AxisApprovalAnchor,
		approvalContent(false, "assets/img/a.png"), "asset_gate")

	active, superseded := 0, 0
	for _, d := range sess.decisions.items {
		if d.Axis != store.AxisApprovalAnchor {
			continue
		}
		switch d.Status {
		case store.DecisionStatusActive:
			active++
		case store.DecisionStatusSuperseded:
			superseded++
		}
	}
	if active != 1 || superseded != 1 {
		t.Fatalf("审批轴应只留 1 条 active（+1 superseded），got active=%d superseded=%d", active, superseded)
	}
	if got := gateStateFrom(sess, store.AxisApprovalAnchor); got != "rejected" {
		t.Fatalf("改判后状态应为 rejected，got %q", got)
	}
}
