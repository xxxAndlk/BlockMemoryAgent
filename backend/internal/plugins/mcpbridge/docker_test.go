package mcpbridge

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/plugins"
)

func TestDockerContainerName(t *testing.T) {
	pidSuffix := fmt.Sprintf("-%d", os.Getpid())
	cases := map[string]string{
		"web_search":        "bma-plugin-web_search",
		"bundle/dir/server": "bma-plugin-bundle-dir-server",
		"deep_research@v2":  "bma-plugin-deep_research-v2",
		"UPPER.lower_1-2":   "bma-plugin-UPPER.lower_1-2",
	}
	for id, wantPrefix := range cases {
		got := dockerContainerName(id)
		if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, pidSuffix) {
			t.Errorf("dockerContainerName(%q) = %q, want 前缀 %q + pid 后缀 %q", id, got, wantPrefix, pidSuffix)
		}
	}
}

func TestDockerRunArgs(t *testing.T) {
	s := Settings{
		Transport: "docker",
		Image:     "bma/firecrawl-mcp:local",
		Args:      []string{"--verbose"},
		Ports:     []string{"6081:6081"},
		Volumes:   []string{"D:/data/workspace/od-artifacts:/out"},
		Env: map[string]string{
			"FIRECRAWL_API_URL": "http://host.docker.internal:3002",
			"FIRECRAWL_API_KEY": "self-hosted",
		},
	}
	name, args := dockerRunArgs("web_search", s)
	if !strings.HasPrefix(name, "bma-plugin-web_search-") {
		t.Fatalf("name = %q", name)
	}
	want := []string{
		"run", "-i", "--rm", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", "6081:6081",
		"-v", "D:/data/workspace/od-artifacts:/out",
		"-e", "FIRECRAWL_API_KEY=self-hosted",
		"-e", "FIRECRAWL_API_URL=http://host.docker.internal:3002",
		"bma/firecrawl-mcp:local", "--verbose",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}

func TestDockerSharedContainerName(t *testing.T) {
	// 全局确定性：同 id 恒同名（跨 workdir/跨实例共用一套容器），无任何后缀。
	a := dockerSharedContainerName("web_search")
	if a != "bma-plugin-web_search" {
		t.Fatalf("容器名错误: %q", a)
	}
	// id 净化与 legacy 同规则。
	if got := dockerSharedContainerName("bundle/dir/server"); strings.Contains(got, "/") {
		t.Fatalf("容器名未净化: %q", got)
	}
	_ = a
}

func TestParseDockerMountSources(t *testing.T) {
	out := "D:\\data\\proj|/workspace\n/var/lib/docker/volumes/x/_data|/app/.od\n"
	got := parseDockerMountSources(out)
	want := []string{`D:\data\proj`, "/var/lib/docker/volumes/x/_data"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %q, want %q", got, want)
	}
	if normalizeMountPath(`D:\Data\Proj`) != "d:/data/proj" {
		t.Fatalf("normalizeMountPath 错误: %q", normalizeMountPath(`D:\Data\Proj`))
	}
}

func TestDockerRunArgsShared(t *testing.T) {
	s := Settings{
		Transport: "docker",
		Image:     "bma/firecrawl-mcp:local",
		Ports:     []string{"6081:6081"},
		Volumes:   []string{"D:/data/workspace:/workspace"},
		Env:       map[string]string{"FIRECRAWL_API_KEY": "self-hosted"},
	}
	args := dockerRunArgsShared("bma-plugin-web_search-ab12cd34", s)
	want := []string{
		"run", "-d", "--name", "bma-plugin-web_search-ab12cd34",
		"--add-host", "host.docker.internal:host-gateway",
		"-p", "6081:6081",
		"-v", "D:/data/workspace:/workspace",
		"-e", "FIRECRAWL_API_KEY=self-hosted",
		"--entrypoint", "sleep", "bma/firecrawl-mcp:local", "infinity",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}

	// 自定义 entrypoint + container_env（与 Env 键冲突时 Env 优先）。
	s2 := Settings{
		Transport:           "docker",
		Image:               "bma/computer-use-mcp:local",
		Env:                 map[string]string{"K": "env"},
		ContainerEnv:        map[string]string{"K": "container", "BMA_KEEPER": "1"},
		ContainerEntrypoint: []string{"/entrypoint.sh"},
	}
	args2 := dockerRunArgsShared("c", s2)
	got := strings.Join(args2, " ")
	wantStr := "run -d --name c --add-host host.docker.internal:host-gateway " +
		"-e BMA_KEEPER=1 -e K=env --entrypoint /entrypoint.sh bma/computer-use-mcp:local"
	if got != wantStr {
		t.Errorf("args = %q, want %q", got, wantStr)
	}
}

func TestDockerExecArgs(t *testing.T) {
	s := Settings{
		ExecCommand: []string{"node", "cli.js"},
		Args:        []string{"--headless", "--output-dir", "/workspace/.bma/ui-artifacts"},
	}
	got := dockerExecArgs("bma-plugin-ui_preview-ab12cd34", s)
	want := []string{
		"exec", "-i", "bma-plugin-ui_preview-ab12cd34",
		"node", "cli.js", "--headless", "--output-dir", "/workspace/.bma/ui-artifacts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args =\n%q\nwant\n%q", got, want)
	}
}

func TestParseDockerInspectRunning(t *testing.T) {
	if running, image := parseDockerInspectRunning("true img:tag\n"); !running || image != "img:tag" {
		t.Fatalf("running 解析错误: %v %q", running, image)
	}
	if running, _ := parseDockerInspectRunning("false img:tag"); running {
		t.Fatal("stopped 容器应解析为 false")
	}
	if running, image := parseDockerInspectRunning("garbage"); running || image != "" {
		t.Fatalf("非法输出应返回 false/空: %v %q", running, image)
	}
}

func TestFromSettingsShared(t *testing.T) {
	s := FromSettings(map[string]any{
		"transport":            "docker",
		"image":                "img:tag",
		"shared":               true,
		"exec_command":         []any{"node", "server.js"},
		"container_entrypoint": []any{"/entrypoint.sh"},
		"container_env":        map[string]any{"BMA_KEEPER": "1"},
	})
	if !s.Shared || s.ExecCommand[0] != "node" || s.ExecCommand[1] != "server.js" {
		t.Fatalf("shared 字段解析错误: %+v", s)
	}
	if s.ContainerEntrypoint[0] != "/entrypoint.sh" || s.ContainerEnv["BMA_KEEPER"] != "1" {
		t.Fatalf("container 字段解析错误: %+v", s)
	}

	// shared 缺 exec_command → Init 报错。
	b := New("p", s, nil)
	b.settings.ExecCommand = nil
	if err := b.Init(context.Background(), plugins.Deps{WorkDir: "w"}); err == nil {
		t.Fatal("shared 缺 exec_command 应报错")
	}

	// shared 仅支持 docker transport。
	b2 := New("p2", FromSettings(map[string]any{"transport": "stdio", "command": "x", "shared": true}), nil)
	if err := b2.Init(context.Background(), plugins.Deps{}); err == nil {
		t.Fatal("shared + stdio 应报错")
	}

	// 合法 shared：Init 预计算容器名（全局确定，与 workdir 无关）。
	b3 := New("p3", s, nil)
	if err := b3.Init(context.Background(), plugins.Deps{WorkDir: "w"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if want := dockerSharedContainerName("p3"); b3.containerName != want {
		t.Fatalf("containerName = %q, want %q", b3.containerName, want)
	}
	if b3.workDir != "w" {
		t.Fatalf("workDir 未记录: %q", b3.workDir)
	}
}

func TestFromSettingsDocker(t *testing.T) {
	s := FromSettings(map[string]any{
		"transport": "docker",
		"image":     "img:tag",
		"env":       map[string]any{"K": "V"},
		"volumes":   []any{"D:/data/workspace/od-artifacts:/out", "bma-data:/data"},
	})
	if s.Transport != "docker" || s.Image != "img:tag" || s.Env["K"] != "V" {
		t.Fatalf("FromSettings = %+v", s)
	}
	if len(s.Volumes) != 2 || s.Volumes[0] != "D:/data/workspace/od-artifacts:/out" {
		t.Fatalf("volumes 解析错误: %v", s.Volumes)
	}

	b := New("p", s, nil)
	if err := b.Init(context.Background(), plugins.Deps{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	b2 := New("p2", FromSettings(map[string]any{"transport": "docker"}), nil)
	if err := b2.Init(context.Background(), plugins.Deps{}); err == nil {
		t.Fatal("docker transport 缺 image 应报错")
	}
}

// Init 展开 volumes 中的 ${WORKDIR} 占位符（deps.WorkDir 即 bootstrap 启动目录）。
func TestBridgeInitExpandsWorkDir(t *testing.T) {
	b := New("p", FromSettings(map[string]any{
		"transport": "docker",
		"image":     "img:tag",
		"volumes":   []any{"${WORKDIR}/.bma/od-artifacts:/out", "bma-data:/data"},
	}), nil)
	if err := b.Init(context.Background(), plugins.Deps{WorkDir: `D:\data\proj`}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if want := "D:/data/proj/.bma/od-artifacts:/out"; b.settings.Volumes[0] != want {
		t.Fatalf("Volumes[0] = %q, want %q", b.settings.Volumes[0], want)
	}
	if b.settings.Volumes[1] != "bma-data:/data" {
		t.Fatalf("命名卷被误改: %q", b.settings.Volumes[1])
	}
}
