package subagent

import (
	"testing"
	"time"
)

// TestActivityReporterChildWait 验证 child_wait 是"纯展示态"：
// 只换 lastKind 供 ActivityEvidenceOf 读取；不刷新 lastTS（不续命）、不冒泡——
// 等子 Agent 的存活判定不受展示态上报干扰。
func TestActivityReporterChildWait(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.activity.Store("session-1/domain-1", newEvidence())

	report := d.activityReporterFn("session-1/domain-1")
	e := d.activityEvidenceFor("session-1/domain-1")
	before := e.lastTS.Load()
	time.Sleep(time.Millisecond)

	report("child_wait")

	kind, _, ok := d.ActivityEvidenceOf("session-1/domain-1")
	if !ok || kind != "child_wait" {
		t.Fatalf("ActivityEvidenceOf kind=%q ok=%t；want child_wait", kind, ok)
	}
	if got := e.lastTS.Load(); got != before {
		t.Fatalf("child_wait 不应刷新 lastTS: before=%d after=%d", before, got)
	}

	// 普通 kind 仍走 report 主路径（回归保护）。
	report("llm_start")
	if !e.llmInFlight.Load() {
		t.Fatal("llm_start 应置 llmInFlight")
	}
}

// TestChildWaitStickyAcrossDescendantBubbling 钉死"等子展示态不被后代冒泡刷掉"：
// 等子期间后代每次活动都会给父 stamp("descendant")——若它无条件覆盖 lastKind，
// child_wait 的占空比趋近 0，前端恒显"执行中"、直连发送恒 409（评审发现）。
// 要求：后代冒泡只续命（lastTS 前进）不换 kind；Agent 自身活动（llm_start 等）才清除。
func TestChildWaitStickyAcrossDescendantBubbling(t *testing.T) {
	e := newEvidence()
	e.markChildWait()

	before := e.lastTS.Load()
	time.Sleep(time.Millisecond)
	e.stamp("descendant", time.Now().UnixNano())

	if got := e.kindString(); got != "child_wait" {
		t.Fatalf("后代冒泡不应覆盖等子展示态: kind=%q", got)
	}
	if e.lastTS.Load() == before {
		t.Fatal("后代冒泡必须续命（刷新 lastTS），否则等子的父会被心跳巡检误杀")
	}

	// 自身恢复活动 → 清除等子态，展示面回到真实活动。
	e.stamp("llm_start", time.Now().UnixNano())
	if got := e.kindString(); got != "llm_start" {
		t.Fatalf("自身活动应清除等子态: kind=%q", got)
	}
	// 等子态清除后，后代冒泡恢复原有展示语义（显示 descendant），但绝不能"复活"child_wait。
	e.stamp("descendant", time.Now().UnixNano())
	if got := e.kindString(); got == "child_wait" {
		t.Fatal("等子态已清除，后代冒泡不应把展示态拉回 child_wait")
	}
}
