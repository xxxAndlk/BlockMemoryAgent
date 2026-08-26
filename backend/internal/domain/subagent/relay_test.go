package subagent

// relay_test.go 测试 TODO #76 接力熔断与【未验证项】强制：
// 派发代数计数（含 takeover 链）、第 3/4 代行为、摘要缺段警告。

import (
	"strings"
	"testing"
)

func TestBumpDispatchGeneration(t *testing.T) {
	d := &Dispatcher{}
	// 前两代：正常计数。
	if g := d.bumpDispatchGeneration("p1", "ui", ""); g != 1 {
		t.Fatalf("gen1 = %d", g)
	}
	if g := d.bumpDispatchGeneration("p1", "ui", ""); g != 2 {
		t.Fatalf("gen2 = %d", g)
	}
	// 第 3 代：达重写评估代数。
	if g := d.bumpDispatchGeneration("p1", "ui", ""); g != 3 {
		t.Fatalf("gen3 = %d", g)
	}
	// 无 domain（叶子）不计数。
	if g := d.bumpDispatchGeneration("p1", "", ""); g != 0 {
		t.Fatalf("leaf dispatch should not count, got %d", g)
	}
	// 其他 domain 独立计数。
	if g := d.bumpDispatchGeneration("p1", "logic", ""); g != 1 {
		t.Fatalf("other domain gen1 = %d", g)
	}
	// 其他 parent 独立计数。
	if g := d.bumpDispatchGeneration("p2", "ui", ""); g != 1 {
		t.Fatalf("other parent gen1 = %d", g)
	}
	// takeover 接管链：ui 已 3 代，takeover=ui 的新 domain 续 4。
	if g := d.bumpDispatchGeneration("p1", "ui-rebuild", "ui"); g != 4 {
		t.Fatalf("takeover chain should continue generation, got %d", g)
	}
	// 旧 domain 键已清零（重新计数为 1）。
	if g := d.bumpDispatchGeneration("p1", "ui", ""); g != 1 {
		t.Fatalf("old domain key should reset after takeover, got %d", g)
	}
}

func TestRelayDeclineAndAssess(t *testing.T) {
	msg := relayDeclineMessage(4, "ui")
	if !strings.Contains(msg, "接力熔断") || !strings.Contains(msg, relayContinueMarker) {
		t.Fatalf("decline message should explain fuse + marker: %q", msg)
	}
	if !taskHasRelayReason("修一下按钮 " + relayContinueMarker + " 前序结构尚可继续修补") {
		t.Fatal("task with marker should pass")
	}
	if taskHasRelayReason("普通任务文本") {
		t.Fatal("task without marker should not pass")
	}
}

func TestUnverifiedSectionWarning(t *testing.T) {
	if !unverifiedSectionMissing("完成摘要正文，无未验证段") {
		t.Fatal("summary without section should be flagged")
	}
	if unverifiedSectionMissing("结论\n【未验证项】无") {
		t.Fatal("summary with section should not be flagged")
	}
	// 空 MachineCheck → 警告独段。
	warn := appendUnverifiedWarning("")
	if !strings.Contains(warn, "未声明未验证项") {
		t.Fatalf("empty check should get standalone warning: %q", warn)
	}
	// 非空 MachineCheck → 追加。
	warn = appendUnverifiedWarning("【机器校验】node --check 通过")
	if !strings.Contains(warn, "node --check") || !strings.Contains(warn, "未声明未验证项") {
		t.Fatalf("warning should append to existing check: %q", warn)
	}
}
