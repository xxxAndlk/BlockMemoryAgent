package plugins

// dockercheck_test.go docker 依赖自检的纯函数单测（CI/本机无 docker 均可跑）：
//   - settings 解析（只有 transport=docker 才算容器插件）；
//   - docker ps --format '{{json .}}' 单行解析（服务标签/keeper 判定）；
//   - docker image prune 输出解析（删除数/回收空间）；
//   - 自检报告 Healthy/Summary 口径（残留停止容器与缺镜像才算不健康）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerImageOf(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"docker", map[string]any{"transport": "docker", "image": "bma/open-design-mcp:local"}, "bma/open-design-mcp:local"},
		{"stdio 忽略 image", map[string]any{"transport": "stdio", "image": "x"}, ""},
		{"http 忽略", map[string]any{"transport": "http", "url": "https://x"}, ""},
		{"无 transport 忽略", map[string]any{"image": "x"}, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		if got := dockerImageOf(c.settings); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// 启动期清理在 Load 之前跑，实例表为空，必须能从磁盘 plugins.yaml 推出
// 启用中的 docker 插件（否则过期 keeper 永远判定不出来）。
func TestDockerPluginImagesFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfg := `plugins:
  ui_design:
    kind: mcp
    enabled: true
    settings:
      transport: docker
      image: bma/open-design-mcp:local
  web_search:
    kind: mcp
    enabled: true
    settings:
      transport: http
      url: https://example.invalid/mcp
  computer_use:
    kind: mcp
    enabled: false
    settings:
      transport: docker
      image: bma/computer-use-mcp:local
`
	if err := os.WriteFile(filepath.Join(dir, "plugins.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write plugins.yaml: %v", err)
	}
	m := NewManager(nil, WithConfigDir(dir))
	got := m.dockerPluginImages()
	if len(got) != 1 || got["ui_design"] != "bma/open-design-mcp:local" {
		t.Fatalf("dockerPluginImages = %v（只应含启用中的 ui_design）", got)
	}
}

func TestParseDockerPSContainer(t *testing.T) {
	line := `{"Names":"bma-plugin-ui_design-50e934ac","Image":"bma/open-design-mcp:local","State":"running","Status":"Up 2 days","Labels":"com.docker.compose.project=bma,com.docker.compose.service=ui_design"}`
	c, ok := parseDockerPSContainer(line)
	if !ok {
		t.Fatal("应解析成功")
	}
	if c.Name != "bma-plugin-ui_design-50e934ac" || c.Service != "ui_design" ||
		c.State != "running" || c.Image != "bma/open-design-mcp:local" || !c.PluginKeeper {
		t.Fatalf("解析结果不符: %+v", c)
	}

	// compose 长驻服务（非 bma-plugin-*）：PluginKeeper=false。
	c, ok = parseDockerPSContainer(`{"Names":"bma-open-design","Image":"bma/open-design-daemon:local","State":"running","Status":"Up 3 days (healthy)","Labels":"com.docker.compose.project=bma,com.docker.compose.service=open_design"}`)
	if !ok || c.PluginKeeper || c.Service != "open_design" {
		t.Fatalf("compose 服务解析不符: ok=%v %+v", ok, c)
	}

	// 多名字取第一个。
	if c, _ := parseDockerPSContainer(`{"Names":"a,b","State":"exited"}`); c.Name != "a" {
		t.Fatalf("多名字应取首个: %+v", c)
	}

	// 空行/坏 JSON 应跳过。
	for _, bad := range []string{"", "   ", "not-json"} {
		if _, ok := parseDockerPSContainer(bad); ok {
			t.Fatalf("非法行应跳过: %q", bad)
		}
	}
}

// 悬空镜像 prune 的安全口径：被任何容器（含已停止）引用的镜像一律不删。
func TestImageIDInUse(t *testing.T) {
	set := map[string]bool{"sha256:c41f0c651958a4d11d94b11c75935c4e": true}
	if !imageIDInUse(set, "c41f0c651958") {
		t.Fatal("短 ID 应命中完整 ID 引用")
	}
	if !imageIDInUse(set, "sha256:c41f0c651958a4d11d94b11c75935c4e") {
		t.Fatal("完整 ID 应命中")
	}
	if imageIDInUse(set, "deadbeef0000") {
		t.Fatal("未被引用的镜像不应命中")
	}
	if imageIDInUse(nil, "anything") {
		t.Fatal("空集合不应命中")
	}
}

func TestHumanBytesAndLines(t *testing.T) {
	cases := map[int64]string{
		0:          "0B",
		431 * 1024: "431.0KB",
		2048:       "2.0KB",
		1610612736: "1.5GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	if got := splitOutputLines(" a \n\n b\r\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("splitOutputLines = %q", got)
	}
	if shortDockerID("sha256:c41f0c651958a4d11d94b11c75935c4e") != "c41f0c651958" {
		t.Fatal("shortDockerID 截短错误")
	}
}

func TestDockerReportHealthyAndSummary(t *testing.T) {
	ok := DockerReport{
		Available: true,
		Version:   "27.0.3",
		Images:    []DockerImageStatus{{Plugin: "ui_design", Image: "img", Present: true}},
		Containers: []DockerContainerStatus{
			{Name: "bma-open-design", State: "running"},
			{Name: "bma-plugin-ui_design-abc", State: "running", PluginKeeper: true},
		},
	}
	if !ok.Healthy() {
		t.Fatalf("无残留应健康: %+v", ok)
	}
	if s := ok.Summary(); !strings.Contains(s, "running=2") || !strings.Contains(s, "docker 27.0.3") {
		t.Fatalf("摘要不符: %s", s)
	}

	bad := DockerReport{
		Available:         true,
		MissingImages:     []string{"bma/open-design-mcp:local"},
		StoppedContainers: []string{"bma-plugin-ui_design-old"},
		Containers:        []DockerContainerStatus{{Name: "bma-plugin-ui_design-old", State: "exited"}},
	}
	if bad.Healthy() {
		t.Fatal("缺镜像+残留容器应不健康")
	}
	s := bad.Summary()
	if !strings.Contains(s, "缺镜像") || !strings.Contains(s, "残留停止容器") || !strings.Contains(s, "stopped=1") {
		t.Fatalf("摘要应含问题项: %s", s)
	}

	// 过期 keeper（镜像重建后没换容器）同样算不健康，摘要要能看出来。
	stale := DockerReport{
		Available:       true,
		StaleContainers: []string{"bma-plugin-ui_design-50e934ac"},
		Containers:      []DockerContainerStatus{{Name: "bma-plugin-ui_design-50e934ac", State: "running", PluginKeeper: true, Stale: true}},
	}
	if stale.Healthy() {
		t.Fatal("有过期 keeper 应不健康")
	}
	if !strings.Contains(stale.Summary(), "过期 keeper") {
		t.Fatalf("摘要应含过期 keeper: %s", stale.Summary())
	}

	unavailable := DockerReport{Error: "docker 不可达", Required: true}
	if unavailable.Healthy() {
		t.Fatal("有 docker 插件依赖时不可用应不健康")
	}
	if !strings.Contains(unavailable.Summary(), "docker 不可用") {
		t.Fatalf("不可用摘要不符: %s", unavailable.Summary())
	}
	// 无 docker 插件依赖（纯 http/stdio 部署）：docker 缺失不算不健康。
	noDep := DockerReport{Error: "docker 不可达"}
	if !noDep.Healthy() {
		t.Fatal("无 docker 依赖时缺失不应判不健康")
	}
	if !strings.Contains(noDep.Summary(), "不影响使用") {
		t.Fatalf("无依赖摘要不符: %s", noDep.Summary())
	}
}
