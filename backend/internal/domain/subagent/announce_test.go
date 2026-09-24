package subagent

// announce_test.go announce 边界协议（TODO #22②）：回报信封、动态回灌预算、fork 硬顶 isolated。

import (
	"strings"
	"testing"
)

// TestAnnounceBudgetFormula 验证回灌预算公式：min(静态封顶, 父剩余×0.5÷子数)，floor 2K。
func TestAnnounceBudgetFormula(t *testing.T) {
	// 单子：150000/2/1 = 75000 > 静态封顶 4000 → 封顶。
	if got := announceBudgetRunes(1); got != announceStaticCapRunes {
		t.Fatalf("1 child budget = %d, want %d", got, announceStaticCapRunes)
	}
	// 高扇出 100 子：150000/2/100 = 750 → floor 2K。
	if got := announceBudgetRunes(100); got != announceBudgetFloorRunes {
		t.Fatalf("100 children budget = %d, want floor %d", got, announceBudgetFloorRunes)
	}
	// 中扇出 30 子：150000/2/30 = 2500 → 介于 floor 与封顶之间。
	if got := announceBudgetRunes(30); got != 2500 {
		t.Fatalf("30 children budget = %d, want 2500", got)
	}
	// 零值防御：按 1 子处理。
	if got := announceBudgetRunes(0); got != announceStaticCapRunes {
		t.Fatalf("0 children budget = %d, want %d", got, announceStaticCapRunes)
	}
}

// TestBuildAnnounce_Envelope 验证回报信封：summary 原文置顶（机读失败标记首行不动），
// 尾部【回报】块带 Status/Notes/统计行。
func TestBuildAnnounce_Envelope(t *testing.T) {
	d := &Dispatcher{}
	body := d.buildAnnounce("s1", "s1/domain-1",
		"[failure kind=error retryable=false]\n炸了", []string{"a.go", "b.go"}, announceStaticCapRunes)
	if !strings.HasPrefix(body, "[failure kind=error retryable=false]") {
		t.Fatalf("machine-readable marker must stay on line 1, got:\n%s", body)
	}
	if !strings.Contains(body, "【回报】") || !strings.Contains(body, "- Status: failed") {
		t.Fatalf("announce tail must carry status, got:\n%s", body)
	}
	if !strings.Contains(body, "修改文件 2 个") || !strings.Contains(body, "- 统计:") {
		t.Fatalf("announce tail must carry notes+stats, got:\n%s", body)
	}

	// done 状态 + 无文件。
	body2 := d.buildAnnounce("s1", "s1/domain-1", "全部完成", nil, 0)
	if !strings.HasPrefix(body2, "全部完成") || !strings.Contains(body2, "- Status: done") {
		t.Fatalf("done envelope mismatch, got:\n%s", body2)
	}
	// 部分完成 → partial。
	body3 := d.buildAnnounce("s1", "s1/domain-1", "部分完成：做了 A", nil, 0)
	if !strings.Contains(body3, "- Status: partial") {
		t.Fatalf("partial status mismatch, got:\n%s", body3)
	}
	// verify_missing → delivered-unverified（三态黄）。
	body4 := d.buildAnnounce("s1", "s1/domain-1", "[failure kind=verify_missing retryable=false]\n缺证据", nil, 0)
	if !strings.Contains(body4, "- Status: delivered-unverified") {
		t.Fatalf("unverified status mismatch, got:\n%s", body4)
	}
}

// TestCapSpawnPrefixes 验证派发前上下文预算：超 fork 硬顶转 isolated（前缀全弃不硬灌）。
func TestCapSpawnPrefixes(t *testing.T) {
	small := []string{"p1", "p2"}
	got, task, isolated := capSpawnPrefixes(small, "干活")
	if isolated || len(got) != 2 || task != "干活" {
		t.Fatalf("under-cap must pass through, got %v %q %v", got, task, isolated)
	}

	big := make([]rune, spawnForkHardCapRunes+1)
	for i := range big {
		big[i] = 'x'
	}
	got2, task2, isolated2 := capSpawnPrefixes([]string{string(big)}, "干活")
	if !isolated2 || len(got2) != 0 {
		t.Fatalf("over-cap must drop prefixes (isolated), got %d prefixes isolated=%v", len(got2), isolated2)
	}
	if !strings.HasPrefix(task2, "干活") || !strings.Contains(task2, "isolated") {
		t.Fatalf("isolated task must keep body + notice, got: %s", truncateRunes(task2, 200))
	}
}
