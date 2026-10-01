// Package agent 包含「工具调用之间的中间正文（assistant_text）落事件」的单元测试。
//
// 背景（2026-09-13 用户实证）：模型"口播一句→调工具"时，那段正文只活在瞬时 StreamingText，
// 下一个轮次的 delta 直接覆盖、前端 live 行也在工具调用时清掉，用户看到话刚出现就消失。
package agent

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// countEventsByKind 统计会话事件中指定 Kind 的条数。
func countEventsByKind(events []internalEvent, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// TestPersistInterimText_OnToolCall 验证工具调用边界把流式正文落成 assistant_text 事件：
// 内容/归属正确，且 StreamingText 保留（ask_user/审批 hook 还要用它做提问正文快照）。
func TestPersistInterimText_OnToolCall(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setStreamingText(sess, "游戏启动脚本已热修，容器正在重启。等 wine 冷启动后我立即验证。")
	svc.store.setThinkingText(sess, "先确认脚本改对了")

	svc.handleLiveEvent(sess, LiveEvent{
		Kind:    LiveEventToolCall,
		Agent:   "MetaAgent",
		AgentID: "session-1",
		Tool:    "RunCommand",
	})

	ev := findEventByKind(sess.Events, eventkind.AssistantText)
	if ev == nil {
		t.Fatalf("工具调用时应落 assistant_text 事件，events=%v", sess.Events)
	}
	if ev.Message != "游戏启动脚本已热修，容器正在重启。等 wine 冷启动后我立即验证。" {
		t.Fatalf("assistant_text 内容不符，got %q", ev.Message)
	}
	if ev.Agent != "MetaAgent" || agentIDFromDetail(ev.DetailJSON) != "session-1" {
		t.Fatalf("assistant_text 归属不符，got agent=%q detail=%q", ev.Agent, ev.DetailJSON)
	}
	if ev.Type != eventkind.Message {
		t.Fatalf("assistant_text 的 Type 应为 message，got %q", ev.Type)
	}
	if sess.StreamingText == "" {
		t.Fatal("落事件后 StreamingText 必须保留（ask_user/审批 hook 还要用它做提问正文快照）")
	}
	// 同一边界仍应落 think 事件（思考与正文本就是两条独立留存通道）。
	if findEventByKind(sess.Events, eventkind.Think) == nil {
		t.Fatal("工具调用时应同时落 think 事件")
	}
}

// TestPersistInterimText_ParallelToolCallsOnce 验证并行工具调用连发多条 ToolCall 事件时
// 只落一条 assistant_text（流式文本未变，重复落会刷屏）。
func TestPersistInterimText_ParallelToolCallsOnce(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setStreamingText(sess, "同时读两个文件")

	for _, name := range []string{"ReadFile", "ListDir", "ReadFile"} {
		svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: "session-1", Tool: name})
	}
	if n := countEventsByKind(sess.Events, eventkind.AssistantText); n != 1 {
		t.Fatalf("同一段正文只应落一条 assistant_text，got %d", n)
	}

	// 下一轮正文（内容不同）照常各落一条。
	svc.store.setStreamingText(sess, "两个文件都读完了，开始改代码")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: "session-1", Tool: "EditFile"})
	if n := countEventsByKind(sess.Events, eventkind.AssistantText); n != 2 {
		t.Fatalf("新一轮正文应再落一条，got %d", n)
	}
}

// TestPersistInterimText_Skips 验证空正文与 ask_user 不落事件。
func TestPersistInterimText_Skips(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)

	// 空正文：不落。
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", Tool: "ReadFile"})
	if n := countEventsByKind(sess.Events, eventkind.AssistantText); n != 0 {
		t.Fatalf("空正文不应落 assistant_text，got %d", n)
	}

	// ask_user：正文由提问事件的 report_text 承载，这里跳过（否则对话栏重复展示同一段话）。
	svc.store.setStreamingText(sess, "我先问清楚需求再动手")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: "session-1", Tool: "ask_user"})
	if n := countEventsByKind(sess.Events, eventkind.AssistantText); n != 0 {
		t.Fatalf("ask_user 不应落 assistant_text，got %d", n)
	}
}

// TestPersistInterimText_SubAgentPrefixStripped 验证子 Agent 正文的【展示名】前缀被剥离。
func TestPersistInterimText_SubAgentPrefixStripped(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setStreamingText(sess, "【代码助手】\n正在重构渲染循环")

	svc.handleLiveEvent(sess, LiveEvent{
		Kind:    LiveEventToolCall,
		Agent:   "代码助手",
		AgentID: "session-1/code_assistant-5",
		Tool:    "EditFile",
	})

	ev := findEventByKind(sess.Events, eventkind.AssistantText)
	if ev == nil {
		t.Fatal("子 Agent 正文也应落 assistant_text 事件")
	}
	if ev.Message != "正在重构渲染循环" {
		t.Fatalf("应剥离【Agent】前缀，got %q", ev.Message)
	}
	if agentIDFromDetail(ev.DetailJSON) != "session-1/code_assistant-5" {
		t.Fatalf("DetailJSON 应含子 Agent 实例 ID，got %q", ev.DetailJSON)
	}
}

// TestTrimDebugEvents_KeepsAssistantText 验证头部裁剪只丢调试类事件，
// assistant_text（用户要看的正文）不在裁剪范围内。
func TestTrimDebugEvents_KeepsAssistantText(t *testing.T) {
	events := []internalEvent{
		{Kind: eventkind.AssistantText, Message: "第一轮正文"},
		{Kind: eventkind.Think, Message: "第一轮思考"},
		{Kind: eventkind.Prompt, Message: "prompt"},
		{Kind: eventkind.TokenUsage, Message: "token"},
		{Kind: eventkind.GraphStep, Message: "step"},
	}
	out := trimDebugEvents(events, 10)
	if len(out) != 1 || out[0].Kind != eventkind.AssistantText {
		t.Fatalf("裁剪后应只留 assistant_text，got %v", out)
	}
}

// TestThinkDelta_ClearsStaleStreamingText 验证新一轮思考开始时清掉已落盘的口播正文
//（2026-10-01 用户实证：正文落 assistant_text 事件后 StreamingText 仍残留，思考更新
// 触发的 SSE live 帧把它一并重推，前端已清掉的 live 行被"复活"，与落盘正文同屏重复
// 且定格不动）。ask_user/审批 hook 保留的提问正文（≠lastInterimText）不得误清。
func TestThinkDelta_ClearsStaleStreamingText(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setStreamingText(sess, "按批准的计划执行。先精确定位归属小节")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: "session-1", Tool: "SearchInFiles"})
	if findEventByKind(sess.Events, eventkind.AssistantText) == nil {
		t.Fatal("工具调用边界应落 assistant_text 事件")
	}

	// 新一轮思考开始：已落盘正文从瞬时字段清掉，思考文本照常更新。
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventThinkDelta, Agent: "MetaAgent", AgentID: "session-1", Text: "思考下一步"})
	if sess.StreamingText != "" {
		t.Fatalf("已落盘口播正文应在思考轮清空，got %q", sess.StreamingText)
	}
	if sess.ThinkingText != "思考下一步" {
		t.Fatalf("思考文本应照常更新，got %q", sess.ThinkingText)
	}

	// ask_user 保留的提问正文（未落 assistant_text，≠lastInterimText）不得误清。
	svc.store.setStreamingText(sess, "开始前需要确认：目标目录用哪个？")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventThinkDelta, Agent: "MetaAgent", AgentID: "session-1", Text: "继续思考"})
	if sess.StreamingText != "开始前需要确认：目标目录用哪个？" {
		t.Fatalf("提问正文不得在思考轮被清，got %q", sess.StreamingText)
	}
}

// TestThinkDelta_ClearsStaleStreamingText_SubAgent 验证子 Agent 场景：StreamingText 带
// 【展示名】前缀而 lastInterimText 是剥前缀后的落盘文本，比较时同样剥前缀才能命中清理。
func TestThinkDelta_ClearsStaleStreamingText_SubAgent(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setStreamingText(sess, "【代码助手】\n正在重构渲染循环")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "代码助手", AgentID: "session-1/code_assistant-5", Tool: "EditFile"})

	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventThinkDelta, Agent: "代码助手", AgentID: "session-1/code_assistant-5", Text: "思考中"})
	if sess.StreamingText != "" {
		t.Fatalf("子 Agent 已落盘口播正文应在思考轮清空，got %q", sess.StreamingText)
	}
}

// TestClusterTopNarration_Suppressed 验证集群档顶层 Meta 的中间轮口播与用户流完全隔离
//（2026-09-18 用户实证：「已确认根因：环境性失败…我注意到自己可用技能中有…」这类
// 编排内心独白原样出现在对话栏）：子 Agent 在跑时 LLMDelta 只进轮缓冲不推 StreamingText，
// 工具调用边界丢弃且不落 assistant_text 事件。无子 Agent 运行时的直推语义见
// TestClusterTopNarration_NoSubAgentsStreamed。
func TestClusterTopNarration_Suppressed(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.setGear("cluster")
	// 编排口播的前提是"正在编排"：树上有运行中的子 Agent 节点。
	svc.TreeFor(sess.ID).Register(orchestrator.Node{
		ID: sess.ID + "/domain-1", ParentID: sess.ID, Role: "domain", Status: orchestrator.StatusRunning,
	})

	// 顶层 Meta 流式输出中间轮口播（AgentID=会话 ID 即顶层实例；Agent 展示名
	// "MetaAgent" 子 Agent 也有，不能用作判定依据）。
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "已确认根因：环境性失败——重派同类子 agent 必然同样失败"})
	if sess.StreamingText != "" {
		t.Fatalf("集群档顶层口播不得推 StreamingText，got %q", sess.StreamingText)
	}
	if sess.pendingTopText == "" {
		t.Fatal("口播应进轮缓冲 pendingTopText")
	}

	// 工具调用边界：缓冲丢弃、不落 assistant_text、不进 StreamingText。
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: sess.ID, Tool: "call_sub_agent"})
	if n := countEventsByKind(sess.Events, eventkind.AssistantText); n != 0 {
		t.Fatalf("集群档顶层口播不得落 assistant_text 事件，got %d", n)
	}
	if sess.StreamingText != "" || sess.pendingTopText != "" {
		t.Fatalf("工具调用边界应丢弃缓冲，stream=%q pending=%q", sess.StreamingText, sess.pendingTopText)
	}
}

// TestClusterTopNarration_NoSubAgentsStreamed 验证直推语义（2026-09-20）：集群档顶层
// Meta 在子 Agent 全部结束后输出的文本即终答/直接汇报，实时直推 StreamingText——
// 否则终答生成期（可达数分钟）前端 live 行恒为空，只剩转圈占位。直推同时清空轮缓冲，
// ask_user 边界/完成时的旧冲刷路径不会把已清空的缓冲再覆盖回去。
func TestClusterTopNarration_NoSubAgentsStreamed(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.setGear("cluster")

	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "终答：三处热修已全部完成并验证"})
	if sess.StreamingText != "终答：三处热修已全部完成并验证" {
		t.Fatalf("无子 Agent 时顶层正文应直推 StreamingText，got %q", sess.StreamingText)
	}
	if sess.pendingTopText != "" {
		t.Fatalf("直推时轮缓冲应保持为空，got %q", sess.pendingTopText)
	}

	// 子 Agent 开跑后回到缓冲抑制语义。
	svc.TreeFor(sess.ID).Register(orchestrator.Node{
		ID: sess.ID + "/domain-1", ParentID: sess.ID, Role: "domain", Status: orchestrator.StatusRunning,
	})
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "已派出子 Agent 继续排查"})
	if sess.StreamingText != "终答：三处热修已全部完成并验证" {
		t.Fatalf("子 Agent 在跑时口播不得覆盖 StreamingText，got %q", sess.StreamingText)
	}
	if sess.pendingTopText != "已派出子 Agent 继续排查" {
		t.Fatalf("子 Agent 在跑时口播应进轮缓冲，got %q", sess.pendingTopText)
	}
}

// TestClusterTopNarration_AskUserFlush 验证 ask_user 例外：提问正文要供提问卡上方展示
//（clarifyReportJSON 读 StreamingText），轮缓冲在 ask_user 工具调用边界冲刷保留。
// 直推模式（无子 Agent 在跑）下 StreamingText 本就是实时正文，冲刷不得把它清空。
func TestClusterTopNarration_AskUserFlush(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.setGear("cluster")
	// 子 Agent 在跑：提问正文进轮缓冲，ask_user 边界冲刷。
	svc.TreeFor(sess.ID).Register(orchestrator.Node{
		ID: sess.ID + "/domain-1", ParentID: sess.ID, Role: "domain", Status: orchestrator.StatusRunning,
	})

	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "开始前需要确认：目标目录用哪个？"})
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", AgentID: sess.ID, Tool: "ask_user"})
	if sess.StreamingText != "开始前需要确认：目标目录用哪个？" {
		t.Fatalf("ask_user 边界应冲刷提问正文进 StreamingText，got %q", sess.StreamingText)
	}

	// 用户答复后恢复路径会清 StreamingText（2026-09-09 修复，service_react askUser 钩子）；
	// 此后新一轮口播继续只进缓冲，不把旧提问正文当实时流重推。
	svc.store.setStreamingText(sess, "")
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "收到，继续处理"})
	if sess.StreamingText != "" {
		t.Fatalf("恢复后口播不得重推 StreamingText，got %q", sess.StreamingText)
	}
}

// TestClusterTopNarration_FinalAnswerFlush 验证 run 完成时终答缓冲冲刷进 StreamingText
//（与 agent_done 事件同 tick 推送），非集群档不受影响。
func TestClusterTopNarration_FinalAnswerFlush(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.setGear("cluster")

	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: sess.ID, Text: "最终答复：三处热修已全部完成并验证"})
	svc.flushPendingTopText(sess)
	if sess.StreamingText != "最终答复：三处热修已全部完成并验证" {
		t.Fatalf("完成时应冲刷终答进 StreamingText，got %q", sess.StreamingText)
	}
	if sess.pendingTopText != "" {
		t.Fatal("冲刷后缓冲应清空")
	}

	// 日常档（非集群）：LLMDelta 照常直推 StreamingText，缓冲不启用。
	svc2, sess2 := newLiveEventTestSession(t)
	svc2.handleLiveEvent(sess2, LiveEvent{Kind: LiveEventLLMDelta, Text: "日常档中间正文"})
	if sess2.StreamingText != "日常档中间正文" {
		t.Fatalf("日常档 LLMDelta 应直推 StreamingText，got %q", sess2.StreamingText)
	}
	svc2.handleLiveEvent(sess2, LiveEvent{Kind: LiveEventToolCall, Agent: "MetaAgent", Tool: "ReadFile"})
	if findEventByKind(sess2.Events, eventkind.AssistantText) == nil {
		t.Fatal("日常档中间正文应照常落 assistant_text 事件")
	}
}
