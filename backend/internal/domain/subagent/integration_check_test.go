package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写临时项目树，返回 workdir。
func writeFixtureProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunIntegrationChecks_Pass(t *testing.T) {
	dir := writeFixtureProject(t, map[string]string{
		"index.html": `<script src="src/main.ts"></script>`,
		"src/main.ts": `import { Game } from "./game";
import "./style.css";`,
		"src/game.ts": `export class Game {}`,
		"src/style.css": `body {}`,
	})
	specFiles := []string{
		filepath.Join(dir, "index.html"),
		filepath.Join(dir, "src", "main.ts"),
		filepath.Join(dir, "src", "game.ts"),
	}
	rep := runIntegrationChecks(dir, specFiles)
	if failed := integrationFailed(rep); len(failed) > 0 {
		t.Fatalf("expected pass, got failures: %v", failed)
	}
}

func TestRunIntegrationChecks_BrokenScriptSrc(t *testing.T) {
	dir := writeFixtureProject(t, map[string]string{
		"index.html": `<script src="src/ghost.js"></script>`,
		"src/main.js": `console.log("hi");`,
	})
	specFiles := []string{filepath.Join(dir, "index.html"), filepath.Join(dir, "src", "main.js")}
	rep := runIntegrationChecks(dir, specFiles)
	if len(integrationFailed(rep)) == 0 {
		t.Fatal("expected broken script src violation")
	}
}

func TestRunIntegrationChecks_BrokenImport(t *testing.T) {
	dir := writeFixtureProject(t, map[string]string{
		"index.html": `<script type="module" src="src/entry.ts"></script>`,
		"src/entry.ts": `import { X } from "./missing";
export const X = 1;`,
	})
	specFiles := []string{filepath.Join(dir, "index.html"), filepath.Join(dir, "src", "entry.ts")}
	rep := runIntegrationChecks(dir, specFiles)
	if len(integrationFailed(rep)) == 0 {
		t.Fatal("expected broken import violation")
	}
}

func TestRunIntegrationChecks_EmptyShellWarning(t *testing.T) {
	// 实证 2026-08-24 塔防空壳形态：入口 stub 无任何 import，多文件项目。
	dir := writeFixtureProject(t, map[string]string{
		"index.html": `<script type="module" src="src/main.ts"></script>`,
		"src/main.ts": `console.log("empty shell");`,
		"src/game.ts": `export class Game {}`,
	})
	specFiles := []string{filepath.Join(dir, "index.html"), filepath.Join(dir, "src", "main.ts"), filepath.Join(dir, "src", "game.ts")}
	rep := runIntegrationChecks(dir, specFiles)
	if len(integrationFailed(rep)) > 0 {
		t.Fatalf("empty-shell should be warning not violation: %v", integrationFailed(rep))
	}
	joined := strings.Join(rep.entriesText(), "\n")
	if !strings.Contains(joined, "空壳") {
		t.Fatalf("expected empty-shell warning, got: %s", joined)
	}
}

func (r integrationReport) entriesText() []string {
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.detail
	}
	return out
}
