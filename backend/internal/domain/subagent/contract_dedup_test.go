package subagent

// contract_dedup_test.go 测试 TODO #70 契约健壮化：
// 注解剥离后签名匹配 + 违例指纹去重（同指纹第二波不重推打回正文）。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// TestSignatureMatched_CommentStripping 签名匹配前剥离行注释（TODO #70）：
// 注解干扰漏匹配产生假违例（实证 2026-08-25）。
func TestSignatureMatched_CommentStripping(t *testing.T) {
	content := "function attack(target, dmg) { // deal damage\n  return target.hp - dmg; # legacy note\n}"
	if !signatureMatched(content, "attack(target, dmg)") {
		t.Fatal("literal match should hit before stripping")
	}
	// 空白归一命中被注释干扰的场景：签名跨行字面在注释后。
	content2 := "// attack (target, dmg) old signature\nfunction attack(target,dmg){}"
	if !signatureMatched(content2, "attack (target, dmg)") {
		t.Fatal("whitespace-normalized match should hit after comment stripping")
	}
	// 真未命中仍判违例。
	if signatureMatched("function heal(target){}", "attack(target, dmg)") {
		t.Fatal("absent signature must not match")
	}
}

// TestStripLineComments 行注释剥离口径。
func TestStripLineComments(t *testing.T) {
	got := stripLineComments("var a = 1; // note\nvar b = 2; # py note\nvar c = 3")
	if strings.Contains(got, "note") || !strings.Contains(got, "var a = 1;") || !strings.Contains(got, "var c = 3") {
		t.Fatalf("strip failed: %q", got)
	}
}

// TestContractViolationDedup 违例指纹去重（TODO #70）：同指纹第二波不重复推送打回正文，
// 收敛为升级提示。
func TestContractViolationDedup(t *testing.T) {
	d := &Dispatcher{}
	mb := mailbox.New()
	d.mailbox = mb
	// 构造 parent spec：含违例契约（签名不在文件中）。
	d.parentSpecs.Store(specRecKey{parentID: "p1", domain: ""}, &parentSpecRecord{
		contract:   &tool.Contract{Signatures: []tool.ContractSignature{{Symbol: "atk", Signature: "attack(target, dmg)", File: "missing-never.js"}}},
		filesMtime: map[string]int64{},
	})
	// 第一波：违例推送（failure marker + 打回正文）。
	d.maybeRunContractChecks("p1")
	msgs := mb.Drain("p1")
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, string(FailureKindContractViolation)) {
		t.Fatalf("first wave should push violation with marker, got %d msgs", len(msgs))
	}
	// 第二波（复验仍未修复）：不重推打回正文，只发升级提示。
	d.maybeRunContractChecks("p1")
	msgs = mb.Drain("p1")
	if len(msgs) != 1 {
		t.Fatalf("second wave should still notify once, got %d", len(msgs))
	}
	if strings.Contains(msgs[0].Body, failureMarker(FailureKindContractViolation, false)) {
		t.Fatalf("second wave must not re-push failure marker, got %q", msgs[0].Body)
	}
	if !strings.Contains(msgs[0].Body, "仍未修复") {
		t.Fatalf("second wave should upgrade wording, got %q", msgs[0].Body)
	}
}

func TestContractViolationFingerprint(t *testing.T) {
	v1 := contractViolation{file: "a.js", detail: "签名 X 未找到"}
	v2 := contractViolation{file: "a.js", detail: "签名 X 未找到"}
	v3 := contractViolation{file: "b.js", detail: "签名 X 未找到"}
	fp1 := violationFingerprint("p1", v1)
	if fp1 != violationFingerprint("p1", v2) {
		t.Fatal("same violation should share fingerprint")
	}
	if fp1 == violationFingerprint("p1", v3) {
		t.Fatal("different file should differ")
	}
	if fp1 == violationFingerprint("p2", v1) {
		t.Fatal("different parent should differ")
	}
}
