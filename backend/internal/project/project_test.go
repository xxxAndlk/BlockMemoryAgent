package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestProject 在临时目录构造一个最小多语言项目骨架供扫描测试。
func writeTestProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mustWrite("go.mod", "module github.com/example/demo\n\ngo 1.25\n")
	mustWrite("Makefile", "run:\n\tgo run ./cmd\n\ntest:\n\tgo test ./...\n\nVAR := x\n")
	mustWrite("README.md", "# demo\n")
	mustWrite("CLAUDE.md", "# guide\n")
	mustWrite("backend/cmd/main.go", "package main\nfunc main() {}\n")
	mustWrite("backend/internal/foo/foo.go", "package foo\n")
	mustWrite("doc/design.md", "# design\n")
	mustWrite("node_modules/skip.js", "// skip\n") // 应被跳过
	mustWrite(".git/config", "[core]\n")            // 应被跳过
	return root
}

func TestEnsureProjectDoc_GeneratesAndIdempotent(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := ProjectDocPath(root)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, ManagedBegin) || !strings.Contains(s, ManagedEnd) {
		t.Fatalf("generated file missing managed markers")
	}
	if !strings.Contains(s, "github.com/example/demo") {
		t.Fatalf("generated file missing module name")
	}
	if !strings.Contains(s, "Go 版本: 1.25") {
		t.Fatalf("generated file missing go version")
	}
	if !strings.Contains(s, "`make run`") {
		t.Fatalf("generated file missing make target run")
	}
	if !strings.Contains(s, "### `backend/` - 后端服务") {
		t.Fatalf("generated file missing backend domain entry")
	}
	if !strings.Contains(s, "## 文档地图") || !strings.Contains(s, "doc/design.md") {
		t.Fatalf("generated file missing doc map")
	}
	if strings.Contains(s, "node_modules") {
		t.Fatalf("node_modules should be skipped but appeared in output")
	}

	// 二次调用幂等：不覆盖。
	before, _ := os.ReadFile(p)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatalf("Ensure not idempotent: file changed on second call")
	}
}

func TestLoadProjectDoc_ReturnsManagedBody(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	body := LoadProjectDoc(root)
	if body == "" {
		t.Fatal("Load returned empty for existing doc")
	}
	if strings.Contains(body, ManagedBegin) || strings.Contains(body, ManagedEnd) {
		t.Fatalf("Load should return body without markers")
	}
	if !strings.Contains(body, "项目概览") {
		t.Fatalf("Load body missing title")
	}
}

func TestLoadProjectDoc_EmptyWhenMissing(t *testing.T) {
	root := t.TempDir()
	if body := LoadProjectDoc(root); body != "" {
		t.Fatalf("expected empty load for missing doc, got %q", body)
	}
}

// TestProjectDocStaleRefs 概览过期引用检测（2026-10-13）：外部删除被引用目录后
// 注入侧要能报告（"概览过期提醒"）并驱动会话启动侧异步重刷新。
func TestProjectDocStaleRefs(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// 文档刚生成：引用与磁盘一致，无过期引用。
	if got := ProjectDocStaleRefs(root); len(got) != 0 {
		t.Fatalf("fresh doc should have no stale refs, got %v", got)
	}

	// 外部删除一个被引用的顶层目录（不经工具层刷新器）→ 该目录应被报告（去重一条，
	// 目录行与文件行都指向同一顶层段）。
	if err := os.RemoveAll(filepath.Join(root, "backend")); err != nil {
		t.Fatalf("remove backend: %v", err)
	}
	got := ProjectDocStaleRefs(root)
	if len(got) != 1 || got[0] != "backend" {
		t.Fatalf("expected stale ref [backend], got %v", got)
	}

	// Refresh 后引用按磁盘重建，不再报过期。
	if err := RefreshProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := ProjectDocStaleRefs(root); len(got) != 0 {
		t.Fatalf("after refresh stale refs should clear, got %v", got)
	}
}

func TestProjectDocStaleRefs_MissingDoc(t *testing.T) {
	root := t.TempDir()
	if got := ProjectDocStaleRefs(root); got != nil {
		t.Fatalf("missing doc must return nil, got %v", got)
	}
}

func TestRefreshProjectDoc_PreservesHumanEdits(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := ProjectDocPath(root)
	// 追加人手补充到标记区外。
	orig, _ := os.ReadFile(p)
	humanNote := "\n## 人手补充\n\n这是 Agent 不会覆盖的备注。\n"
	if err := os.WriteFile(p, append([]byte(string(orig)), []byte(humanNote)...), 0o644); err != nil {
		t.Fatalf("append human note: %v", err)
	}
	// 新增一个顶层目录，验证 Refresh 重写 managed 区能反映结构变化。
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "build.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write build.sh: %v", err)
	}

	if err := RefreshProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	refreshed, _ := os.ReadFile(p)
	s := string(refreshed)
	if !strings.Contains(s, "人手补充") || !strings.Contains(s, "这是 Agent 不会覆盖的备注。") {
		t.Fatalf("Refresh lost human note outside managed region")
	}
	if !strings.Contains(s, "### `scripts/` - 脚本") {
		t.Fatalf("Refresh did not pick up new scripts/ domain")
	}
	// 标记区应仍只出现一次。
	if c := strings.Count(s, ManagedBegin); c != 1 {
		t.Fatalf("expected 1 managed begin marker, got %d", c)
	}
}

func TestRefreshProjectDoc_CreatesWhenMissing(t *testing.T) {
	root := writeTestProject(t)
	// 不先 Ensure，直接 Refresh 应等价于生成。
	if err := RefreshProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Refresh on missing: %v", err)
	}
	if _, err := os.ReadFile(ProjectDocPath(root)); err != nil {
		t.Fatalf("Refresh did not create file: %v", err)
	}
}

func TestRefreshProjectDoc_NoMarkersPrepends(t *testing.T) {
	root := writeTestProject(t)
	p := ProjectDocPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	humanOnly := "# 我手写的 PROJECT\n\n纯人手内容，无标记。\n"
	if err := os.WriteFile(p, []byte(humanOnly), 0o644); err != nil {
		t.Fatalf("write human-only: %v", err)
	}
	if err := RefreshProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s, _ := os.ReadFile(p)
	body := string(s)
	if !strings.Contains(body, ManagedBegin) {
		t.Fatalf("Refresh should insert managed block into markerless file")
	}
	if !strings.Contains(body, "纯人手内容，无标记。") {
		t.Fatalf("Refresh should preserve existing human content when no markers")
	}
	// managed 区在前，人手内容在后。
	if strings.Index(body, ManagedEnd) > strings.Index(body, "纯人手内容") {
		t.Fatalf("managed block should be prepended before existing human content")
	}
}

// fakeClassifier 注入假 LLM 分区结果，供 DomainClassifier 路径测试。
type fakeClassifier struct {
	parts     []DomainPartition
	called    bool
	callCount int
}

func (f *fakeClassifier) Partition(ctx context.Context, root string, files []string) ([]DomainPartition, error) {
	f.called = true
	f.callCount++
	return f.parts, nil
}

// writeWebFixture 构造一个类 tower-defense 的最小 Web 项目：index.html 用 <script src>
// 聚合 js/a.js + js/b.js，<link> 聚合 css/s.css。4 文件应聚成一个依赖簇。
func writeWebFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	must("index.html", `<!DOCTYPE html><html><head>
<link rel="stylesheet" href="css/s.css">
</head><body>
<script src="js/a.js"></script>
<script src="js/b.js"></script>
</body></html>`)
	must("js/a.js", "window.A = {};\n")
	must("js/b.js", "window.B = {};\n")
	must("css/s.css", "body{margin:0}\n")
	return root
}

func TestScanDomains_DependencyGraphMergesCluster(t *testing.T) {
	root := writeWebFixture(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	s, _ := os.ReadFile(ProjectDocPath(root))
	body := string(s)
	// 4 文件应聚成一个簇（影响文件 4），而非切成 js/ css/ 多个领域。
	if !strings.Contains(body, "- 影响文件 (4):") {
		t.Fatalf("expected one merged cluster of 4 files, got:\n%s", body)
	}
	if strings.Contains(body, "待人工标注") {
		t.Fatalf("blank label leaked:\n%s", body)
	}
	// 只应有一个领域标题（### `）。index.html 是入口锚点。
	if c := strings.Count(body, "### `"); c != 1 {
		t.Fatalf("expected 1 domain header, got %d:\n%s", c, body)
	}
	if !strings.Contains(body, "index.html") {
		t.Fatalf("expected index.html anchor in body:\n%s", body)
	}
	// 轮廓注入：js 文件后应括注行数与顶层符号（window.A L1），css 不注。
	if !strings.Contains(body, "js/a.js (1 行): window.A L1") {
		t.Fatalf("expected outline suffix for js/a.js, got:\n%s", body)
	}
	if strings.Contains(body, "css/s.css (") {
		t.Fatalf("css should not carry outline suffix, got:\n%s", body)
	}
}

// TestFileOutline 直接验证符号轮廓抽取：JS 顶层符号 + Go func/type + 行数。
func TestFileOutline(t *testing.T) {
	root := t.TempDir()
	js := `// tower.js 注释行
import { cfg } from './config.js';
const CONFIG = { towers: {} };
class Tower {
	fire() {}
}
export function spawnTower() {}
window.TowerGlobal = Tower;
`
	if err := os.WriteFile(filepath.Join(root, "tower.js"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fileOutline(root, "tower.js")
	for _, want := range []string{"(8 行)", "CONFIG L3", "class Tower L4", "spawnTower() L7", "window.TowerGlobal L8"} {
		if !strings.Contains(got, want) {
			t.Fatalf("outline %q missing %q", got, want)
		}
	}
	// 类方法 fire 有缩进，不应出现在顶层轮廓。
	if strings.Contains(got, "fire(") {
		t.Fatalf("indented method should be skipped, got %q", got)
	}

	go_ := "package x\n\ntype Server struct{}\n\nfunc NewServer() *Server { return nil }\n\nfunc (s *Server) Start() {}\n"
	if err := os.WriteFile(filepath.Join(root, "s.go"), []byte(go_), 0o644); err != nil {
		t.Fatal(err)
	}
	got = fileOutline(root, "s.go")
	for _, want := range []string{"type Server L3", "NewServer() L5", "Start() L7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("go outline %q missing %q", got, want)
		}
	}

	// 非轮廓扩展名返回空串。
	if got := fileOutline(root, "s.css"); got != "" {
		t.Fatalf("css should have no outline, got %q", got)
	}
}

func TestDomainClassifier_LLMNaming(t *testing.T) {
	root := writeWebFixture(t)
	cls := &fakeClassifier{parts: []DomainPartition{{Name: "游戏运行时", Purpose: "塔防游戏运行时主控", Files: []string{"index.html", "js/a.js", "js/b.js", "css/s.css"}}}}
	if err := RefreshProjectDoc(context.Background(), root, cls); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s, _ := os.ReadFile(ProjectDocPath(root))
	body := string(s)
	if !strings.Contains(body, "游戏运行时") {
		t.Fatalf("LLM semantic name missing:\n%s", body)
	}
	if !strings.Contains(body, "塔防游戏运行时主控") {
		t.Fatalf("LLM purpose missing:\n%s", body)
	}
	if strings.Contains(body, "待人工标注") {
		t.Fatalf("blank label leaked:\n%s", body)
	}
}

func TestDomainClassifier_NilFallbackNoBlank(t *testing.T) {
	root := writeWebFixture(t)
	if err := RefreshProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s, _ := os.ReadFile(ProjectDocPath(root))
	body := string(s)
	if strings.Contains(body, "待人工标注") {
		t.Fatalf("nil classifier must not produce blank labels:\n%s", body)
	}
	// 兜底应有锚点命名（index.html）。
	if !strings.Contains(body, "index.html") {
		t.Fatalf("heuristic anchor missing:\n%s", body)
	}
}

// writeTowerFixture 构造一个类 tower-defense 的最小项目：index.html 聚合 4 个 js + css。
// js 文件零 import 边（纯 browser global），静态依赖图会把全图并成一簇；
// 用 fakeClassifier 模拟 LLM 按职责拆成多域（游戏运行时/炮塔实体/怪物实体/配置/渲染入口）。
func writeTowerFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	must("index.html", `<!DOCTYPE html><html><head>
<link rel="stylesheet" href="css/style.css">
</head><body>
<script src="js/config.js"></script>
<script src="js/monster.js"></script>
<script src="js/tower.js"></script>
<script src="js/game.js"></script>
</body></html>`)
	must("js/config.js", "window.Config = {};\n")
	must("js/game.js", "window.Game = {};\n")
	must("js/tower.js", "window.Tower = {};\n")
	must("js/monster.js", "window.Monster = {};\n")
	must("css/style.css", "body{margin:0}\n")
	return root
}

func TestDomainClassifier_PartitionByResponsibility(t *testing.T) {
	root := writeTowerFixture(t)
	cls := &fakeClassifier{parts: []DomainPartition{
		{Name: "游戏运行时", Purpose: "主循环与调度", Files: []string{"js/game.js"}},
		{Name: "炮塔实体", Purpose: "炮塔逻辑", Files: []string{"js/tower.js"}},
		{Name: "怪物实体", Purpose: "怪物逻辑", Files: []string{"js/monster.js"}},
		{Name: "配置", Purpose: "全局配置数据", Files: []string{"js/config.js"}},
		{Name: "渲染入口", Purpose: "HTML 入口与样式", Files: []string{"index.html", "css/style.css"}},
	}}
	if err := RefreshProjectDoc(context.Background(), root, cls); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	s, _ := os.ReadFile(ProjectDocPath(root))
	body := string(s)
	// 5 个领域标题。
	if c := strings.Count(body, "### `"); c != 5 {
		t.Fatalf("expected 5 domain headers, got %d:\n%s", c, body)
	}
	for _, want := range []string{"游戏运行时", "炮塔实体", "怪物实体", "配置", "渲染入口"} {
		if !strings.Contains(body, "### `"+want+"`") {
			t.Fatalf("missing domain %q:\n%s", want, body)
		}
	}
	// 各域列影响文件。
	if !strings.Contains(body, "- 影响文件 (1):\n  - js/game.js") {
		t.Fatalf("missing game.js file listing:\n%s", body)
	}
	if !strings.Contains(body, "  - js/tower.js") {
		t.Fatalf("missing tower.js file listing:\n%s", body)
	}
	if !strings.Contains(body, "- 影响文件 (2):") {
		t.Fatalf("missing 2-file listing for 渲染入口:\n%s", body)
	}
	if strings.Contains(body, "待人工标注") {
		t.Fatalf("blank label leaked:\n%s", body)
	}
}

func TestEnsureProjectDoc_LLMClassifierUsed(t *testing.T) {
	root := writeTowerFixture(t)
	cls := &fakeClassifier{parts: []DomainPartition{
		{Name: "游戏运行时", Purpose: "主循环", Files: []string{"js/game.js"}},
	}}
	if err := EnsureProjectDoc(context.Background(), root, cls); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !cls.called {
		t.Fatalf("EnsureProjectDoc with non-nil cls must call cls.Partition")
	}
	s, _ := os.ReadFile(ProjectDocPath(root))
	body := string(s)
	// LLM 覆盖 js/game.js -> 游戏运行时；其余文件走 clusterByDeps 兜底（不丢文件）。
	if !strings.Contains(body, "游戏运行时") {
		t.Fatalf("LLM partition name missing:\n%s", body)
	}
	if strings.Contains(body, "待人工标注") {
		t.Fatalf("blank label leaked:\n%s", body)
	}
}

// TestEnsureProjectDoc_RefreshesOnAssetChange 验证资产指纹变化会刷新 managed 区
// （2026-10-22 事故：项目新增 19 个页面/图集后概览仍是空壳，模型看不到可复用资产），
// 同时保留标记区外的人手补充。
func TestEnsureProjectDoc_RefreshesOnAssetChange(t *testing.T) {
	root := writeTestProject(t)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := ProjectDocPath(root)
	// 人手补充写在 managed 区之外。
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("\n## 人手补充\n- 保留我\n"); err != nil {
		t.Fatalf("append human note: %v", err)
	}
	f.Close()

	// 新增资产：图片/视频/HTML。
	for _, name := range []string{"public/assets/img/run/frames16/a.png", "public/assets/video/x.mp4", "public/phaser-run.html"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure after assets: %v", err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	if !strings.Contains(s, "## 已有资产") {
		t.Fatalf("概览应包含资产清单:\n%s", s)
	}
	if !strings.Contains(s, "public/phaser-run.html") {
		t.Fatalf("资产清单应列出 HTML 交付物:\n%s", s)
	}
	if !strings.Contains(s, "保留我") {
		t.Fatalf("刷新不得覆盖标记区外的人手补充:\n%s", s)
	}

	// 指纹已更新：再次调用不应再改文件。
	before, _ := os.ReadFile(p)
	if err := EnsureProjectDoc(context.Background(), root, nil); err != nil {
		t.Fatalf("Ensure third: %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("资产指纹未变化时 Ensure 不应重写文件")
	}
}

// TestScanAssetAssets_HiddenDirAndProducer 回归 2026-10-22 交付事故：
// 素材目录 `hidden` 里的整条帧序列此前被"隐藏目录一律跳过"过滤掉；且清单不写来路，
// 模型把 `tools/rig/bake.mjs` 用骨架烘焙出的帧当成合规的"连续 16 帧"照单全收。
// 本测试要求：隐藏素材目录进清单，且能反查到生成脚本。
func TestScanAssetAssets_HiddenDirAndProducer(t *testing.T) {
	root := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// 隐藏素材目录：16 帧 + 动作元数据。
	for i := 0; i < 4; i++ {
		write(filepath.ToSlash(filepath.Join(".workbuddy", "frames", "anim", "run", "f000"+string(rune('0'+i))+".png")), "x")
	}
	write(".workbuddy/frames/anim/_meta.json", "{}")
	// 生成脚本：正文引用父目录（真实脚本会把 state 子目录拼出来）。
	write("tools/rig/bake.mjs", "// 逐帧烘焙：骨架 IK 摆姿势后逐帧截图\nconst out = '.workbuddy/frames/anim';\n")
	// 噪音隐藏目录：不得计入。
	write(".git/objects/x.png", "x")

	inv := scanAssetAssets(root)
	if inv.images != 4 {
		t.Fatalf("应只统计隐藏素材目录里的 4 张图（.git 不计），got %d", inv.images)
	}
	section := renderAssetSection(inv)
	if !strings.Contains(section, ".workbuddy/frames/anim/run/") {
		t.Fatalf("素材目录应包含隐藏目录 .workbuddy/frames/anim/run:\n%s", section)
	}
	if !strings.Contains(section, "生成脚本: tools/rig/bake.mjs") {
		t.Fatalf("应反查到生成脚本 tools/rig/bake.mjs:\n%s", section)
	}
	if !strings.Contains(section, "明确排除") {
		t.Fatalf("生成脚本存在时应附路线核对提醒:\n%s", section)
	}
	if strings.Contains(section, ".git/") {
		t.Fatalf("噪音隐藏目录 .git 不应出现:\n%s", section)
	}
}
