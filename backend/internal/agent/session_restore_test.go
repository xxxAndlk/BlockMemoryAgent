package agent

// 会话持久化与恢复测试。
// 分两层:
//  1. 纯函数单测（restoredSessionStatus / mergeSessionLists），无 PG 依赖；
//  2. PG 集成测试（BMA_TEST_PG_DSN 门控），覆盖 upsert 保留最新行、事件
//     delete-then-insert 幂等、懒恢复（中断标记 + History 重建 + 续跑上下文延续）。
//     集成测试指向 docker/docker-compose.test.yml 实例（独立端口，勿指向开发实例）:
//     postgres://blockmemory:blockmemory_dev@localhost:55432/blockmemory?sslmode=disable

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/blades"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// --- 纯函数单测 ---

// TestRestoredSessionStatus 验证 session_history.status 列到内存状态的映射:
// running → error + 中断提示；空串/未知值 → completed 兜底；其余状态原样保留。
func TestRestoredSessionStatus(t *testing.T) {
	cases := []struct {
		stored       string
		summary      string
		wantStatus   enums.SessionStatus
		wantResult   string
		wantInterrupted bool
	}{
		// 进程死亡遗留的运行态: 标记为中断。
		{stored: "running", summary: "任意", wantStatus: enums.SessionStatusError, wantResult: interruptedByRestartMsg, wantInterrupted: true},
		// 009 迁移前的旧数据(空串): 按 completed 兜底。
		{stored: "", summary: "旧总结", wantStatus: enums.SessionStatusCompleted, wantResult: "旧总结"},
		// 已知终态: 原样保留。
		{stored: "completed", summary: "s1", wantStatus: enums.SessionStatusCompleted, wantResult: "s1"},
		{stored: "error", summary: "boom", wantStatus: enums.SessionStatusError, wantResult: "boom"},
		{stored: "awaiting_clarify", summary: "s2", wantStatus: enums.SessionStatusAwaitingClarify, wantResult: "s2"},
		{stored: "paused_on_child", summary: "s3", wantStatus: enums.SessionStatusPausedOnChild, wantResult: "s3"},
		// 无法识别的值: completed 兜底。
		{stored: "weird", summary: "s4", wantStatus: enums.SessionStatusCompleted, wantResult: "s4"},
	}
	for _, tc := range cases {
		status, result, interrupted := restoredSessionStatus(tc.stored, tc.summary)
		if status != tc.wantStatus {
			t.Errorf("restoredSessionStatus(%q) status = %q, want %q", tc.stored, status, tc.wantStatus)
		}
		if result != tc.wantResult {
			t.Errorf("restoredSessionStatus(%q) result = %q, want %q", tc.stored, result, tc.wantResult)
		}
		if interrupted != tc.wantInterrupted {
			t.Errorf("restoredSessionStatus(%q) interrupted = %v, want %v", tc.stored, interrupted, tc.wantInterrupted)
		}
	}
}

// TestMergeSessionLists 验证列表合并: 内存优先去重、StartedAt 降序、limit 截断。
func TestMergeSessionLists(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	mid := time.Now().Add(-1 * time.Hour)
	now := time.Now()
	mk := func(id string, at time.Time) *Session {
		return &Session{ID: id, StartedAt: at}
	}
	// 内存: mid;库: now / mid 重复 / old。期望合并为 now, mid, old。
	memory := []*Session{mk("session-m", mid)}
	fromDB := []*Session{mk("session-n", now), mk("session-m", mid), mk("session-o", old)}
	got := mergeSessionLists(memory, fromDB, 0)
	if len(got) != 3 {
		t.Fatalf("合并后应有 3 条(同 ID 去重), got %d", len(got))
	}
	wantOrder := []string{"session-n", "session-m", "session-o"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("排序[%d] = %q, want %q", i, got[i].ID, w)
		}
	}
	// 截断: limit=2 只保留最新两条。
	got = mergeSessionLists(memory, fromDB, 2)
	if len(got) != 2 || got[0].ID != "session-n" || got[1].ID != "session-m" {
		t.Errorf("limit=2 应截断为最新两条, got %v", got)
	}
}

// --- PG 集成测试 ---

// newPGStoreForRestoreTest 构造集成测试用的 PostgresStore 并确保涉及表结构齐备;
// BMA_TEST_PG_DSN 未设置时跳过(pgStore 为具体类型无法 stub,只能真库验证)。
func newPGStoreForRestoreTest(t *testing.T) *store.PostgresStore {
	t.Helper()
	dsn := os.Getenv("BMA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("BMA_TEST_PG_DSN 未设置,跳过 PG 集成测试(指向 docker/docker-compose.test.yml 实例: postgres://blockmemory:blockmemory_dev@localhost:55432/blockmemory?sslmode=disable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pg, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	// 幂等确保测试涉及的表结构(含 009 status 列 + 唯一索引)。
	for _, ensure := range []func(context.Context) error{
		func(c context.Context) error { return store.EnsureSessionHistorySchema(c, pg.DB()) },
		func(c context.Context) error { return store.EnsureSessionEventsSchema(c, pg.DB()) },
		func(c context.Context) error { return store.EnsureAgentMessagesSchema(c, pg.DB()) },
	} {
		if err := ensure(ctx); err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
	}
	return pg
}

// seedSessionForRestoreTest 向 PG 写入一条完整会话持久化快照(历史行 + 事件 + agent_messages),
// 返回会话 ID。status 控制历史行状态,markers 注入 agent_messages 供上下文断言。
func seedSessionForRestoreTest(t *testing.T, pg *store.PostgresStore, status string, summary string) string {
	t.Helper()
	ctx := context.Background()
	id := fmt.Sprintf("session-test-restore-%d", time.Now().UnixNano())
	rec := &store.SessionHistoryRecord{
		SessionID: id,
		Goal:      "重构用户中心模块",
		Summary:   summary,
		CreatedAt: time.Now(),
		Status:    status,
	}
	if err := pg.SaveSessionHistory(ctx, rec); err != nil {
		t.Fatalf("SaveSessionHistory: %v", err)
	}
	events := []store.SessionEventRecord{
		{SessionID: id, Type: eventkind.System, Agent: "System", Message: "会话启动", Timestamp: time.Now()},
		{SessionID: id, Type: eventkind.UserMessage, Agent: "User", Message: "重构用户中心模块", Timestamp: time.Now()},
	}
	if err := pg.SaveSessionEvents(ctx, id, events); err != nil {
		t.Fatalf("SaveSessionEvents: %v", err)
	}
	history := []ReactMessage{
		{Role: string(enums.ChatRoleUser), Content: "重构用户中心模块:历史锚点结论A"},
		{Role: string(enums.ChatRoleAssistant), Content: "已按锚点结论A完成主体改造"},
	}
	if err := NewPostgresMessagesStore(pg.DB()).SaveMessages(ctx, id, id, history); err != nil {
		t.Fatalf("SaveMessages: %v", err)
	}
	return id
}

// TestSaveHistoryUpsertKeepsLatest 验证 009 upsert: 同 session_id 只保留一行,
// 且 summary/status 始终为最新写入(修复此前每轮插一行、GetHistoryByID 取到旧行的问题)。
func TestSaveHistoryUpsertKeepsLatest(t *testing.T) {
	pg := newPGStoreForRestoreTest(t)
	ctx := context.Background()
	id := fmt.Sprintf("session-test-upsert-%d", time.Now().UnixNano())
	first := &store.SessionHistoryRecord{SessionID: id, Goal: "g", Summary: "第一轮总结", CreatedAt: time.Now(), Status: string(enums.SessionStatusRunning)}
	if err := pg.SaveSessionHistory(ctx, first); err != nil {
		t.Fatalf("first SaveSessionHistory: %v", err)
	}
	second := &store.SessionHistoryRecord{SessionID: id, Goal: "g", Summary: "第二轮总结", CreatedAt: time.Now(), Status: string(enums.SessionStatusCompleted)}
	if err := pg.SaveSessionHistory(ctx, second); err != nil {
		t.Fatalf("second SaveSessionHistory: %v", err)
	}
	rec, err := pg.GetSessionHistoryByID(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("GetSessionHistoryByID: rec=%v err=%v", rec, err)
	}
	if rec.Summary != "第二轮总结" {
		t.Errorf("upsert 应保留最新 summary, got %q", rec.Summary)
	}
	if rec.Status != string(enums.SessionStatusCompleted) {
		t.Errorf("upsert 应保留最新 status, got %q", rec.Status)
	}
	var n int
	if err := pg.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM session_history WHERE session_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("upsert 后应仅 1 行, got %d", n)
	}
}

// TestSaveEventsDeleteThenInsert 验证事件全量覆盖语义: 重复落库不叠加(修复每轮翻倍)。
func TestSaveEventsDeleteThenInsert(t *testing.T) {
	pg := newPGStoreForRestoreTest(t)
	ctx := context.Background()
	id := fmt.Sprintf("session-test-events-%d", time.Now().UnixNano())
	events := []store.SessionEventRecord{
		{SessionID: id, Type: eventkind.System, Agent: "System", Message: "m1", Timestamp: time.Now()},
		{SessionID: id, Type: eventkind.UserMessage, Agent: "User", Message: "m2", Timestamp: time.Now()},
	}
	if err := pg.SaveSessionEvents(ctx, id, events); err != nil {
		t.Fatalf("first SaveSessionEvents: %v", err)
	}
	if err := pg.SaveSessionEvents(ctx, id, events); err != nil {
		t.Fatalf("second SaveSessionEvents: %v", err)
	}
	got, err := pg.GetSessionEvents(ctx, id)
	if err != nil {
		t.Fatalf("GetSessionEvents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("delete-then-insert 后应仍为 2 条, got %d", len(got))
	}
}

// TestRestoreOneSessionInterrupted 验证懒恢复对 running 遗留行的中断标记:
// status=error + 中断提示 + 合成系统事件 + History 自 agent_messages 重建。
func TestRestoreOneSessionInterrupted(t *testing.T) {
	pg := newPGStoreForRestoreTest(t)
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	svc.store.setPostgresStore(pg)
	ctx := context.Background()
	id := seedSessionForRestoreTest(t, pg, string(enums.SessionStatusRunning), "")

	sess := svc.store.restoreOneSession(ctx, id)
	if sess == nil {
		t.Fatal("restoreOneSession 返回 nil,期望恢复成功")
	}
	if sess.Status != enums.SessionStatusError {
		t.Errorf("Status = %q, want error", sess.Status)
	}
	if sess.Result != interruptedByRestartMsg {
		t.Errorf("Result = %q, want %q", sess.Result, interruptedByRestartMsg)
	}
	// 合成中断事件应位于加载事件末尾。
	if n := len(sess.Events); n == 0 || sess.Events[n-1].Type != eventkind.System || sess.Events[n-1].Message != interruptedByRestartMsg {
		t.Errorf("末尾应为中断系统事件, got %+v", sess.Events)
	}
	// History 应自 agent_messages 重建(含历史锚点内容)。
	if len(sess.History) != 2 || !strings.Contains(sess.History[0].Content, "历史锚点结论A") {
		t.Errorf("History 应含 2 条且首条含历史锚点, got %+v", sess.History)
	}
	// 幂等: 二次恢复返回同一指针。
	if again := svc.store.restoreOneSession(ctx, id); again != sess {
		t.Error("二次恢复应返回同一会话指针")
	}
	// Get 走懒恢复: 返回完整快照(非两条消息降级版)。
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(enums.SessionStatusError) || len(got.Events) == 0 {
		t.Errorf("Get 应返回完整快照, status=%q events=%d", got.Status, len(got.Events))
	}
}

// capturingProvider 包装 mockReactModelProvider,捕获最后一次 Generate 的请求,
// 用于断言恢复后的续跑请求确实携带重启前的历史上下文。
type capturingProvider struct {
	mockReactModelProvider
	mu      sync.Mutex
	lastReq *blades.ModelRequest
}

func (c *capturingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	c.mu.Lock()
	c.lastReq = req
	c.mu.Unlock()
	return c.mockReactModelProvider.Generate(ctx, req)
}

// TestLazyRestoreThenSendContinuesContext 验证"任意旧会话可续聊": 对不在内存的
// 旧会话直接 Send 应懒恢复并以完整历史续跑(而非 404),轮结束后库中状态推进为 completed。
func TestLazyRestoreThenSendContinuesContext(t *testing.T) {
	pg := newPGStoreForRestoreTest(t)
	provider := &capturingProvider{mockReactModelProvider: mockReactModelProvider{
		responses: []*blades.Message{blades.AssistantMessage("续跑完成")},
	}}
	svc := newReactServiceForTest(provider, t.TempDir())
	svc.store.setPostgresStore(pg)
	ctx := context.Background()
	// 库中遗留 completed 旧会话,内存为空(模拟重启后未预热)。
	id := seedSessionForRestoreTest(t, pg, string(enums.SessionStatusCompleted), "上一轮总结")

	if err := svc.Send(ctx, id, Message{Role: string(enums.ChatRoleUser), Content: "继续刚才的重构"}); err != nil {
		t.Fatalf("Send(懒恢复路径): %v", err)
	}
	// 轮询等待续跑完成。
	deadline := time.Now().Add(5 * time.Second)
	var got *Session
	var err error
	for time.Now().Before(deadline) {
		got, err = svc.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != string(enums.SessionStatusCompleted) {
		t.Fatalf("续跑未完成: status=%q result=%q", got.Status, got.Result)
	}
	if got.Result != "续跑完成" {
		t.Errorf("Result = %q, want 续跑完成", got.Result)
	}
	// 续跑请求应携带重启前的历史上下文(锚点文本出现在模型请求中)。
	provider.mu.Lock()
	req := provider.lastReq
	provider.mu.Unlock()
	if req == nil {
		t.Fatal("续跑未产生模型调用")
	}
	if !strings.Contains(fmt.Sprintf("%+v", req), "历史锚点结论A") {
		t.Error("续跑请求应包含重启前的历史上下文(历史锚点结论A)")
	}
	// 轮结束后库中该会话状态应推进为 completed。
	rec, err := pg.GetSessionHistoryByID(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("GetSessionHistoryByID: rec=%v err=%v", rec, err)
	}
	if rec.Status != string(enums.SessionStatusCompleted) {
		t.Errorf("轮结束后库中 status = %q, want completed", rec.Status)
	}
}

// TestListMergesDBSessions 验证 List 合并库记录: 内存会话 + 不在内存的库行,
// 同 ID 去重,重启后旧会话仍出现在列表中(running 遗留行映射为 error + 中断提示)。
func TestListMergesDBSessions(t *testing.T) {
	pg := newPGStoreForRestoreTest(t)
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	svc.store.setPostgresStore(pg)
	ctx := context.Background()
	// 库中两条旧会话: 一条 running 遗留,一条 completed。
	interruptedID := seedSessionForRestoreTest(t, pg, string(enums.SessionStatusRunning), "")
	completedID := seedSessionForRestoreTest(t, pg, string(enums.SessionStatusCompleted), "旧任务总结")

	list, err := svc.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]*Session, len(list))
	for _, s := range list {
		byID[s.ID] = s
	}
	intr, ok := byID[interruptedID]
	if !ok {
		t.Fatalf("列表应包含库中 running 遗留会话 %s", interruptedID)
	}
	if intr.Status != string(enums.SessionStatusError) || intr.Result != interruptedByRestartMsg {
		t.Errorf("running 遗留行应映射为 error + 中断提示, got status=%q result=%q", intr.Status, intr.Result)
	}
	comp, ok := byID[completedID]
	if !ok {
		t.Fatalf("列表应包含库中 completed 会话 %s", completedID)
	}
	if comp.Status != string(enums.SessionStatusCompleted) || comp.Result != "旧任务总结" {
		t.Errorf("completed 会话映射错误, got status=%q result=%q", comp.Status, comp.Result)
	}
}
