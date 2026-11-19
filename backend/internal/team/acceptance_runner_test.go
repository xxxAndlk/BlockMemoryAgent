package team

// acceptance_runner_test.go 覆盖 2026-11-19 验收报告"WP3 证据采集未接线"的返修：
// 判定层（L0）已经确定性可测，这里测**采集层**是否真的把真实执行结果变成合规证据：
//   - 命令类规则（static/unit/integration）走真实执行器，退出码决定通过与否；
//   - 能力缺失（无执行器/无 DOM 采集器）只能产出环境性缺口，绝不当作通过；
//   - 采集出的证据必须带 task/revision/hash 绑定（否则可能被当成跨版本证据复用）；
//   - file_hash 不变量真实读盘校验。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runnerContract(t *testing.T, acceptance []AcceptanceRule, invariants []Invariant) *LogicalTaskContract {
	t.Helper()
	c := &LogicalTaskContract{
		LogicalTaskID: "lt-runner", SessionID: "s-runner", Revision: 7,
		Status: TaskStatusAwaitingVerification, Goal: "采集验收证据",
		Acceptance: acceptance, Invariants: invariants,
	}
	if err := NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAcceptanceRunner_CommandEvidenceBindsContract 采集出的证据必须绑定合同版本。
func TestAcceptanceRunner_CommandEvidenceBindsContract(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{{
		ID: "rule-build", Kind: EvidenceStatic, Required: true,
		Spec: json.RawMessage(`{"cmd":"noop"}`),
	}}, nil)
	runner := &AcceptanceRunner{Exec: func(context.Context, string, string, time.Duration) CommandResult {
		return CommandResult{ExitCode: 0, Output: "built fine"}
	}}
	evidence := runner.Collect(context.Background(), c, t.TempDir())
	if len(evidence) != 1 {
		t.Fatalf("expected one evidence, got %+v", evidence)
	}
	ev := evidence[0]
	if ev.TaskID != c.LogicalTaskID || ev.Revision != c.Revision || ev.ContractHash != c.ContentHash {
		t.Fatalf("evidence must be bound to contract revision: %+v", ev)
	}
	if !ev.Passed || ev.Kind != EvidenceStatic || ev.ID != "rule-build" {
		t.Fatalf("command exit 0 must pass a static rule: %+v", ev)
	}
	if runner.Verify(context.Background(), c, t.TempDir()).Status != VerdictVerified {
		t.Fatal("all required rules satisfied → verified")
	}
}

// TestAcceptanceRunner_NonZeroExitFails 非 0 退出 ≠ 环境问题，必须是行为失败。
func TestAcceptanceRunner_NonZeroExitFails(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{{
		ID: "rule-unit", Kind: EvidenceUnit, Required: true,
		Spec: json.RawMessage(`{"cmd":"go test ./..."}`),
	}}, nil)
	runner := &AcceptanceRunner{Exec: func(context.Context, string, string, time.Duration) CommandResult {
		return CommandResult{ExitCode: 1, Output: "--- FAIL: TestX"}
	}}
	verdict := runner.Verify(context.Background(), c, t.TempDir())
	if verdict.Status != VerdictFailed {
		t.Fatalf("non-zero exit must be failed, got %q (%+v)", verdict.Status, verdict)
	}
	if len(verdict.Gaps) != 0 {
		t.Fatalf("a real test failure must not be downgraded to a gap: %v", verdict.Gaps)
	}
}

// TestAcceptanceRunner_EnvironmentErrorIsGap 环境性失败（命令起不来）只能 unverified。
func TestAcceptanceRunner_EnvironmentErrorIsGap(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{{
		ID: "rule-build", Kind: EvidenceStatic, Required: true,
		Spec: json.RawMessage(`{"cmd":"npm run build"}`),
	}}, nil)
	runner := &AcceptanceRunner{Exec: func(context.Context, string, string, time.Duration) CommandResult {
		return CommandResult{ExitCode: -1, EnvironmentError: true, Detail: "executable not found"}
	}}
	verdict := runner.Verify(context.Background(), c, t.TempDir())
	if verdict.Status != VerdictUnverified {
		t.Fatalf("environment error must be unverified, got %q (%+v)", verdict.Status, verdict)
	}
	if len(verdict.Failures) != 0 {
		t.Fatalf("environment error must not be a behaviour failure: %v", verdict.Failures)
	}
}

// TestAcceptanceRunner_MissingDOMProberCannotVerify 没有 DOM 采集能力时 DOM 规则只能 unverified。
func TestAcceptanceRunner_MissingDOMProberCannotVerify(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{
		{ID: "rule-build", Kind: EvidenceStatic, Required: true, Spec: json.RawMessage(`{"cmd":"noop"}`)},
		{ID: "rule-gap", Kind: EvidenceDOM, Required: true, Spec: json.RawMessage(`{}`)},
	}, nil)
	runner := &AcceptanceRunner{Exec: func(context.Context, string, string, time.Duration) CommandResult {
		return CommandResult{ExitCode: 0}
	}}
	verdict := runner.Verify(context.Background(), c, t.TempDir())
	if verdict.Status == VerdictVerified {
		t.Fatalf("build-only evidence must never verify a DOM rule: %+v", verdict)
	}
	joined := strings.Join(verdict.Gaps, " | ")
	if !strings.Contains(joined, "rule-gap") {
		t.Fatalf("gap must name the DOM rule: %v", verdict.Gaps)
	}
	if !verdict.BuildOnlyWarning {
		t.Fatal("build-only scenario must raise the explicit warning")
	}
}

// TestAcceptanceRunner_DOMProberEvidenceIsUsed 注入 prober 后 DOM 规则可被满足。
func TestAcceptanceRunner_DOMProberEvidenceIsUsed(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{
		{ID: "rule-gap", Kind: EvidenceDOM, Required: true, Spec: json.RawMessage(`{}`)},
	}, nil)
	runner := &AcceptanceRunner{
		DOM: stubProber{ev: Evidence{Passed: true, Detail: "gap=60px", Artifact: "dom-snapshot.json"}},
	}
	verdict := runner.Verify(context.Background(), c, t.TempDir())
	if verdict.Status != VerdictVerified {
		t.Fatalf("prober evidence must satisfy the DOM rule, got %q (%+v)", verdict.Status, verdict)
	}
}

// TestAcceptanceRunner_ProberErrorIsEnvironmentGap 采集器报错 → 环境缺口（非通过/非失败）。
func TestAcceptanceRunner_ProberErrorIsEnvironmentGap(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{
		{ID: "rule-visual", Kind: EvidenceVisual, Required: true, Spec: json.RawMessage(`{}`)},
	}, nil)
	runner := &AcceptanceRunner{DOM: nil, Visual: errProber{}}
	verdict := runner.Verify(context.Background(), c, t.TempDir())
	if verdict.Status != VerdictUnverified || len(verdict.Failures) != 0 {
		t.Fatalf("prober failure must be an environment gap, got %+v", verdict)
	}
}

// TestAcceptanceRunner_FileHashInvariant 真实读盘校验 file_hash 不变量。
func TestAcceptanceRunner_FileHashInvariant(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "guard.txt")
	content := []byte("locked content\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	spec, _ := json.Marshal(map[string]string{"file": "guard.txt", "hash": hex.EncodeToString(sum[:])})
	inv := Invariant{
		ID: "inv-guard", Kind: InvariantKindFileHash, Status: InvariantActive,
		Verification: InvariantKindFileHash, Expected: spec,
	}
	c := runnerContract(t, nil, []Invariant{inv})
	runner := &AcceptanceRunner{}
	if verdict := runner.Verify(context.Background(), c, root); verdict.Status != VerdictVerified {
		t.Fatalf("unchanged file must satisfy file_hash invariant: %+v", verdict)
	}
	if err := os.WriteFile(path, []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdict := runner.Verify(context.Background(), c, root)
	if verdict.Status != VerdictFailed {
		t.Fatalf("tampered file must fail the invariant: %+v", verdict)
	}
	if !strings.Contains(strings.Join(verdict.Failures, " "), "inv-guard") {
		t.Fatalf("failure must name the invariant: %v", verdict.Failures)
	}
}

// TestAcceptanceRunner_EvidenceKindMismatchRejected 采集层也不能让 static 满足 DOM 规则。
func TestAcceptanceRunner_EvidenceKindMismatchRejected(t *testing.T) {
	c := runnerContract(t, []AcceptanceRule{
		{ID: "rule-gap", Kind: EvidenceDOM, Required: true, Spec: json.RawMessage(`{"cmd":"noop"}`)},
	}, nil)
	runner := &AcceptanceRunner{Exec: func(context.Context, string, string, time.Duration) CommandResult {
		return CommandResult{ExitCode: 0}
	}}
	// 即使规则 spec 里写了命令，采集层按 Kind=dom 归类，不得退化成命令证据。
	evidence := runner.Collect(context.Background(), c, t.TempDir())
	if len(evidence) != 1 || evidence[0].Kind != EvidenceDOM {
		t.Fatalf("evidence kind must follow the rule kind: %+v", evidence)
	}
	if evidence[0].Passed {
		t.Fatalf("DOM rule without prober must not pass: %+v", evidence[0])
	}
}

type stubProber struct {
	ev  Evidence
	err error
}

func (p stubProber) Probe(context.Context, string, AcceptanceRule) (Evidence, error) {
	return p.ev, p.err
}

type errProber struct{}

func (errProber) Probe(context.Context, string, AcceptanceRule) (Evidence, error) {
	return Evidence{}, os.ErrPermission
}
