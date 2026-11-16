package mcpbridge

// stdio_workdir_test.go 覆盖"本地 stdio 插件按会话工作目录起独立进程"的行为。
//
// 背景：桌面包把 ui_preview（Playwright 截图）从容器改成随包 Node 本地跑后，
// 截图输出目录必须落在**会话自己的工作目录**——否则 Agent 在自己目录里找不到产物
// （历史上因此反复"排查同步"死循环）。这要求 args/env 里的 ${WORKDIR} 能按
// 会话目录重新展开（docker 路径靠 per-workdir 容器实现同一件事）。

import (
	"context"
	"reflect"
	"testing"

	"github.com/blockmemory/agent/backend/internal/plugins"
)

// TestBridgeStdioExpandsArgsPerWorkDir 验证 stdio 插件的 args/env 按目录展开。
func TestBridgeStdioExpandsArgsPerWorkDir(t *testing.T) {
	b := New("ui_preview", FromSettings(map[string]any{
		"transport": "stdio",
		"command":   "node",
		"args": []any{
			"${BMA_UI_PREVIEW_MCP}",
			"--output-dir", "${WORKDIR}/.bma/ui-artifacts",
		},
		"env": map[string]any{
			"PLAYWRIGHT_BROWSERS_PATH": "${BMA_PLAYWRIGHT_BROWSERS}",
			"OUT_DIR":                  "${WORKDIR}/out",
		},
	}), nil)
	if err := b.Init(context.Background(), plugins.Deps{WorkDir: `D:\data\proj`}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !b.perWorkDir {
		t.Fatal("args 里带 ${WORKDIR} 的 stdio 插件应标记 perWorkDir（按目录起进程）")
	}
	if !b.perWorkDirSession() {
		t.Fatal("stdio 传输应支持按目录分会话")
	}
	// 默认会话：占位符换成启动目录。
	gotArgs := b.expandArgs(`D:\data\proj`)
	wantArgs := []string{"${BMA_UI_PREVIEW_MCP}", "--output-dir", "D:/data/proj/.bma/ui-artifacts"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("expandArgs = %q, want %q", gotArgs, wantArgs)
	}
	gotEnv := b.expandEnv(`D:\data\proj`)
	if gotEnv["OUT_DIR"] != "D:/data/proj/out" {
		t.Fatalf("env OUT_DIR = %q", gotEnv["OUT_DIR"])
	}
	if gotEnv["PLAYWRIGHT_BROWSERS_PATH"] != "${BMA_PLAYWRIGHT_BROWSERS}" {
		t.Fatalf("非占位符 env 被误改: %q", gotEnv["PLAYWRIGHT_BROWSERS_PATH"])
	}
	// 异目录会话：占位符换成该目录（envRaw 必须留的是**未展开**的模板）。
	gotArgs2 := b.expandArgs(`D:\data\other`)
	if gotArgs2[2] != "D:/data/other/.bma/ui-artifacts" {
		t.Fatalf("异目录 args = %q", gotArgs2[2])
	}
	if e := b.expandEnv(`D:\data\other`); e["OUT_DIR"] != "D:/data/other/out" {
		t.Fatalf("异目录 env = %q", e["OUT_DIR"])
	}
}

// TestBridgeDockerPerWorkDirSessionGate 验证 docker 只有 shared 模式才按目录分会话。
func TestBridgeDockerPerWorkDirSessionGate(t *testing.T) {
	b := New("ui_preview", Settings{
		Transport:   "docker",
		Image:       "img:tag",
		Shared:      true,
		ExecCommand: []string{"node", "cli.js"},
		Volumes:     []string{"${WORKDIR}:/workspace"},
	}, nil)
	if err := b.Init(context.Background(), plugins.Deps{WorkDir: `D:\data\proj`}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !b.perWorkDirSession() {
		t.Fatal("shared docker 应支持按目录分会话")
	}
	legacy := New("x", Settings{Transport: "docker", Image: "img:tag", Volumes: []string{"${WORKDIR}:/w"}}, nil)
	if err := legacy.Init(context.Background(), plugins.Deps{WorkDir: `D:\data\proj`}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if legacy.perWorkDirSession() {
		t.Fatal("legacy docker（每次调用起容器）不应按目录分会话")
	}
}
