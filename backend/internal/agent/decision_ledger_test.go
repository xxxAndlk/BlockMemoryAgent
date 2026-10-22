package agent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/store"
)

func newLedgerTestSession() *reactInternalSession {
	return &reactInternalSession{ID: "sess-test", Goal: "做一个异兽的奔跑完整版，使用骨架拼接"}
}

// TestDecisionLedgerRouteSupersede 验证路线级指令会废止上一条路线，且账本块同时
// 呈现"现行路线"和"已废止"——2026-10-22 事故里模型只看到陈旧目标，看不到改向。
func TestDecisionLedgerRouteSupersede(t *testing.T) {
	svc := &ReactService{}
	sess := newLedgerTestSession()
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "继续之前的16帧图集，用图集序列帧渲染", "user_message")

	var active, superseded int
	for _, d := range sess.decisions.items {
		if d.Kind != store.DecisionKindRoute {
			continue
		}
		switch d.Status {
		case store.DecisionStatusActive:
			active++
		case store.DecisionStatusSuperseded:
			superseded++
		}
	}
	if active != 1 {
		t.Fatalf("应只有 1 条现行路线，got %d", active)
	}
	if superseded != 1 {
		t.Fatalf("旧路线应被标 superseded，got %d", superseded)
	}
	block := svc.decisionBlock(sess)
	if !strings.Contains(block, "16帧图集") {
		t.Fatalf("现行路线应包含最新图集指令:\n%s", block)
	}
	if !strings.Contains(block, "已废止") || !strings.Contains(block, "骨架拼接") {
		t.Fatalf("已废止路线应保留并标注:\n%s", block)
	}
	if !strings.Contains(block, "最高优先级") {
		t.Fatalf("账本块应声明优先级:\n%s", block)
	}
}

// TestDecisionLedgerNegativeFeedback 验证用户否定会把现行路线标 rejected，
// 防止模型"用户骂了但继续在旧路线上打补丁"。
func TestDecisionLedgerNegativeFeedback(t *testing.T) {
	svc := &ReactService{}
	sess := newLedgerTestSession()
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "使用骨架拼接做奔跑", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "你做的什么玩意？为什么做的这么差", "user_message")

	block := svc.decisionBlock(sess)
	if !strings.Contains(block, "已否决") {
		t.Fatalf("账本应标记已否决路线:\n%s", block)
	}
	if !strings.Contains(block, "禁止继续") {
		t.Fatalf("否决路线应带禁止继续提示:\n%s", block)
	}
}

// TestDecisionLedgerReference 验证用户给的参考产物路径独立入账为验收基准。
func TestDecisionLedgerReference(t *testing.T) {
	svc := &ReactService{}
	sess := newLedgerTestSession()
	svc.recordDecision(t.Context(), sess, store.DecisionKindClarify,
		`"D:\WebData\demo\moyingshanhai\public\assets\video\REF.mp4" 要到这种类型，你做的什么玩意？`,
		"clarify_answer")
	block := svc.decisionBlock(sess)
	if !strings.Contains(block, "验收基准") || !strings.Contains(block, "REF.mp4") {
		t.Fatalf("参考产物应作为验收基准入账:\n%s", block)
	}
}

// TestPlanConflictWarning 验证计划与现行路线互斥时给出审批前警示。
func TestPlanConflictWarning(t *testing.T) {
	svc := &ReactService{}
	sess := newLedgerTestSession()
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "继续之前的16帧图集，用图集序列帧", "user_message")
	warn := svc.planConflictWarning(sess,
		"目标：产出 run-rig.html，把一只异兽的奔跑做成骨架拼接演示（替代此前的 16 帧整图序列）。",
		"【计划确认】做异兽「狰」骨架拼接奔跑完整版")
	if warn == "" {
		t.Fatal("骨架拼接计划与 16 帧图集路线互斥，应给出冲突警示")
	}
	if !strings.Contains(warn, "决策冲突") {
		t.Fatalf("警示文案应含决策冲突: %s", warn)
	}
	// 同路线计划不应误报。
	svc2 := &ReactService{}
	sess2 := newLedgerTestSession()
	svc2.recordDecision(t.Context(), sess2, store.DecisionKindDirective, "继续之前的16帧图集，用图集序列帧", "user_message")
	if w := svc2.planConflictWarning(sess2, "把16帧打包成图集，按 rect 取帧渲染", "【计划确认】16帧图集"); w != "" {
		t.Fatalf("同路线计划不应误报冲突: %s", w)
	}
}

// TestPlanConflictIgnoresNegatedRoute 回归 2026-10-22 23:06 误报：
// 任务书用「明确排除：骨架拼接、部位拼接、Spine」写死禁行方案，主题词统计若把否定句
// 当正面命中，现行路线会被判成骨架组，16 帧图集计划反而被判"属于另一条互斥路线"。
func TestPlanConflictIgnoresNegatedRoute(t *testing.T) {
	goal := `# 任务：异兽奔跑动画（16帧图集序列帧版）

## 一、路线（已定，不得更改）

与《向僵尸开炮》同款的**16帧图集序列帧 + 动作表驱动**。

**明确排除**：骨架拼接、部位拼接、Spine、骨骼动画、骨骼蒙皮。
「骨架拼接」是早期口头表述，实测已否决（关节必然裂缝，刚性旋转物理上不可能接出连续肢体）。

若项目偏好或用户历史表述与本节冲突，**以本节为准**，不要重新讨论方案。`
	plan := `## 目标
在同一工作目录，用已有素材（禁 AI 重生图、禁拆部位）交付《向僵尸开炮》同款 16 帧图集序列帧奔跑动画的完整闭环。

## 步骤
1. 基线比对：frames16/ vs .workbuddy/anim/run/
2. 单张横向图集 + 动作表 JSON`
	svc := &ReactService{}
	sess := &reactInternalSession{ID: "sess-negated", Goal: goal}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, goal, "goal")
	if w := svc.planConflictWarning(sess, plan, "【计划确认】16帧图集序列帧版"); w != "" {
		t.Fatalf("16 帧图集计划与任务书同路线，不应误报冲突: %s", w)
	}
	// 真互斥仍要报：计划只提骨架、完全没提图集/序列帧。
	if w := svc.planConflictWarning(sess, "把异兽奔跑做成骨架拼接演示，用骨骼动画驱动", "【计划确认】骨架拼接"); w == "" {
		t.Fatal("骨架拼接计划与 16 帧图集路线互斥，必须警示")
	}
}

// TestDecisionBlockRouteHeadline 验证权威块不再整篇复制任务书、也不出现
// "(truncated)" 字样——那会让模型以为服务端藏着全文，转去文件系统里找。
func TestDecisionBlockRouteHeadline(t *testing.T) {
	goal := `# 任务：异兽奔跑动画（16帧图集序列帧版）

工作目录：` + "`D:\\WebData\\demo\\moyingshanhai`" + `

## 一、路线（已定，不得更改）

与《向僵尸开炮》同款的**16帧图集序列帧 + 动作表驱动**。明确排除骨架拼接。

## 二、开工前必做

先跑一次资产扫描，再动任何代码。`
	svc := &ReactService{}
	sess := &reactInternalSession{ID: "sess-headline", Goal: goal}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, goal, "goal")
	block := svc.decisionBlock(sess)
	if strings.Contains(block, "(truncated)") {
		t.Fatalf("权威块不得出现 truncated 字样:\n%s", block)
	}
	if !strings.Contains(block, "【现行路线】任务：异兽奔跑动画") {
		t.Fatalf("现行路线应以标题开头:\n%s", block)
	}
	if !strings.Contains(block, "任务书全文见会话首条用户消息") {
		t.Fatalf("摘录版应指向首条 user 消息里的任务书全文:\n%s", block)
	}
	// 不应该把整篇任务书（含第二节）搬进账本。
	if strings.Contains(block, "先跑一次资产扫描") {
		t.Fatalf("账本只摘路线，不应复制任务书全文:\n%s", block)
	}
}

// TestEnsureGoalMessage 验证续跑历史缺任务书时会把 goal 补回首位（且不重复补）。
func TestEnsureGoalMessage(t *testing.T) {
	goal := "# 任务：异兽奔跑动画（16帧图集序列帧版）"
	hist := []ReactMessage{{Role: "user", Content: "继续"}}
	got := ensureGoalMessage(hist, goal)
	if len(got) != 2 || got[0].Role != "user" || got[0].Content != goal {
		t.Fatalf("应把任务书补到首位，got %+v", got)
	}
	again := ensureGoalMessage(got, goal)
	if len(again) != len(got) {
		t.Fatalf("已存在任务书时不得重复补，got %d", len(again))
	}
	if empty := ensureGoalMessage(hist, ""); len(empty) != 1 {
		t.Fatalf("空 goal 不应改动历史，got %d", len(empty))
	}
}

// TestExtractReferencePaths 验证参考产物路径提取（去重、上限 3）。
func TestExtractReferencePaths(t *testing.T) {
	got := extractReferencePaths(`看 D:\a\b.mp4 和 /tmp/c.png，再对比 /tmp/c.png 与 D:\x\y.html`)
	if len(got) != 3 {
		t.Fatalf("应提取 3 条去重路径，got %v", got)
	}
	if got[0] != `D:\a\b.mp4` {
		t.Fatalf("首条路径不对: %v", got)
	}
}

// TestIsRouteDirective 验证路线判定不会把普通动作句（"继续""渲染给我看"）误判为改向。
func TestIsRouteDirective(t *testing.T) {
	yes := []string{"使用骨架拼接", "继续之前的16帧图集", "不要用骨骼，改用图集", "这一版要用引擎渲染"}
	no := []string{"继续", "渲染给我看啊", "把视频给我看", "看看当前项目有什么skill"}
	for _, s := range yes {
		if !isRouteDirective(s) {
			t.Errorf("应判为路线级指令: %q", s)
		}
	}
	for _, s := range no {
		if isRouteDirective(s) {
			t.Errorf("不应判为路线级指令: %q", s)
		}
	}
}

// TestDecisionLedgerIncidentScenario 用 2026-10-22 事故原句做端到端账本回归：
// 目标(骨架) → 用户改向(16帧图集) → 用户确认帧渲染 → 用户给参考产物并否决 → 追问渲染。
// 账本必须同时呈现：现行/已否决路线、验收基准、行动要求、最新指令。
func TestDecisionLedgerIncidentScenario(t *testing.T) {
	svc := &ReactService{}
	sess := &reactInternalSession{
		ID:   "session-incident",
		Goal: "做一个异兽的奔跑完整版，使用骨架拼接，先Ai生一张图再进行部位拼接进行奔跑动作，根据地图视角适度的倾斜俯视",
	}
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, sess.Goal, "goal")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "继续之前的16帧图集", "user_message")
	svc.recordDecision(t.Context(), sess, store.DecisionKindClarify,
		"你用之前的渲染方式用帧渲染做一个看起来连续的视频就行，这是正确做法，向僵尸开炮就是使用引擎进行渲染", "clarify_answer")
	svc.recordDecision(t.Context(), sess, store.DecisionKindClarify,
		`"D:\WebData\demo\moyingshanhai\public\assets\video\HARD_LOCKED_STATIC_CAMERA__Abs.mp4"要到这种类型，你做的什么玩意？`, "clarify_answer")
	svc.recordDecision(t.Context(), sess, store.DecisionKindDirective, "渲染给我看啊", "user_message")

	block := svc.decisionBlock(sess)
	for _, want := range []string{"16帧图集", "已否决", "行动要求", "验收基准", "HARD_LOCKED", "渲染给我看啊"} {
		if !strings.Contains(block, want) {
			t.Fatalf("账本应包含 %q:\n%s", want, block)
		}
	}
	// 骨架路线已被"继续之前的16帧图集"废止，不能作为现行路线出现。
	if strings.Contains(block, "【现行路线】做一个异兽的奔跑完整版，使用骨架拼接") {
		t.Fatalf("陈旧目标不应成为现行路线:\n%s", block)
	}
	// 计划审批：此时的骨架计划必须触发冲突警示。
	warn := svc.planConflictWarning(sess, "目标：产出 run-rig.html，把奔跑做成骨架拼接演示（替代此前的 16 帧整图序列）", "【计划确认】骨架拼接奔跑完整版")
	if warn == "" {
		t.Fatal("事故场景下骨架计划必须触发冲突警示")
	}
}
