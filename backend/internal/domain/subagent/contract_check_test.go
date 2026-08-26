package subagent

// contract_check_test.go 覆盖跨域契约静态校验（TODO #57）：
// 符号缺失/DOM id 未声明/script 顺序颠倒/签名缺失/契约文件缺失五类违例检出、
// 契约为空跳过、违例按文件归属分组、全完成触发打回集成链路。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/go-kratos/blades"
)

// newContractCheckEnv 写契约涉及文件（index.html/engine.js/main.js）并构造 Dispatcher。
func newContractCheckEnv(t *testing.T, html, engine, main string) (*Dispatcher, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "engine.js"), []byte(engine), 0644); err != nil {
		t.Fatalf("write engine.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.js"), []byte(main), 0644); err != nil {
		t.Fatalf("write main.js: %v", err)
	}
	d := &Dispatcher{tools: tool.NewBuiltinRegistry(dir, nil, nil)}
	return d, dir
}

// fullContract 构造覆盖四类条目的全量契约。
func fullContract() *tool.Contract {
	return &tool.Contract{
		Symbols: []tool.ContractSymbol{
			{Symbol: "GameEngine.init", File: "engine.js", Refs: []string{"main.js"}},
		},
		DOMIDs: []tool.ContractDOMID{
			{ID: "game-canvas", File: "index.html"},
		},
		Scripts: []tool.ContractScript{
			{File: "engine.js"},
			{File: "main.js"},
		},
		Signatures: []tool.ContractSignature{
			{Symbol: "start", Signature: "function start()", File: "engine.js"},
		},
	}
}

const (
	fixtureHTML = `<html><head>
<script src="engine.js"></script>
<script src="main.js"></script>
</head><body><canvas id="game-canvas"></canvas></body></html>`
	fixtureEngine = "var GameEngine = { init: function() {} };\nfunction start() {}"
	fixtureMain   = "GameEngine.init();"
)

// TestContractCheckAllPass 契约全过：无违例，条目含通过记录。
func TestContractCheckAllPass(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, fixtureEngine, fixtureMain)
	rep := d.runContractChecks(fullContract(), []string{"index.html", "engine.js", "main.js"})
	if !rep.pass() {
		t.Fatalf("expected pass, violations: %+v", rep.violations)
	}
	if len(rep.entries) < 4 {
		t.Fatalf("expected entries for all four check kinds, got %d: %v", len(rep.entries), rep.entries)
	}
	text := contractReportText(rep)
	if !strings.Contains(text, "【机器校验】") || !strings.Contains(text, "非 agent 自述") {
		t.Fatalf("report missing machine-check annotation: %s", text)
	}
}

// TestContractCheckSymbolMissing 符号未在声明文件找到 → 违例归属声明文件。
func TestContractCheckSymbolMissing(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, "var other = 1;", fixtureMain)
	c := &tool.Contract{Symbols: []tool.ContractSymbol{
		{Symbol: "GameEngine.init", File: "engine.js", Refs: []string{"main.js"}},
	}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 1 {
		t.Fatalf("expected 1 violation on decl file, got %+v", rep.violations)
	}
	if rep.violations[0].file != "engine.js" {
		t.Fatalf("violation should attribute to engine.js, got %q", rep.violations[0].file)
	}
}

// TestContractCheckRefMissing 声明存在但引用方未引用 → 违例归属引用方文件。
func TestContractCheckRefMissing(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, fixtureEngine, "var other = 1;")
	c := &tool.Contract{Symbols: []tool.ContractSymbol{
		{Symbol: "GameEngine.init", File: "engine.js", Refs: []string{"main.js"}},
	}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 1 {
		t.Fatalf("expected 1 violation on ref file, got %+v", rep.violations)
	}
	if rep.violations[0].file != "main.js" {
		t.Fatalf("violation should attribute to main.js, got %q", rep.violations[0].file)
	}
}

// TestContractCheckDOMIDMissing DOM id 未声明 → 违例。
func TestContractCheckDOMIDMissing(t *testing.T) {
	d, _ := newContractCheckEnv(t, "<html></html>", fixtureEngine, fixtureMain)
	c := &tool.Contract{DOMIDs: []tool.ContractDOMID{{ID: "game-canvas", File: "index.html"}}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 1 || rep.violations[0].file != "index.html" {
		t.Fatalf("expected 1 violation on index.html, got %+v", rep.violations)
	}
}

// TestContractCheckScriptOrderReversed script 顺序颠倒 → 违例归属 HTML 文件。
func TestContractCheckScriptOrderReversed(t *testing.T) {
	reversed := `<html><head>
<script src="main.js"></script>
<script src="engine.js"></script>
</head></html>`
	d, _ := newContractCheckEnv(t, reversed, fixtureEngine, fixtureMain)
	c := &tool.Contract{Scripts: []tool.ContractScript{{File: "engine.js"}, {File: "main.js"}}}
	rep := d.runContractChecks(c, []string{"index.html"})
	if rep.pass() || len(rep.violations) != 1 {
		t.Fatalf("expected 1 order violation, got %+v", rep.violations)
	}
	if rep.violations[0].file != "index.html" || !strings.Contains(rep.violations[0].detail, "顺序") {
		t.Fatalf("violation should attribute to index.html with order detail, got %+v", rep.violations[0])
	}
}

// TestContractCheckSignatureMissing 签名文本缺失 → 违例。
func TestContractCheckSignatureMissing(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, "var x = 1;", fixtureMain)
	c := &tool.Contract{Signatures: []tool.ContractSignature{
		{Symbol: "start", Signature: "function start()", File: "engine.js"},
	}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 1 || rep.violations[0].file != "engine.js" {
		t.Fatalf("expected 1 signature violation on engine.js, got %+v", rep.violations)
	}
}

// TestContractCheckFileMissing 契约涉及文件缺失 → 违例归属该文件。
func TestContractCheckFileMissing(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, fixtureEngine, fixtureMain)
	c := &tool.Contract{DOMIDs: []tool.ContractDOMID{{ID: "x", File: "missing.html"}}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 1 || rep.violations[0].file != "missing.html" {
		t.Fatalf("expected missing-file violation, got %+v", rep.violations)
	}
}

// TestContractCheckEmptyContract 空契约跳过：无条目、无违例。
func TestContractCheckEmptyContract(t *testing.T) {
	d, _ := newContractCheckEnv(t, fixtureHTML, fixtureEngine, fixtureMain)
	rep := d.runContractChecks(&tool.Contract{}, nil)
	if !rep.pass() || len(rep.entries) != 0 {
		t.Fatalf("empty contract should pass with no entries, got %+v", rep)
	}
}

// TestContractCheckViolationGrouping 多违例按文件分组（一次消息列全部违例）。
func TestContractCheckViolationGrouping(t *testing.T) {
	d, _ := newContractCheckEnv(t, "<html></html>", "var other = 1;", "var other2 = 1;")
	c := &tool.Contract{
		Symbols: []tool.ContractSymbol{{Symbol: "GameEngine.init", File: "engine.js", Refs: []string{"main.js"}}},
		DOMIDs:  []tool.ContractDOMID{{ID: "game-canvas", File: "index.html"}},
	}
	rep := d.runContractChecks(c, nil)
	if rep.pass() || len(rep.violations) != 2 {
		t.Fatalf("expected 2 violations, got %+v", rep.violations)
	}
	text := contractViolationText(rep)
	for _, f := range []string{"engine.js", "index.html"} {
		if !strings.Contains(text, f) {
			t.Fatalf("grouped text missing file %q: %s", f, text)
		}
	}
}

// TestDispatcher_ContractCheckOnAllChildrenDone 集成链路：兄弟域全完成（pending 归零）
// 触发契约检查；违例以 contract_violation 打回父 Agent，通过以【机器校验】段通知。
func TestDispatcher_ContractCheckOnAllChildrenDone(t *testing.T) {
	// 违例分支：main.js 未引用契约符号。
	t.Run("violation", func(t *testing.T) {
		provider := &scriptedMsgProvider{msgs: []*blades.Message{blades.AssistantMessage("done")}}
		d, mb, toolsReg, dir := newSmokeTestEnv(t, provider, nil)
		for name, content := range map[string]string{
			"index.html": fixtureHTML,
			"engine.js":  fixtureEngine,
			"main.js":    "var other = 1;",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		ctx := agent.WithAgentID(context.Background(), "meta")
		res, err := toolsReg.Dispatch(ctx, "WriteSpec", map[string]any{
			"goal":       "契约集成测试",
			"acceptance": []any{"跨域集成点一致"},
			"files":      []any{"index.html", "engine.js", "main.js"},
			"contract": map[string]any{
				"symbols": []any{map[string]any{
					"symbol": "GameEngine.init",
					"file":   "engine.js",
					"refs":   []any{"main.js"},
				}},
			},
		})
		if err != nil || !res.Success {
			t.Fatalf("WriteSpec: err=%v res=%+v", err, res)
		}
		if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
			"role_id": "code_assistant", "task": "占位", "verify_kind": "none",
		}); err != nil {
			t.Fatalf("dispatch: %v", err)
		}

		deadline := time.Now().Add(5 * time.Second)
		var body string
		for time.Now().Before(deadline) {
			for _, m := range mb.Drain("meta") {
				if strings.Contains(m.Body, "跨域契约") {
					body = m.Body
				}
			}
			if body != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if body == "" {
			t.Fatal("parent mailbox got no contract check message")
		}
		if !strings.Contains(body, "[failure kind=contract_violation retryable=false]") {
			t.Fatalf("expected contract_violation marker, got: %q", body)
		}
		if !strings.Contains(body, "main.js") || !strings.Contains(body, "GameEngine.init") {
			t.Fatalf("violation should attribute to main.js with symbol, got: %q", body)
		}
		_ = d
	})

	// 通过分支：契约全过 → 【机器校验】段通知（无 failure marker）。
	t.Run("pass", func(t *testing.T) {
		provider := &scriptedMsgProvider{msgs: []*blades.Message{blades.AssistantMessage("done")}}
		_, mb, toolsReg, dir := newSmokeTestEnv(t, provider, nil)
		for name, content := range map[string]string{
			"index.html": fixtureHTML,
			"engine.js":  fixtureEngine,
			"main.js":    fixtureMain,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		ctx := agent.WithAgentID(context.Background(), "meta")
		if _, err := toolsReg.Dispatch(ctx, "WriteSpec", map[string]any{
			"goal":       "契约集成测试",
			"acceptance": []any{"跨域集成点一致"},
			"contract": map[string]any{
				"symbols": []any{map[string]any{
					"symbol": "GameEngine.init",
					"file":   "engine.js",
					"refs":   []any{"main.js"},
				}},
				"dom_ids": []any{map[string]any{"id": "game-canvas", "file": "index.html"}},
			},
		}); err != nil {
			t.Fatalf("WriteSpec: %v", err)
		}
		if _, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
			"role_id": "code_assistant", "task": "占位", "verify_kind": "none",
		}); err != nil {
			t.Fatalf("dispatch: %v", err)
		}

		deadline := time.Now().Add(5 * time.Second)
		var body string
		for time.Now().Before(deadline) {
			for _, m := range mb.Drain("meta") {
				if strings.Contains(m.Body, "跨域契约") {
					body = m.Body
				}
			}
			if body != "" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if body == "" {
			t.Fatal("parent mailbox got no contract check message")
		}
		if strings.Contains(body, "contract_violation") {
			t.Fatalf("pass branch must not contain violation marker: %q", body)
		}
		if !strings.Contains(body, "【机器校验】") || !strings.Contains(body, "通过") {
			t.Fatalf("expected machine-check pass section, got: %q", body)
		}
	})
}

// TestSymbolFound_CaseInsensitivePropertyPath 契约大写符号命中属性访问路径（TODO #62）：
// `Assets` 应命中 `this.game.assets.getImage()`（实证 2026-08-24 塔防误报场景）。
func TestSymbolFound_CaseInsensitivePropertyPath(t *testing.T) {
	content := `const g = new Game();
g.assets.getImage('plant-peashooter');`
	if !symbolFound(content, "Assets") {
		t.Fatal("Assets should match this.assets property access case-insensitively")
	}
	// 非末段大小写仍敏感：GameEngine 与 gameengine 是不同符号。
	if symbolFound(content, "Assets.getImage") {
		t.Fatal("composite with wrong mid-case should not match")
	}
	if !symbolFound(content, "g.assets.getImage") {
		t.Fatal("composite with lowercase property should match")
	}
}

// TestSignatureMatched_WhitespaceInsensitive 签名空白归一匹配（TODO #62）：
// `attack (target, dmg)` 与 `attack(target, dmg)` 等价。
func TestSignatureMatched_WhitespaceInsensitive(t *testing.T) {
	if !signatureMatched("function attack(target, dmg) {}", "attack (target, dmg)") {
		t.Fatal("whitespace-insensitive signature match should pass")
	}
	if signatureMatched("function attack() {}", "attack(target, dmg)") {
		t.Fatal("different parameter lists should not match")
	}
}

// TestStubOrphanDetected 占位桩责任挂名（TODO #61）：Stub 标记 + 声明文件仍含占位
// 注释 → 孤儿桩违例；实装（占位注释移除）→ 通过。
func TestStubOrphanDetected(t *testing.T) {
	stubDecl := "// 占位：完整实现由其他领域负责\nclass Assets { static get() { return null; } }"
	d, dir := newContractCheckEnv(t, fixtureHTML, fixtureEngine, stubDecl)
	_ = dir
	c := &tool.Contract{Symbols: []tool.ContractSymbol{
		{Symbol: "Assets", File: "main.js", Stub: true, Owner: "美术资产领域"},
	}}
	rep := d.runContractChecks(c, nil)
	if rep.pass() {
		t.Fatalf("stub with placeholder markers should violate, got %+v", rep.entries)
	}
	if !strings.Contains(rep.violations[0].detail, "美术资产领域") {
		t.Fatalf("stub violation should name owner, got %q", rep.violations[0].detail)
	}

	implemented := "class Assets { static getImage(n) { return imgCache[n]; } }"
	d2, _ := newContractCheckEnv(t, fixtureHTML, fixtureEngine, implemented)
	rep2 := d2.runContractChecks(c, nil)
	if !rep2.pass() {
		t.Fatalf("implemented stub (markers removed) should pass, got %+v", rep2.violations)
	}
}
