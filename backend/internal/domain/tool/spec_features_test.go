package tool

// spec_features_test.go 测试 TODO #67/#68/#69/#70/#74/#75 的 WriteSpec 侧功能：
// acceptance 双形态解析、contract 代码形态校验、baseline 强制、key 往返、
// FormatAcceptanceLine/ParseAcceptanceLine 对称性。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptanceArg_DualForm(t *testing.T) {
	// 纯字符串形态透传。
	out, err := parseAcceptanceArg([]any{"条目一", "条目二"})
	if err != "" || len(out) != 2 || out[0] != "条目一" {
		t.Fatalf("string form passthrough failed: out=%v err=%q", out, err)
	}
	// 结构化形态渲染为带标记行。
	out, err = parseAcceptanceArg([]any{map[string]any{
		"text": "页面可打开", "evidence": "probe", "layer": "quality",
	}})
	if err != "" {
		t.Fatalf("structured form failed: err=%q", err)
	}
	if len(out) != 1 || out[0] != "页面可打开 [evidence:probe layer:quality]" {
		t.Fatalf("unexpected rendered line: %q", out[0])
	}
	// 混合形态。
	out, err = parseAcceptanceArg([]any{"纸面条目", map[string]any{"text": "有截图", "evidence": "screenshot"}})
	if err != "" || len(out) != 2 || out[1] != "有截图 [evidence:screenshot]" {
		t.Fatalf("mixed form failed: out=%v err=%q", out, err)
	}
	// 非法 evidence。
	_, err = parseAcceptanceArg([]any{map[string]any{"text": "x", "evidence": "vibes"}})
	if err == "" || !strings.Contains(err, "非法") {
		t.Fatalf("illegal evidence should error, got %q", err)
	}
	// 缺 text。
	_, err = parseAcceptanceArg([]any{map[string]any{"evidence": "command"}})
	if err == "" {
		t.Fatal("missing text should error")
	}
}

func TestAcceptanceLineRoundTrip(t *testing.T) {
	cases := []struct{ text, evidence, layer string }{
		{"页面可打开", "probe", "quality"},
		{"有截图", "screenshot", ""},
		{"跑测试", "command", "functional"},
	}
	for _, c := range cases {
		line := FormatAcceptanceLine(c.text, c.evidence, c.layer)
		gotText, gotEv, gotLayer := ParseAcceptanceLine(line)
		if gotText != c.text || gotEv != c.evidence {
			t.Fatalf("round trip mismatch: line=%q got=(%q,%q,%q)", line, gotText, gotEv, gotLayer)
		}
		if c.layer == "" || c.layer == "functional" {
			if gotLayer != "" && gotLayer != "functional" {
				t.Fatalf("functional layer round trip mismatch: %q", gotLayer)
			}
		} else if gotLayer != c.layer {
			t.Fatalf("layer round trip mismatch: %q vs %q", gotLayer, c.layer)
		}
	}
	// 无标记行 → manual。
	text, ev, layer := ParseAcceptanceLine("纯纸面条目")
	if text != "纯纸面条目" || ev != "" || layer != "" {
		t.Fatalf("unmarked line should parse as manual, got (%q,%q,%q)", text, ev, layer)
	}
}

func TestValidateContractShape_RejectsProse(t *testing.T) {
	// 全角分号 + 中文注解（2026-08-25 水果忍者实证形态）。
	bad := &Contract{Signatures: []ContractSignature{{
		Symbol: "FruitNinjaGame", File: "game.js",
		Signature: "window.FruitNinjaGame = { start(mode), version }；mode ∈ 'classic'|'arcade'",
	}}}
	if msg := ValidateContractShape(bad); msg == "" {
		t.Fatal("prose signature with fullwidth chars should be rejected")
	}
	// CJK 字符。
	bad2 := &Contract{Symbols: []ContractSymbol{{Symbol: "游戏引擎.init", File: "a.js"}}}
	if msg := ValidateContractShape(bad2); msg == "" {
		t.Fatal("CJK symbol should be rejected")
	}
	// 合法代码形态。
	ok := &Contract{
		Symbols: []ContractSymbol{{Symbol: "GameEngine.init", File: "a.js"}},
		Signatures: []ContractSignature{{
			Symbol: "attack", File: "a.js", Signature: "attack(target, dmg)",
		}},
	}
	if msg := ValidateContractShape(ok); msg != "" {
		t.Fatalf("legal contract shape rejected: %q", msg)
	}
}

func TestHasFidelityKeyword(t *testing.T) {
	if !HasFidelityKeyword("高还原度复刻水果忍者") {
		t.Fatal("fidelity keyword should match")
	}
	if HasFidelityKeyword("实现一个新游戏") {
		t.Fatal("no fidelity keyword should not match")
	}
}

func TestWriteSpec_BaselineEnforcedForFidelityGoal(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 还原类 goal + 空 baseline → 拒收。
	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Success {
		t.Fatal("fidelity goal without baseline should be rejected")
	}
	if !strings.Contains(res.Error, "baseline") {
		t.Fatalf("error should mention baseline, got %q", res.Error)
	}

	// baseline 引用文件不存在 → 拒收。
	res, _ = r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline":   []any{"refs/fruit-ref.png"},
	})
	if res.Success {
		t.Fatal("nonexistent baseline file should be rejected")
	}

	// baseline 落盘 → 通过。
	ref := filepath.Join(dir, "fruit-ref.png")
	if werr := os.WriteFile(ref, []byte("png"), 0644); werr != nil {
		t.Fatalf("write ref: %v", werr)
	}
	res, _ = r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline":   []any{ref},
	})
	if !res.Success {
		t.Fatalf("valid baseline should pass, got err=%q", res.Error)
	}

	// 非还原类 goal 无 baseline → 通过（零行为变化）。
	res, _ = r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "实现一个全新游戏",
		"acceptance": []any{"可玩"},
	})
	if !res.Success {
		t.Fatalf("non-fidelity goal should pass without baseline, got err=%q", res.Error)
	}
}

func TestWriteSpec_ProbesScenesBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.png")
	if err := os.WriteFile(ref, []byte("png"), 0644); err != nil {
		t.Fatalf("write ref: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "g",
		"acceptance": []any{map[string]any{"text": "探针通过", "evidence": "probe"}},
		"probes":     []any{"打开主菜单", "点击开始断言实体生成"},
		"scenes":     []any{"主菜单", "游玩中"},
		"baseline":   []any{ref},
		"key":        "fruit-game",
	})
	if err != nil || !res.Success {
		t.Fatalf("WriteSpec: err=%v res=%+v", err, res)
	}
	val, ok := store.items["meta-1:spec:fruit-game"]
	if !ok {
		t.Fatal("keyed spec entry missing")
	}
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatal("decode spec failed")
	}
	if len(fm.Probes) != 2 || len(fm.Scenes) != 2 || len(fm.Baseline) != 1 {
		t.Fatalf("probes/scenes/baseline round trip failed: %+v", fm)
	}
	if len(fm.Acceptance) != 1 || !strings.Contains(fm.Acceptance[0], "[evidence:probe]") {
		t.Fatalf("structured acceptance not persisted: %v", fm.Acceptance)
	}
}

func TestWriteSpec_BaselineContentInline(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	store.workDir = dir
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 还原类 goal + baseline_content 内联 → 自动落盘 .bma/baseline/ 并通过。
	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline_content": []any{map[string]any{
			"path":    "fruit-baseline.md",
			"content": "# Fruit Ninja 基线\n- 切水果 1 分",
		}},
	})
	if err != nil || !res.Success {
		t.Fatalf("baseline_content should succeed: err=%v res=%+v", err, res)
	}
	dst := filepath.Join(dir, ".bma", "baseline", "fruit-baseline.md")
	data, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("baseline file not written: %v", rerr)
	}
	if !strings.Contains(string(data), "切水果 1 分") {
		t.Fatalf("baseline content mismatch: %q", data)
	}

	// frontmatter Baseline 记录相对路径。
	val, ok := store.items["meta-1:spec"]
	if !ok {
		t.Fatal("spec entry missing")
	}
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatal("decode spec failed")
	}
	if len(fm.Baseline) != 1 || fm.Baseline[0] != ".bma/baseline/fruit-baseline.md" {
		t.Fatalf("baseline should record written path, got %v", fm.Baseline)
	}
}

func TestWriteSpec_BaselineContentPathEscape(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	store.workDir = dir
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 路径穿越：只取文件名，落盘仍在 .bma/baseline/ 内。
	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline_content": []any{map[string]any{
			"path":    "../escape.md",
			"content": "escape attempt",
		}},
	})
	if err != nil || !res.Success {
		t.Fatalf("path escape should be neutralized: err=%v res=%+v", err, res)
	}
	if _, rerr := os.Stat(filepath.Join(dir, "escape.md")); rerr == nil {
		t.Fatal("file escaped baseline dir")
	}
	if _, rerr := os.Stat(filepath.Join(dir, ".bma", "baseline", "escape.md")); rerr != nil {
		t.Fatalf("escaped name should land in baseline dir: %v", rerr)
	}

	// path 为空 → 拒收。
	res, _ = r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline_content": []any{map[string]any{
			"content": "no path",
		}},
	})
	if res.Success {
		t.Fatal("empty path should be rejected")
	}

	// content 为空 → 拒收。
	res, _ = r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline_content": []any{map[string]any{
			"path": "empty.md",
		}},
	})
	if res.Success {
		t.Fatal("empty content should be rejected")
	}
}

func TestWriteSpec_BaselineContentNeedsWorkDir(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 非文件后端（workDir 空）→ baseline_content 不可用，明确报错而非静默。
	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline_content": []any{map[string]any{
			"path":    "b.md",
			"content": "content",
		}},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Success {
		t.Fatal("baseline_content without workDir should fail")
	}
	if !strings.Contains(res.Error, "baseline_content") {
		t.Fatalf("error should mention baseline_content, got %q", res.Error)
	}
}

func TestWriteSpec_MissingBaselineErrorSelfDescribing(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	store.workDir = dir
	r.SetSharedMemory(store)
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "高还原度复刻水果忍者游戏",
		"acceptance": []any{"可玩"},
		"baseline":   []any{".bma/baseline/gone.md"},
	})
	if res.Success {
		t.Fatal("missing baseline should be rejected")
	}
	// 错误自描述：附解析后绝对路径 + 指引 baseline_content。
	if !strings.Contains(res.Error, filepath.Join(dir, ".bma", "baseline", "gone.md")) {
		t.Fatalf("error should contain resolved absolute path, got %q", res.Error)
	}
	if !strings.Contains(res.Error, "baseline_content") {
		t.Fatalf("error should point to baseline_content, got %q", res.Error)
	}
}
