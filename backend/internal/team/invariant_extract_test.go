package team

// invariant_extract_test.go 事故第 20～22 轮的确定性回放（计划 §4.2 第 2 项）：
//   - 第 20 轮 "商品编码到商品名称的间距+30px，商品名称到下单时间 +60px"
//     必须提取出两条 dom_metric 不变量；
//   - 第 21 轮 "修省略" 不得产生新不变量，也不得撤销已生效的间距约束；
//   - 第 22 轮 "之前那个距离保持不要动" 必须被识别为纠正/保持语义；
//   - "不要动订单号" 生成 freeze 不变量；
//   - 无法机器验证的表述不生成不变量（更不得生成"自然语言不变量"）。

import (
	"encoding/json"
	"testing"
)

const vueFile = "web/src/view/distribution/disOrder/disOrder.vue"

// jsonUnmarshal 测试辅助：解 RawMessage（避免测试文件污染生产 import）。
func jsonUnmarshal(raw json.RawMessage, out any) error { return json.Unmarshal(raw, out) }

// mustNormalized 测试辅助：规范化合同并断言成功。
func mustNormalized(t *testing.T, c *LogicalTaskContract) *LogicalTaskContract {
	t.Helper()
	if err := NormalizeContract(c); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return c
}

func fixtureHeaderTargets() []SemanticTarget {
	return []SemanticTarget{
		{ID: "hdr-ordersn", File: vueFile, Kind: "template_node", Anchor: "header-item-ordersn", Label: "订单号"},
		{ID: "hdr-encoding", File: vueFile, Kind: "template_node", Anchor: "header-item-encoding", Label: "商品编码"},
		{ID: "hdr-name", File: vueFile, Kind: "template_node", Anchor: "header-item-name", Label: "商品名称"},
		{ID: "hdr-time", File: vueFile, Kind: "template_node", Anchor: "header-item-time", Label: "下单时间"},
	}
}

// TestExtractInvariants_Turn20GapInstruction 第 20 轮：+30px/+60px 必须落成机器可验证不变量。
func TestExtractInvariants_Turn20GapInstruction(t *testing.T) {
	text := "给商品编码到商品名称的间距+30px，给商品名称到时间 的间距+60px"
	res := ExtractInvariants(text, fixtureHeaderTargets(), 1201, "lt-1", 4)
	if len(res.Invariants) != 2 {
		t.Fatalf("want 2 gap invariants, got %d: %+v (notes=%v)", len(res.Invariants), res.Invariants, res.Notes)
	}
	values := map[float64]bool{}
	for _, inv := range res.Invariants {
		if inv.Kind != InvariantKindDOMMetric {
			t.Fatalf("gap invariant kind = %q, want dom_metric", inv.Kind)
		}
		var payload struct {
			DeltaPX float64 `json:"delta_px"`
			Metric  string  `json:"metric"`
		}
		if err := jsonUnmarshal(inv.Expected, &payload); err != nil {
			t.Fatalf("decode expected: %v", err)
		}
		if payload.Metric != "gap_after" {
			t.Fatalf("metric = %q, want gap_after", payload.Metric)
		}
		values[payload.DeltaPX] = true
		if inv.SourceEvent != 1201 || inv.SourceTaskID != "lt-1" {
			t.Fatalf("invariant must carry source (event=%d task=%s)", inv.SourceEvent, inv.SourceTaskID)
		}
		if inv.Status != InvariantActive {
			t.Fatalf("fresh invariant must be active, got %q", inv.Status)
		}
	}
	for _, want := range []float64{30, 60} {
		if !values[want] {
			t.Fatalf("missing +%gpx invariant, got %v", want, values)
		}
	}
}

// TestExtractInvariants_Turn21NoNewInvariantFromBugFix 第 21 轮"修省略"不产生约束、
// 也不得撤销第 20 轮约束（撤销只能由显式撤销语义触发）。
func TestExtractInvariants_Turn21NoNewInvariantFromBugFix(t *testing.T) {
	prev := &LogicalTaskContract{
		LogicalTaskID: "lt-1", SessionID: "s", Revision: 4, Status: TaskStatusAwaitingVerification,
		Invariants: ExtractInvariants("给商品编码到商品名称的间距+30px，给商品名称到时间 的间距+60px",
			fixtureHeaderTargets(), 1201, "lt-1", 4).Invariants,
	}
	res := ExtractInvariants("商品名称的长度还没接近下单时间标题就已经省略了，这点修复下，订单号与商品编码不能省略",
		fixtureHeaderTargets(), 1300, "lt-1", 5)
	if len(res.Invariants) != 0 {
		t.Fatalf("bug-fix instruction must not invent invariants, got %+v", res.Invariants)
	}
	inherit := InheritInvariants(prev, fixtureHeaderTargets(), nil, "商品名称的长度还没接近下单时间标题就已经省略了")
	if len(inherit.Inherited) != 2 {
		t.Fatalf("turn-20 gaps must stay active in turn 21, got %+v", inherit)
	}
	if len(inherit.Revoked) != 0 {
		t.Fatalf("bug-fix must not revoke gaps, got %+v", inherit.Revoked)
	}
}

// TestExtractInvariants_Turn22KeepDistanceIsCorrection 第 22 轮语义识别：
// "之前那个距离保持不要动，要改的是省略的字符数" = 纠正 + 保持。
func TestExtractInvariants_Turn22KeepDistanceIsCorrection(t *testing.T) {
	if !IsCorrection("你为什么把下单时间放到最后了？之前那个距离保持不要动，要改的是省略的字符数或者是长度") {
		t.Fatal("turn-22 input must be classified as correction")
	}
	res := ExtractInvariants("之前那个距离保持不要动", fixtureHeaderTargets(), 1400, "lt-1", 6)
	// 保守策略：这句话没有具体数值，不产生新的可验证不变量（保持语义由已有 active 项覆盖）。
	if len(res.Invariants) != 0 {
		t.Fatalf("non-numeric keep instruction must not invent invariants, got %+v", res.Invariants)
	}
}

// TestExtractInvariants_NoTouchGeneratesFreeze "不要动订单号" 生成 file_hash 冻结项。
func TestExtractInvariants_NoTouchGeneratesFreeze(t *testing.T) {
	res := ExtractInvariants("原本的订单号位置不要动", fixtureHeaderTargets(), 1500, "lt-2", 1)
	if len(res.Invariants) != 1 {
		t.Fatalf("want 1 freeze invariant, got %+v", res.Invariants)
	}
	inv := res.Invariants[0]
	if inv.Kind != InvariantKindFileHash {
		t.Fatalf("freeze invariant kind = %q", inv.Kind)
	}
	if err := ValidateContract(mustNormalized(t, &LogicalTaskContract{
		LogicalTaskID: "lt-2", SessionID: "s", Revision: 1, Status: TaskStatusActive,
		Invariants: res.Invariants,
	})); err != nil {
		t.Fatalf("freeze invariant must be machine-verifiable: %v", err)
	}
}

// TestExtractInvariants_IgnoresVagueStatements 模糊表述不产生不变量。
func TestExtractInvariants_IgnoresVagueStatements(t *testing.T) {
	for _, text := range []string{
		"再好看一点",
		"间距不太对",
		"优化一下排版",
	} {
		res := ExtractInvariants(text, nil, 1, "", 1)
		if len(res.Invariants) != 0 {
			t.Fatalf("%q must not produce invariants, got %+v", text, res.Invariants)
		}
	}
}

// TestExtractInvariants_StableIDs 同一表述重复提取得到相同 ID（便于去重与幂等）。
func TestExtractInvariants_StableIDs(t *testing.T) {
	text := "商品名称 +30px"
	a := ExtractInvariants(text, nil, 1, "", 1)
	b := ExtractInvariants(text, nil, 2, "", 2)
	if len(a.Invariants) != 1 || len(b.Invariants) != 1 {
		t.Fatalf("want one invariant each, got %+v / %+v", a, b)
	}
	if a.Invariants[0].ID != b.Invariants[0].ID {
		t.Fatalf("invariant ID must be stable: %q vs %q", a.Invariants[0].ID, b.Invariants[0].ID)
	}
}
