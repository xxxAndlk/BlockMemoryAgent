package goal

// manager_test.go Manager 单测：内存 Store + 假评估/唤醒/通告，走真实钩子路径。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// fakeStore 是 Store 的内存实现（无并发压力，单测串行使用）。
type fakeStore struct {
	directives map[string]*Directive      // session_id -> directive
	reminders  map[string]*Reminder       // id -> reminder
	dueErr     error
}

func newFakeStore() *fakeStore {
	return &fakeStore{directives: map[string]*Directive{}, reminders: map[string]*Reminder{}}
}

func (f *fakeStore) UpsertDirective(_ context.Context, d *Directive) error {
	cp := *d
	f.directives[d.SessionID] = &cp
	return nil
}

func (f *fakeStore) GetDirectiveBySession(_ context.Context, sid string) (*Directive, error) {
	if d, ok := f.directives[sid]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeStore) TouchTurn(_ context.Context, id string, turn int, lastEval string) error {
	for _, d := range f.directives {
		if d.ID == id {
			d.TurnCount = turn
			d.LastEval = lastEval
		}
	}
	return nil
}

func (f *fakeStore) FinishDirective(_ context.Context, id, status string, turn int, lastEval string) (bool, error) {
	for _, d := range f.directives {
		if d.ID == id && d.Status == StatusActive {
			d.Status = status
			d.TurnCount = turn
			d.LastEval = lastEval
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) AddReminder(_ context.Context, r *Reminder) error {
	cp := *r
	f.reminders[r.ID] = &cp
	return nil
}

func (f *fakeStore) ClaimDueReminders(_ context.Context, limit int) ([]*Reminder, error) {
	if f.dueErr != nil {
		return nil, f.dueErr
	}
	now := time.Now()
	out := []*Reminder{}
	for _, r := range f.reminders {
		if r.Status == ReminderStatusPending && !r.RunAt.After(now) {
			r.Status = ReminderStatusFired
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) CancelPendingReminders(_ context.Context, sid string) (int64, error) {
	var n int64
	for _, r := range f.reminders {
		if r.SessionID == sid && r.Status == ReminderStatusPending {
			r.Status = ReminderStatusCancelled
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) ListPendingReminders(_ context.Context, sid string) ([]*Reminder, error) {
	out := []*Reminder{}
	for _, r := range f.reminders {
		if r.SessionID == sid && r.Status == ReminderStatusPending {
			out = append(out, r)
		}
	}
	return out, nil
}

// testRig 组装 Manager + 记录点。
type testRig struct {
	m      *Manager
	store  *fakeStore
	mu     sync.Mutex
	wakes  []string
	notifs []string
	judgeResp string
	judgeErr  error
}

func newRig() *testRig {
	r := &testRig{store: newFakeStore()}
	r.m = NewManager(Config{PollInterval: time.Hour, MinLoopInterval: 30 * time.Second, JudgeTimeout: time.Second},
		r.store,
		func(ctx context.Context, prompt string) (string, error) { return r.judgeResp, r.judgeErr },
		func(ctx context.Context, sid, text string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.wakes = append(r.wakes, sid+"|"+text)
			return nil
		},
		func(sid, text string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.notifs = append(r.notifs, sid+"|"+text)
		},
		nil)
	return r
}

func (r *testRig) wakeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.wakes)
}

func (r *testRig) lastWake() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.wakes) == 0 {
		return ""
	}
	return r.wakes[len(r.wakes)-1]
}

func (r *testRig) lastNotif() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.notifs) == 0 {
		return ""
	}
	return r.notifs[len(r.notifs)-1]
}

// waitWakes 轮询等待唤醒次数达标（fireDue 的唤醒在 goroutine 里，需等其落地）。
func (r *testRig) waitWakes(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.wakeCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待唤醒超时：期望 ≥%d, got %d", want, r.wakeCount())
}

func goalSpec() *agent.DirectiveSpec {
	return &agent.DirectiveSpec{Mode: ModeGoal, Objective: "帮我自动开通网店", CompletionCriterion: "店铺已上线可下单"}
}

func TestApplyDirectiveValidation(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	cases := []*agent.DirectiveSpec{
		{Mode: "bogus"},
		{Mode: ModeGoal},                                     // 缺 objective
		{Mode: ModeLoop, Objective: "x"},                     // 缺 interval
		{Mode: ModeGoal, Objective: "x", Deadline: "not-a-time"},
	}
	for i, spec := range cases {
		if err := r.m.ApplyDirective(ctx, "s1", spec); err == nil {
			t.Fatalf("case %d: 期望校验失败", i)
		}
	}
}

func TestApplyDirectiveNormalStopsAndCancelsReminders(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	if err := r.m.ApplyDirective(ctx, "s1", goalSpec()); err != nil {
		t.Fatal(err)
	}
	// 塞一条未触发提醒，切 normal 后应被取消。
	if _, err := r.m.AddFollowup(ctx, "s1", 2*time.Hour, "检查审核", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := r.m.ApplyDirective(ctx, "s1", &agent.DirectiveSpec{Mode: ModeNormal}); err != nil {
		t.Fatal(err)
	}
	d := r.store.directives["s1"]
	if d == nil || d.Mode != ModeNormal || d.Status != StatusStopped {
		t.Fatalf("切 normal 后行异常: %+v", d)
	}
	for _, rem := range r.store.reminders {
		if rem.Status == ReminderStatusPending {
			t.Fatalf("切 normal 后仍有 pending 提醒: %+v", rem)
		}
	}
}

func TestGoalTurnEndNotAchievedWakesContinue(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	if err := r.m.ApplyDirective(ctx, "s1", goalSpec()); err != nil {
		t.Fatal(err)
	}
	r.judgeResp = `{"achieved": false, "reason": "店铺尚未提交审核"}`
	r.m.OnTurnEnd("s1", "completed", "已完成资料填写，待提交")
	if r.wakeCount() != 1 {
		t.Fatalf("期望 1 次续跑注入, got %d", r.wakeCount())
	}
	if !strings.Contains(r.lastWake(), "尚未提交审核") || !strings.Contains(r.lastWake(), "schedule_followup") {
		t.Fatalf("续跑消息缺少评估理由/工具提示: %s", r.lastWake())
	}
	if d := r.store.directives["s1"]; d.TurnCount != 1 {
		t.Fatalf("TurnCount=1 期望, got %d", d.TurnCount)
	}
}

func TestGoalTurnEndAchievedFinishes(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	_ = r.m.ApplyDirective(ctx, "s1", goalSpec())
	r.judgeResp = "```json\n{\"achieved\": true, \"reason\": \"店铺已上线\"}\n```"
	r.m.OnTurnEnd("s1", "completed", "店铺已上线，首单完成")
	if r.wakeCount() != 0 {
		t.Fatalf("达成后不应续跑, wakes=%d", r.wakeCount())
	}
	if d := r.store.directives["s1"]; d.Status != StatusComplete {
		t.Fatalf("期望 complete, got %s", d.Status)
	}
	if !strings.Contains(r.lastNotif(), "已达成") {
		t.Fatalf("达成通告缺失: %s", r.lastNotif())
	}
}

func TestGoalJudgeUnavailableKeepsLooping(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	_ = r.m.ApplyDirective(ctx, "s1", goalSpec())
	r.judgeErr = errors.New("model down")
	r.m.OnTurnEnd("s1", "completed", "x")
	if r.wakeCount() != 1 {
		t.Fatalf("评委失败应按未达成续跑, wakes=%d", r.wakeCount())
	}
	if d := r.store.directives["s1"]; d.Status != StatusActive {
		t.Fatalf("评委失败不应终局, got %s", d.Status)
	}
}

func TestGoalBudgetStops(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	spec := goalSpec()
	spec.MaxTurns = 1
	_ = r.m.ApplyDirective(ctx, "s1", spec)
	r.m.OnTurnEnd("s1", "completed", "x")
	if r.wakeCount() != 0 {
		t.Fatalf("预算耗尽不应续跑")
	}
	if d := r.store.directives["s1"]; d.Status != StatusFailed {
		t.Fatalf("期望 failed, got %s", d.Status)
	}
	if !strings.Contains(r.lastNotif(), "预算") {
		t.Fatalf("预算通告缺失: %s", r.lastNotif())
	}
}

func TestLoopSchedulesNextReminder(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	if err := r.m.ApplyDirective(ctx, "s1", &agent.DirectiveSpec{
		Mode: ModeLoop, Objective: "每轮巡检接口状态", LoopIntervalSec: 120,
	}); err != nil {
		t.Fatal(err)
	}
	r.m.OnTurnEnd("s1", "completed", "本轮正常")
	pending, _ := r.store.ListPendingReminders(ctx, "s1")
	if len(pending) != 1 {
		t.Fatalf("期望排 1 条下一轮提醒, got %d", len(pending))
	}
	if pending[0].Kind != ReminderKindInterval || !strings.Contains(pending[0].Text, "巡检接口状态") {
		t.Fatalf("提醒内容异常: %+v", pending[0])
	}
	// 无停止条件的 loop 也要记轮（徽标轮数/预算闸都依赖它）。
	if d := r.store.directives["s1"]; d.TurnCount != 1 {
		t.Fatalf("loop 无 until 也应记轮, got %d", d.TurnCount)
	}
	if r.wakeCount() != 0 {
		t.Fatalf("loop 不应直接唤醒, wakes=%d", r.wakeCount())
	}
	// 最小间隔收敛：30s 配置下限 → 120s 不受影响；再试 1s 应被收敛到 30s。
	if err := r.m.ApplyDirective(ctx, "s2", &agent.DirectiveSpec{Mode: ModeLoop, Objective: "y", LoopIntervalSec: 1}); err != nil {
		t.Fatal(err)
	}
	if d := r.store.directives["s2"]; d.LoopIntervalSec != 30 {
		t.Fatalf("最小间隔收敛失败: got %d", d.LoopIntervalSec)
	}
}

func TestLoopUntilMetStops(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	_ = r.m.ApplyDirective(ctx, "s1", &agent.DirectiveSpec{
		Mode: ModeLoop, Objective: "持续压测直到错误率<1%", LoopIntervalSec: 120, LoopUntil: "错误率<1%",
	})
	r.judgeResp = `{"met": true, "reason": "错误率已降至 0.8%"}`
	r.m.OnTurnEnd("s1", "completed", "本轮错误率 0.8%")
	if d := r.store.directives["s1"]; d.Status != StatusComplete {
		t.Fatalf("期望 complete, got %s", d.Status)
	}
	if pending, _ := r.store.ListPendingReminders(ctx, "s1"); len(pending) != 0 {
		t.Fatalf("停止后不应再排提醒")
	}
}

func TestOnTurnEndIgnoresNonCompleted(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	_ = r.m.ApplyDirective(ctx, "s1", goalSpec())
	r.m.OnTurnEnd("s1", "paused", "x")
	r.m.OnTurnEnd("s1", "awaiting_clarify", "x")
	if d := r.store.directives["s1"]; d.TurnCount != 0 {
		t.Fatalf("非 completed 不应计数, got %d", d.TurnCount)
	}
}

func TestDeactivateStopsAndCancels(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	_ = r.m.ApplyDirective(ctx, "s1", goalSpec())
	_, _ = r.m.AddFollowup(ctx, "s1", time.Hour, "回查", "agent")
	r.m.Deactivate("s1", "用户停止")
	d := r.store.directives["s1"]
	if d.Status != StatusStopped {
		t.Fatalf("期望 stopped, got %s", d.Status)
	}
	if pending, _ := r.store.ListPendingReminders(ctx, "s1"); len(pending) != 0 {
		t.Fatalf("停止后提醒应被取消")
	}
	if !strings.Contains(r.lastNotif(), "已停止") {
		t.Fatalf("停止通告缺失: %s", r.lastNotif())
	}
}

func TestFireDueWakesSessions(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	if _, err := r.m.AddFollowup(ctx, "s1", time.Minute, "检查开店审核结果", "agent"); err != nil {
		t.Fatal(err)
	}
	// 手动把 run_at 拨到过去，模拟到期。
	for _, rem := range r.store.reminders {
		rem.RunAt = time.Now().Add(-time.Second)
	}
	r.m.fireDue(ctx)
	r.waitWakes(t, 1)
	if !strings.Contains(r.lastWake(), "检查开店审核结果") {
		t.Fatalf("唤醒消息异常: %s", r.lastWake())
	}
	// 已认领，再次扫描不重复触发。
	r.m.fireDue(ctx)
	time.Sleep(50 * time.Millisecond)
	if r.wakeCount() != 1 {
		t.Fatalf("重复扫描不应重复唤醒, wakes=%d", r.wakeCount())
	}
}

func TestAddFollowupValidation(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	if _, err := r.m.AddFollowup(ctx, "s1", 30*time.Second, "x", "agent"); err == nil {
		t.Fatal("短间隔应拒绝")
	}
	if _, err := r.m.AddFollowup(ctx, "s1", 40*24*time.Hour, "x", "agent"); err == nil {
		t.Fatal("超 30 天应拒绝")
	}
	if _, err := r.m.AddFollowup(ctx, "s1", time.Hour, "", "agent"); err == nil {
		t.Fatal("空内容应拒绝")
	}
	got, err := r.m.AddFollowup(ctx, "s1", 2*time.Hour, "检查审核", "agent")
	if err != nil || !strings.Contains(got, "检查审核") {
		t.Fatalf("合法回查异常: %q %v", got, err)
	}
}

func TestGetView(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	v, err := r.m.GetView(ctx, "s1")
	if err != nil || v.Directive != nil {
		t.Fatalf("无指令会话应返回空视图: %+v %v", v, err)
	}
	_ = r.m.ApplyDirective(ctx, "s1", goalSpec())
	_, _ = r.m.AddFollowup(ctx, "s1", 2*time.Hour, "回查", "agent")
	v, err = r.m.GetView(ctx, "s1")
	if err != nil || v.Directive == nil || len(v.PendingReminders) != 1 {
		t.Fatalf("视图异常: %+v %v", v, err)
	}
}
