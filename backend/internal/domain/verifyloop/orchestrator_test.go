package verifyloop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// fakeRunner 记录 ExecuteChild 调用并按预设脚本返回结果。
// scripts 键为 "roleID:callIndex"，每次该 role 的 ExecuteChild 调用消费一条脚本。
type fakeRunner struct {
	mu          sync.Mutex
	calls       []fakeCall
	roleCounter map[string]int
	scripts     map[string]string
	defaultResp string
	err         error
}

type fakeCall struct {
	RoleID   string
	Task     string
	Response string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		roleCounter: map[string]int{},
		scripts:     map[string]string{},
	}
}

func (f *fakeRunner) ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	idx := f.roleCounter[roleID]
	f.roleCounter[roleID]++
	f.calls = append(f.calls, fakeCall{RoleID: roleID, Task: task})
	resp := f.defaultResp
	if s, ok := f.scripts[scriptKey(roleID, idx)]; ok {
		resp = s
	}
	f.calls[len(f.calls)-1].Response = resp
	return resp, nil
}

func scriptKey(roleID string, idx int) string {
	return roleID + ":" + itoa(idx)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// newDefaultOrchestrator 用 New 默认装配（AgentVerifier + AgentFixer + MailboxReporter）构造编排器。
// 测试用例默认走这条路径，覆盖主流程；自定义接口注入的用例走 NewWith。
func newDefaultOrchestrator(t *testing.T, r Runner, mb *mailbox.Mailbox, maxRounds int) *Orchestrator {
	t.Helper()
	return New(r, mb, maxRounds, "code_assistant", "test_assistant")
}

func baseRequest(initial, produced string) Request {
	return Request{
		ParentID:    "session-1",
		ProducerID:  "session-1/code_assistant-1",
		InitialTask: initial,
		Produced:    produced,
	}
}

func TestOrchestrator_PassOnFirstRound(t *testing.T) {
	r := newFakeRunner()
	// AgentVerifier 实现 PlanConfirmVerifier，每轮先 PlanConfirm 消耗 1 脚本，再 SelfTest/UnifiedTest。
	r.scripts["test_assistant:0"] = verifyMarkerPass // PlanConfirm 通过
	r.scripts["test_assistant:1"] = "tests passed\n" + verifyMarkerPass // SelfTest
	r.scripts["test_assistant:2"] = "unified tests passed\n" + verifyMarkerPass // UnifiedTest
	o := newDefaultOrchestrator(t, r, nil, 5)

	res := o.Run(context.Background(), baseRequest("write calc.go", "func Add(a,b int)int{return a+b}"))

	if !res.Passed {
		t.Fatalf("expected pass, got fail: %s", res.FailReason)
	}
	if res.Rounds != 1 {
		t.Fatalf("expected 1 round, got %d", res.Rounds)
	}
	// PlanConfirm + SelfTest + UnifiedTest = 3 次调用。
	if got := len(r.calls); got != 3 {
		t.Fatalf("expected 3 ExecuteChild calls, got %d", got)
	}
	if res.FinalProduced != "func Add(a,b int)int{return a+b}" {
		t.Fatalf("FinalProduced mismatch: %s", res.FinalProduced)
	}
}

func TestOrchestrator_SelfTestFailThenFixThenPass(t *testing.T) {
	r := newFakeRunner()
	// round 1: PlanConfirm 通过 -> SelfTest 失败 -> Fix
	// round 2: PlanConfirm 通过 -> SelfTest 通过 -> UnifiedTest 通过
	r.scripts["test_assistant:0"] = verifyMarkerPass                       // PlanConfirm r1
	r.scripts["test_assistant:1"] = "unit test failed: Add returns 0\n" + verifyMarkerFail // SelfTest r1
	r.scripts["code_assistant:0"] = "fixed: return a+b"                    // Fix r1
	r.scripts["test_assistant:2"] = verifyMarkerPass                       // PlanConfirm r2
	r.scripts["test_assistant:3"] = "unit tests passed\n" + verifyMarkerPass // SelfTest r2
	r.scripts["test_assistant:4"] = "unified tests passed\n" + verifyMarkerPass // UnifiedTest r2
	o := newDefaultOrchestrator(t, r, nil, 5)

	res := o.Run(context.Background(), baseRequest("write calc.go", "func Add(a,b int)int{return 0}"))

	if !res.Passed {
		t.Fatalf("expected pass, got fail: %s", res.FailReason)
	}
	if res.Rounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", res.Rounds)
	}
	if res.FinalProduced != "fixed: return a+b" {
		t.Fatalf("FinalProduced mismatch: %s", res.FinalProduced)
	}
	fixIdx := -1
	for i, c := range r.calls {
		if c.RoleID == "code_assistant" {
			fixIdx = i
			break
		}
	}
	if fixIdx < 0 {
		t.Fatal("no code_assistant fix call recorded")
	}
	if !strings.Contains(r.calls[fixIdx].Task, "unit test failed") {
		t.Fatalf("fix task should contain test feedback, got: %s", r.calls[fixIdx].Task)
	}
}

func TestOrchestrator_UnifiedTestFailLoopsBack(t *testing.T) {
	r := newFakeRunner()
	// round 1: PlanConfirm 通过 -> SelfTest 通过 -> UnifiedTest 失败 -> Fix
	// round 2: PlanConfirm 通过 -> SelfTest 通过 -> UnifiedTest 通过
	r.scripts["test_assistant:0"] = verifyMarkerPass                            // PlanConfirm r1
	r.scripts["test_assistant:1"] = verifyMarkerPass                            // SelfTest r1
	r.scripts["test_assistant:2"] = "unified: integration broke\n" + verifyMarkerFail // UnifiedTest r1
	r.scripts["code_assistant:0"] = "fixed integration"                         // Fix r1
	r.scripts["test_assistant:3"] = verifyMarkerPass                            // PlanConfirm r2
	r.scripts["test_assistant:4"] = verifyMarkerPass                            // SelfTest r2
	r.scripts["test_assistant:5"] = verifyMarkerPass                            // UnifiedTest r2
	o := newDefaultOrchestrator(t, r, nil, 5)

	res := o.Run(context.Background(), baseRequest("write calc.go", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass, got fail: %s", res.FailReason)
	}
	if res.Rounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", res.Rounds)
	}
	if res.FinalProduced != "fixed integration" {
		t.Fatalf("FinalProduced mismatch: %s", res.FinalProduced)
	}
}

func TestOrchestrator_MaxRoundsExceeded(t *testing.T) {
	r := newFakeRunner()
	// PlanConfirm 通过（:0,:2,:4,:6），SelfTest 失败（:1,:3,:5,:7），每轮触发 Fix。
	r.scripts["test_assistant:0"] = verifyMarkerPass
	r.scripts["test_assistant:1"] = "still failing\n" + verifyMarkerFail
	r.scripts["test_assistant:2"] = verifyMarkerPass
	r.scripts["test_assistant:3"] = "still failing\n" + verifyMarkerFail
	r.scripts["test_assistant:4"] = verifyMarkerPass
	r.scripts["test_assistant:5"] = "still failing\n" + verifyMarkerFail
	r.scripts["code_assistant:0"] = "fixed-v1"
	r.scripts["code_assistant:1"] = "fixed-v2"
	o := newDefaultOrchestrator(t, r, nil, 2)

	res := o.Run(context.Background(), baseRequest("write calc.go", "v1"))

	if res.Passed {
		t.Fatal("expected fail, got pass")
	}
	if res.Rounds != 2 {
		t.Fatalf("expected 2 rounds, got %d", res.Rounds)
	}
	if !strings.Contains(res.FailReason, "max rounds") {
		t.Fatalf("FailReason should mention max rounds, got: %s", res.FailReason)
	}
}

func TestOrchestrator_ExecuteChildError(t *testing.T) {
	r := newFakeRunner()
	r.err = errors.New("provider down")
	o := newDefaultOrchestrator(t, r, nil, 5)

	res := o.Run(context.Background(), baseRequest("write calc.go", "v1"))

	if res.Passed {
		t.Fatal("expected fail on ExecuteChild error")
	}
	// PlanConfirm 先失败（AgentVerifier 实现 PlanConfirmVerifier）。
	if !strings.Contains(res.FailReason, "plan-confirm failed") {
		t.Fatalf("FailReason should mention plan-confirm failure, got: %s", res.FailReason)
	}
}

func TestOrchestrator_MailboxReporter(t *testing.T) {
	mb := mailbox.New()
	// AgentVerifier 实现 PlanConfirmVerifier，需 3 脚本：PlanConfirm + SelfTest + UnifiedTest。
	o := NewWith(
		NewAgentVerifier(newFakeRunnerWithScripts(map[string]string{
			"test_assistant:0": verifyMarkerPass,
			"test_assistant:1": verifyMarkerPass,
			"test_assistant:2": verifyMarkerPass,
		}), "test_assistant", verifyMarkerPass, verifyMarkerFail),
		NewAgentFixer(newFakeRunner(), "code_assistant"),
		NewMailboxReporter(mb),
		5,
	)

	o.Run(context.Background(), baseRequest("t", "v1"))
	msgs := mb.Drain("session-1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 pass message, got %d", len(msgs))
	}
	if msgs[0].Type != mailbox.MsgInfo {
		t.Fatalf("expected MsgInfo, got %s", msgs[0].Type)
	}
	if !strings.Contains(msgs[0].Subject, "通过") {
		t.Fatalf("subject should mention pass, got: %s", msgs[0].Subject)
	}
}

func TestOrchestrator_DefaultMaxRounds(t *testing.T) {
	r := newFakeRunner()
	// PlanConfirm 通过但 SelfTest 始终失败，跑满默认轮数。
	r.defaultResp = verifyMarkerFail
	// 覆盖 PlanConfirm 使其通过（每轮第 0、2、4... 次 test_assistant 调用）。
	for i := 0; i < defaultMaxRounds*2; i += 2 {
		r.scripts["test_assistant:"+itoa(i)] = verifyMarkerPass
	}
	r.scripts["code_assistant:0"] = "x"
	r.scripts["code_assistant:1"] = "x"
	r.scripts["code_assistant:2"] = "x"
	r.scripts["code_assistant:3"] = "x"
	o := newDefaultOrchestrator(t, r, nil, 0)
	res := o.Run(context.Background(), baseRequest("t", "v1"))
	if res.Passed {
		t.Fatal("expected fail")
	}
	if res.Rounds != defaultMaxRounds {
		t.Fatalf("expected default %d rounds, got %d", defaultMaxRounds, res.Rounds)
	}
}

func TestParseAgentVerdict(t *testing.T) {
	v := ParseAgentVerdict("ok " + verifyMarkerPass)
	if !v.Passed {
		t.Fatal("trailing PASS should be pass")
	}
	v = ParseAgentVerdict("bad " + verifyMarkerFail + " reason here")
	if v.Passed {
		t.Fatal("trailing FAIL should be fail")
	}
	if v.Reason != "reason here" {
		t.Fatalf("reason mismatch: %s", v.Reason)
	}
	// FAIL 在前、PASS 出现在失败原因中：应判失败。
	v = ParseAgentVerdict(verifyMarkerFail + " see " + verifyMarkerPass + " in log")
	if v.Passed {
		t.Fatal("FAIL marker should dominate over PASS in text")
	}
	v = ParseAgentVerdict("no marker")
	if v.Passed {
		t.Fatal("no marker should be fail")
	}
	if !strings.Contains(v.Reason, "missing") {
		t.Fatalf("missing-marker reason, got: %s", v.Reason)
	}
}

func TestOrchestrator_ContextCancel(t *testing.T) {
	o := NewWith(
		&slowVerifier{delay: 20 * time.Millisecond, pass: false},
		NewAgentFixer(&slowRunner{delay: 20 * time.Millisecond}, "code_assistant"),
		nil,
		100,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res := o.Run(ctx, baseRequest("t", "v1"))
	if res.Passed {
		t.Fatal("expected fail on cancel")
	}
	if res.Rounds >= 100 {
		t.Fatalf("should return early on cancel, ran %d rounds", res.Rounds)
	}
	if !strings.Contains(res.FailReason, "cancelled") {
		t.Fatalf("FailReason should mention cancel, got: %s", res.FailReason)
	}
}

// ---- 自定义接口注入测试（验证抽象层支持非 Agent 验证器）----

// slowVerifier 每次 SelfTest/UnifiedTest 阻塞 delay 后返回固定 Verdict。
type slowVerifier struct {
	delay time.Duration
	pass  bool
}

func (s *slowVerifier) SelfTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	select {
	case <-time.After(s.delay):
		return Verdict{Passed: s.pass, Detail: "slow self"}, nil
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	}
}

func (s *slowVerifier) UnifiedTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	select {
	case <-time.After(s.delay):
		return Verdict{Passed: s.pass, Detail: "slow unified"}, nil
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	}
}

// slowRunner 每次 ExecuteChild 阻塞 delay，供 Fixer 测试。
type slowRunner struct {
	delay time.Duration
	resp  string
}

func (s *slowRunner) ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error) {
	select {
	case <-time.After(s.delay):
		return s.resp, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// newFakeRunnerWithScripts 是 newFakeRunner 的便捷构造，直接传 scripts。
func newFakeRunnerWithScripts(scripts map[string]string) *fakeRunner {
	r := newFakeRunner()
	for k, v := range scripts {
		r.scripts[k] = v
	}
	return r
}

// TestOrchestrator_CustomVerifierInjection 验证 NewWith 可注入完全自定义的
// Verifier/Fixer/Reporter，编排器引擎不依赖 Agent 路径。
func TestOrchestrator_CustomVerifierInjection(t *testing.T) {
	// 自定义验证器：始终通过。
	cv := &customVerifier{pass: true}
	// 自定义修正器：不应被调用（验证器始终通过）。
	cf := &customFixer{}
	// 自定义上报器：记录 Report 调用。
	cr := &customReporter{}

	o := NewWith(cv, cf, cr, 3)
	res := o.Run(context.Background(), baseRequest("t", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass, got: %s", res.FailReason)
	}
	if cv.selfTestCalls != 1 {
		t.Fatalf("expected 1 self-test call, got %d", cv.selfTestCalls)
	}
	if cv.unifiedTestCalls != 1 {
		t.Fatalf("expected 1 unified-test call, got %d", cv.unifiedTestCalls)
	}
	if cf.fixCalls != 0 {
		t.Fatalf("fixer should not be called when all pass, got %d", cf.fixCalls)
	}
	if cr.reportCalls != 1 {
		t.Fatalf("expected 1 report call, got %d", cr.reportCalls)
	}
	if !cr.lastResult.Passed {
		t.Fatal("reported result should be pass")
	}
}

// TestOrchestrator_CustomVerifierFailThenFix 验证自定义验证器失败时触发自定义修正器。
func TestOrchestrator_CustomVerifierFailThenFix(t *testing.T) {
	cv := &customVerifier{pass: false, passAfter: 1} // 第2次自测通过
	cf := &customFixer{produced: "fixed"}
	cr := &customReporter{}

	o := NewWith(cv, cf, cr, 5)
	res := o.Run(context.Background(), baseRequest("t", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass after fix, got: %s", res.FailReason)
	}
	if cf.fixCalls != 1 {
		t.Fatalf("expected 1 fix call, got %d", cf.fixCalls)
	}
	if res.FinalProduced != "fixed" {
		t.Fatalf("FinalProduced should be fixer output, got: %s", res.FinalProduced)
	}
}

// customVerifier 自定义验证器：passAfter 次自测后转为通过。
type customVerifier struct {
	pass       bool
	passAfter  int // >0 时前 N 次 SelfTest 失败，之后通过
	selfTestCalls int
	unifiedTestCalls int
}

func (c *customVerifier) SelfTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	c.selfTestCalls++
	if c.passAfter > 0 && c.selfTestCalls <= c.passAfter {
		return Verdict{Passed: false, Reason: "custom self fail"}, nil
	}
	return Verdict{Passed: true, Detail: "custom self pass"}, nil
}

func (c *customVerifier) UnifiedTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	c.unifiedTestCalls++
	return Verdict{Passed: true, Detail: "custom unified pass"}, nil
}

// customFixer 自定义修正器：返回固定 produced。
type customFixer struct {
	produced string
	fixCalls int
}

func (c *customFixer) Fix(ctx context.Context, req Request, produced, feedback string) (string, error) {
	c.fixCalls++
	if c.produced == "" {
		return "default-fixed", nil
	}
	return c.produced, nil
}

// customReporter 自定义上报器：记录调用次数与最后结果。
type customReporter struct {
	reportCalls int
	lastResult  Result
}

func (c *customReporter) Report(req Request, result Result) {
	c.reportCalls++
	c.lastResult = result
}

// TestComputerUseVerifier_NotImplemented 验证扩展验证器口子存在且未实现，
// 确保后续接入 computeruse 包时有明确实现点。
func TestComputerUseVerifier_NotImplemented(t *testing.T) {
	cv := &ComputerUseVerifier{}
	v, err := cv.SelfTest(context.Background(), Request{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("ComputerUseVerifier.SelfTest should not pass before implementation")
	}
	if !strings.Contains(v.Reason, "not implemented") {
		t.Fatalf("reason should mention not implemented, got: %s", v.Reason)
	}
}

func TestCLIVerifier_NotImplemented(t *testing.T) {
	cv := &CLIVerifier{SelfTestCmd: "go test ./...", UnifiedCmd: "go build ./..."}
	v, err := cv.SelfTest(context.Background(), Request{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("CLIVerifier.SelfTest should not pass before implementation")
	}
}

func TestMCPVerifier_NotImplemented(t *testing.T) {
	mv := &MCPVerifier{SelfTestTool: "unit_test", UnifiedTestTool: "integration_test"}
	v, err := mv.SelfTest(context.Background(), Request{}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Passed {
		t.Fatal("MCPVerifier.SelfTest should not pass before implementation")
	}
}

// TestOrchestrator_PlanConfirm_Pass 验证 Verifier 实现 PlanConfirmVerifier 时
// 引擎在 SelfTest 前调用 PlanConfirm，通过后进 SelfTest。
func TestOrchestrator_PlanConfirm_Pass(t *testing.T) {
	cv := &planConfirmVerifier{planPass: true, selfPass: true, unifiedPass: true}
	cf := &customFixer{}
	cr := &customReporter{}
	o := NewWith(cv, cf, cr, 5)

	res := o.Run(context.Background(), baseRequest("t", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass, got: %s", res.FailReason)
	}
	if cv.planCalls != 1 {
		t.Fatalf("expected 1 plan-confirm call, got %d", cv.planCalls)
	}
	if cv.selfCalls != 1 {
		t.Fatalf("expected 1 self-test call, got %d", cv.selfCalls)
	}
	if cv.unifiedCalls != 1 {
		t.Fatalf("expected 1 unified-test call, got %d", cv.unifiedCalls)
	}
	if cf.fixCalls != 0 {
		t.Fatalf("fixer should not be called when all pass, got %d", cf.fixCalls)
	}
}

// TestOrchestrator_PlanConfirm_FailThenFix 验证方案确认未通过时触发 Fixer 修正。
func TestOrchestrator_PlanConfirm_FailThenFix(t *testing.T) {
	// planPass=true + planPassAfter=1：第 1 次 PlanConfirm 失败，之后通过。
	cv := &planConfirmVerifier{planPass: true, planPassAfter: 1, selfPass: true, unifiedPass: true}
	cf := &customFixer{produced: "fixed"}
	cr := &customReporter{}
	o := NewWith(cv, cf, cr, 5)

	res := o.Run(context.Background(), baseRequest("t", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass after fix, got: %s", res.FailReason)
	}
	if cv.planCalls != 2 {
		t.Fatalf("expected 2 plan-confirm calls (fail then pass), got %d", cv.planCalls)
	}
	if cf.fixCalls != 1 {
		t.Fatalf("expected 1 fix call after plan reject, got %d", cf.fixCalls)
	}
}

// TestOrchestrator_PlanConfirm_SkippedWhenNotImplemented 验证未实现 PlanConfirmVerifier 的
// Verifier 跳过方案确认，直接进 SelfTest（向后兼容）。
func TestOrchestrator_PlanConfirm_SkippedWhenNotImplemented(t *testing.T) {
	// customVerifier 不实现 PlanConfirmVerifier。
	cv := &customVerifier{pass: true}
	cf := &customFixer{}
	cr := &customReporter{}
	o := NewWith(cv, cf, cr, 3)

	res := o.Run(context.Background(), baseRequest("t", "v1"))

	if !res.Passed {
		t.Fatalf("expected pass, got: %s", res.FailReason)
	}
	// PlanConfirmVerdict 应为零值（未调用）。
	if res.PlanConfirmVerdict.Detail != "" || res.PlanConfirmVerdict.Reason != "" {
		t.Fatalf("PlanConfirmVerdict should be zero when Verifier doesn't implement it, got %+v", res.PlanConfirmVerdict)
	}
}

// planConfirmVerifier 自定义验证器，实现 PlanConfirmVerifier 接口。
type planConfirmVerifier struct {
	planPass      bool
	planPassAfter int // >0 时前 N 次 PlanConfirm 失败，之后按 planPass 决定
	planCalls     int
	selfPass      bool
	selfCalls     int
	unifiedPass   bool
	unifiedCalls  int
}

func (p *planConfirmVerifier) PlanConfirm(ctx context.Context, req Request, produced string) (Verdict, error) {
	p.planCalls++
	if p.planPassAfter > 0 && p.planCalls <= p.planPassAfter {
		return Verdict{Passed: false, Reason: "plan incomplete"}, nil
	}
	return Verdict{Passed: p.planPass, Detail: "plan"}, nil
}

func (p *planConfirmVerifier) SelfTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	p.selfCalls++
	return Verdict{Passed: p.selfPass, Detail: "self"}, nil
}

func (p *planConfirmVerifier) UnifiedTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	p.unifiedCalls++
	return Verdict{Passed: p.unifiedPass, Detail: "unified"}, nil
}

// TestAgentVerifier_PlanConfirm 验证默认 AgentVerifier 实现 PlanConfirmVerifier。
func TestAgentVerifier_PlanConfirm(t *testing.T) {
	r := newFakeRunner()
	// 单轮 PlanConfirm：列方案+自评通过。
	r.scripts["test_assistant:0"] = "plan: cover Add edge cases\n" + verifyMarkerPass
	v := NewAgentVerifier(r, "test_assistant", verifyMarkerPass, verifyMarkerFail)

	req := baseRequest("write calc.go", "func Add(a,b int)int{return a+b}")
	verdict, err := v.PlanConfirm(context.Background(), req, req.Produced)
	if err != nil {
		t.Fatalf("PlanConfirm: %v", err)
	}
	if !verdict.Passed {
		t.Fatalf("expected plan pass, got: %s", verdict.Reason)
	}
	if !strings.Contains(verdict.Detail, "plan: cover Add") {
		t.Fatalf("detail should contain plan, got: %s", verdict.Detail)
	}
}
