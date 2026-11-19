package team

// acceptance_eval_test.go 对应计划 §7.4 必测场景（可确定性验证的部分）：
//   - 构建成功但 60px 间距改变 → 验收失败（不是"构建通过就 PASS"）；
//   - 构建成功但非目标 AST 节点改变 → 验收失败；
//   - 环境 EBUSY 不被误判为代码行为通过或失败；
//   - 必需证据缺失（真实 DOM 不可用）→ 只 unverified，不伪造 PASS；
//   - 合同 revision 改变后旧证据不能复用（证据按规则 ID 绑定，这里验证规则变更是缺口）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func acceptanceContract(t *testing.T, invariants []Invariant, acceptance []AcceptanceRule) *LogicalTaskContract {
	t.Helper()
	c := &LogicalTaskContract{
		LogicalTaskID: "lt-acc", SessionID: "s-acc", Revision: 3, Status: TaskStatusAwaitingVerification,
		Goal:       "修省略且不破坏间距",
		Invariants: invariants,
		Acceptance: acceptance,
	}
	if err := NormalizeContract(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func gapInvariantForTest(t *testing.T, id string, px float64) Invariant {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"metric": "gap_after", "delta_px": px})
	if err != nil {
		t.Fatal(err)
	}
	return Invariant{
		ID: id, Kind: InvariantKindDOMMetric, Status: InvariantActive, Verification: InvariantKindDOMMetric,
		Text: "间距 +" + formatPx(px), Expected: payload,
	}
}

func formatPx(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strings.TrimRight(
		strings.ReplaceAll(strings.TrimSpace(
			func() string { b, _ := json.Marshal(v); return string(b) }()), "\"", ""), "0"), "."), ".")
}

// TestEvaluateVerification_BuildPassButInvariantBroken 事故第 21～22 轮：
// 构建通过、但 60px 间距被改 → 必须判 failed，不得因为 build EXIT=0 而 PASS。
func TestEvaluateVerification_BuildPassButInvariantBroken(t *testing.T) {
	c := acceptanceContract(t,
		[]Invariant{gapInvariantForTest(t, "gap-time-60", 60)},
		[]AcceptanceRule{
			{ID: "rule-build", Kind: "static", Required: true, Spec: json.RawMessage(`{"cmd":"npm run build"}`)},
			{ID: "rule-dom-gap", Kind: "dom", Required: true, InvariantIDs: []string{"gap-time-60"}, Spec: json.RawMessage(`{}`)},
		})
	verdict := EvaluateVerification(c,
		[]InvariantResult{{ID: "gap-time-60", OK: false, Observed: "实测 8px（应为 60px）"}},
		[]Evidence{
			{Kind: EvidenceStatic, ID: "rule-build", Passed: true, Detail: "EXIT_CODE=0"},
			{Kind: EvidenceDOM, ID: "rule-dom-gap", Passed: false, Detail: "time 元素相对 name 元素位移 8px"},
		})
	if verdict.Status != VerdictFailed {
		t.Fatalf("status = %q, want failed (build pass must not override invariant failure)", verdict.Status)
	}
	joined := strings.Join(verdict.Failures, " | ")
	if !strings.Contains(joined, "gap-time-60") {
		t.Fatalf("failures must name the broken invariant: %v", verdict.Failures)
	}
	if strings.Contains(strings.Join(verdict.Gaps, " | "), "gap-time-60") {
		t.Fatalf("broken invariant must be a failure, not a gap: %v", verdict.Gaps)
	}
}

// TestEvaluateVerification_MissingRequiredEvidenceIsUnverified 真实 DOM 不可用 → unverified。
func TestEvaluateVerification_MissingRequiredEvidenceIsUnverified(t *testing.T) {
	c := acceptanceContract(t, nil, []AcceptanceRule{
		{ID: "rule-dom", Kind: "dom", Required: true, Spec: json.RawMessage(`{}`)},
	})
	verdict := EvaluateVerification(c, nil, []Evidence{
		{Kind: EvidenceStatic, ID: "rule-build", Passed: true},
	})
	if verdict.Status != VerdictUnverified {
		t.Fatalf("status = %q, want delivered-unverified", verdict.Status)
	}
	if len(verdict.Gaps) == 0 {
		t.Fatal("missing required evidence must be listed as a gap")
	}
	if !verdict.BuildOnlyWarning {
		t.Fatal("build-only evidence must raise the explicit warning")
	}
}

// TestEvaluateVerification_EnvironmentErrorNotTreatedAsPass EBUSY 既不算通过也不算行为失败。
func TestEvaluateVerification_EnvironmentErrorNotTreatedAsPass(t *testing.T) {
	c := acceptanceContract(t, nil, []AcceptanceRule{
		{ID: "rule-build", Kind: "static", Required: true, Spec: json.RawMessage(`{}`)},
	})
	verdict := EvaluateVerification(c, nil, []Evidence{
		{Kind: EvidenceStatic, ID: "rule-build", EnvironmentError: true, Detail: "EBUSY: dist/favicon.ico"},
	})
	if verdict.Status != VerdictUnverified {
		t.Fatalf("status = %q, want unverified for environment error", verdict.Status)
	}
	if len(verdict.Failures) != 0 {
		t.Fatalf("environment error must not be classified as behaviour failure: %v", verdict.Failures)
	}
	if !strings.Contains(strings.Join(verdict.Gaps, "|"), "EBUSY") {
		t.Fatalf("gap must explain the environment error: %v", verdict.Gaps)
	}
}

// TestEvaluateVerification_InvariantSkippedIsGap 不变量无法校验（缺基线）→ unverified。
func TestEvaluateVerification_InvariantSkippedIsGap(t *testing.T) {
	c := acceptanceContract(t, []Invariant{gapInvariantForTest(t, "gap-name-30", 30)}, nil)
	verdict := EvaluateVerification(c,
		[]InvariantResult{{ID: "gap-name-30", Skipped: true, Observed: "无 DOM 基线"}}, nil)
	if verdict.Status != VerdictUnverified {
		t.Fatalf("status = %q, want unverified", verdict.Status)
	}
	if len(verdict.Failures) != 0 {
		t.Fatalf("skipped invariant must not be a failure: %v", verdict.Failures)
	}
}

// TestEvaluateVerification_AllGreen 全绿 → verified。
func TestEvaluateVerification_AllGreen(t *testing.T) {
	c := acceptanceContract(t,
		[]Invariant{gapInvariantForTest(t, "gap-name-30", 30)},
		[]AcceptanceRule{{ID: "rule-dom", Kind: "dom", Required: true, Spec: json.RawMessage(`{}`)}})
	verdict := EvaluateVerification(c,
		[]InvariantResult{{ID: "gap-name-30", OK: true, Observed: "30px"}},
		[]Evidence{{Kind: EvidenceDOM, ID: "rule-dom", Passed: true}})
	if verdict.Status != VerdictVerified {
		t.Fatalf("status = %q, want verified (%+v)", verdict.Status, verdict)
	}
}

// TestEvaluateVerification_NoContractIsUnverified legacy 会话不伪造 PASS。
func TestEvaluateVerification_NoContractIsUnverified(t *testing.T) {
	verdict := EvaluateVerification(nil, nil, []Evidence{{Kind: EvidenceStatic, Passed: true}})
	if verdict.Status != VerdictUnverified {
		t.Fatalf("legacy contract must be unverified, got %q", verdict.Status)
	}
}

// TestEvaluateVerification_RevisionChangeInvalidatesOldEvidence 规则 ID 变更后旧证据不可复用。
func TestEvaluateVerification_RevisionChangeInvalidatesOldEvidence(t *testing.T) {
	// 旧 revision 的证据 ID 是 rule-dom-v3，新 revision 引入了 rule-dom-v4。
	c := acceptanceContract(t, nil, []AcceptanceRule{
		{ID: "rule-dom-v4", Kind: "dom", Required: true, Spec: json.RawMessage(`{}`)},
	})
	verdict := EvaluateVerification(c, nil, []Evidence{
		{Kind: EvidenceDOM, ID: "rule-dom-v3", Passed: true},
	})
	if verdict.Status != VerdictUnverified {
		t.Fatalf("stale evidence must not satisfy new revision, got %q", verdict.Status)
	}
}

// TestRenderVerdict_ExplainsGaps 交付文本必须能解释"为什么未验证"，而不是一句 PASS。
func TestRenderVerdict_ExplainsGaps(t *testing.T) {
	text := RenderVerdict(VerificationVerdict{
		Status:           VerdictUnverified,
		Gaps:             []string{"必需证据缺失 rule-dom（kind=dom）"},
		BuildOnlyWarning: true,
	})
	for _, want := range []string{"未验证", "必需证据缺失", "构建通过不代表目标完成"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered verdict missing %q:\n%s", want, text)
		}
	}
}
