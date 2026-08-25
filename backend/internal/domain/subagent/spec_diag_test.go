package subagent

// spec_diag_test.go 测试 TODO #74 spec 诊断精确化：
// missing 列已有 key、唯一 keyed spec 候选回退、墓碑（invalidated）与从未写区分。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestHasFreshSpec_MissingListsExistingKeys(t *testing.T) {
	d := &Dispatcher{}
	kv := newTestKVMemory(true)
	d.sharedMem = kv

	// 空 store：missing 不带 key 清单（从未写）。
	ok, reason := d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if ok || strings.Contains(reason, "已写入的 spec key") {
		t.Fatalf("empty store should be plain missing, got ok=%v reason=%q", ok, reason)
	}

	// 写入一个自由槽位（非 spec）：仍不列（无 spec 键）。
	kv.items["meta-1:file_tree"] = "tree"
	_, reason = d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if strings.Contains(reason, "已写入的 spec key") {
		t.Fatalf("non-spec slot should not be listed: %q", reason)
	}

	// 写入 key=fruit-art-spec 的 spec（与派发 domain=fruit-game 不匹配）：
	// 唯一 keyed spec -> 唯一候选回退放行（#74②，自由命名 key 错配兜底）。
	kv.items["meta-1:spec:fruit-art-spec"] = tool.EncodeSpecMD("meta-1", tool.Spec{
		Goal: "g", Acceptance: []string{"a"},
	}, nil)
	ok, _ = d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if !ok {
		t.Fatal("single mismatched keyed spec should fall back and pass")
	}

	// 第二个 keyed spec 出现后无法唯一定位：missing 文案列出已有 key，直指错配。
	kv.items["meta-1:spec:fruit-logic-spec"] = tool.EncodeSpecMD("meta-1", tool.Spec{
		Goal: "g", Acceptance: []string{"a"},
	}, nil)
	ok, reason = d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if ok {
		t.Fatal("two keyed specs with mismatched domain should fail")
	}
	if !strings.Contains(reason, "已写入的 spec key") || !strings.Contains(reason, "fruit-art-spec") || !strings.Contains(reason, "fruit-logic-spec") {
		t.Fatalf("reason should list existing keys, got %q", reason)
	}
}

func TestHasFreshSpec_UniqueCandidateFallback(t *testing.T) {
	d := &Dispatcher{}
	kv := newTestKVMemory(true)
	d.sharedMem = kv

	// 仅存在一个 keyed spec（key 与 domain 不一致）：唯一候选回退放行。
	kv.items["meta-1:spec:fruit-game-spec"] = tool.EncodeSpecMD("meta-1", tool.Spec{
		Goal: "g", Acceptance: []string{"a"},
	}, nil)
	ok, reason := d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if !ok {
		t.Fatalf("unique keyed spec should fall back and pass, got reason=%q", reason)
	}

	// 两个 keyed spec：无法唯一定位，回退失效，仍报 missing 列出两 key。
	kv.items["meta-1:spec:fruit-art-spec"] = tool.EncodeSpecMD("meta-1", tool.Spec{
		Goal: "g", Acceptance: []string{"a"},
	}, nil)
	ok, reason = d.hasFreshSpec(context.Background(), "meta-1", "fruit-game")
	if ok {
		t.Fatal("two keyed specs should not fall back")
	}
	if !strings.Contains(reason, "fruit-art-spec") || !strings.Contains(reason, "fruit-game-spec") {
		t.Fatalf("reason should list both keys: %q", reason)
	}
}

func TestCheckSpecKey_TombstoneVsMissing(t *testing.T) {
	d := &Dispatcher{}
	kv := newTestKVMemory(true)
	d.sharedMem = kv

	// 墓碑值：报 invalidated（文件变更）而非 missing。
	kv.items["meta-1:spec"] = tool.SpecTombstonePrefix + "game.js 已变更，请用 WriteSpec 重写"
	ok, reason := d.checkSpecKey(context.Background(), "meta-1:spec")
	if ok {
		t.Fatal("tombstone should not pass")
	}
	if !strings.Contains(reason, "spec invalidated") || !strings.Contains(reason, "game.js") {
		t.Fatalf("tombstone reason wrong: %q", reason)
	}

	// 墓碑键不计入 specKeysOfParent（missing 诊断不受污染）。
	if keys := d.specKeysOfParent(context.Background(), "meta-1"); len(keys) != 0 {
		t.Fatalf("tombstone should be excluded from key listing, got %v", keys)
	}
}
