package plugins

// service 插件（kind=service）单元测试：
//   - settings 解析（缺省值 / 全字段）；
//   - 容器名净化 + pid 后缀（与 mcpbridge 同一约定）；
//   - docker run 参数构造（纯函数，不依赖 Docker）；
//   - Init 校验（image 必填）；
//   - Manager 集成：kind=service 实例创建、Info 携带 url、未知 kind 报错。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestServiceSettingsFromMapDefaults(t *testing.T) {
	s := serviceSettingsFromMap(map[string]any{})
	if s.Image != "" || s.HealthURL != "" || s.URL != "" {
		t.Fatalf("缺省字段应为空: %+v", s)
	}
	if s.StartTimeout != serviceStartTimeoutDefault {
		t.Fatalf("StartTimeout 缺省应为 %v, got %v", serviceStartTimeoutDefault, s.StartTimeout)
	}
}

func TestServiceSettingsFromMapFull(t *testing.T) {
	s := serviceSettingsFromMap(map[string]any{
		"image":             "ghcr.io/nexu-io/od:latest",
		"url":               "http://localhost:7456",
		"health_url":        "http://localhost:7456/api/health",
		"args":              []any{"--no-open"},
		"env":               map[string]any{"OD_API_TOKEN": "tok"},
		"ports":             []any{"127.0.0.1:7456:7456"},
		"volumes":           []any{"bma-open-design-data:/app/.od"},
		"requires_env":      []any{"OD_API_TOKEN"},
		"start_timeout_sec": float64(30),
	})
	if s.Image != "ghcr.io/nexu-io/od:latest" {
		t.Fatalf("image 解析错误: %q", s.Image)
	}
	if s.URL != "http://localhost:7456" || s.HealthURL != "http://localhost:7456/api/health" {
		t.Fatalf("url/health_url 解析错误: %+v", s)
	}
	if len(s.Args) != 1 || s.Args[0] != "--no-open" {
		t.Fatalf("args 解析错误: %v", s.Args)
	}
	if s.Env["OD_API_TOKEN"] != "tok" {
		t.Fatalf("env 解析错误: %v", s.Env)
	}
	if len(s.Ports) != 1 || len(s.Volumes) != 1 {
		t.Fatalf("ports/volumes 解析错误: %+v", s)
	}
	if len(s.RequiresEnv) != 1 || s.RequiresEnv[0] != "OD_API_TOKEN" {
		t.Fatalf("requires_env 解析错误: %v", s.RequiresEnv)
	}
	if s.StartTimeout != 30*time.Second {
		t.Fatalf("start_timeout_sec 解析错误: %v", s.StartTimeout)
	}
}

func TestServiceSettingsSyncFiles(t *testing.T) {
	s := serviceSettingsFromMap(map[string]any{
		"image": "img",
		"sync_files": []any{
			map[string]any{
				"path": "/app/.od/media-config.json",
				"json": map[string]any{
					"providers": map[string]any{
						"volcengine": map[string]any{"baseUrl": "https://ark.example/api/v3"},
					},
				},
			},
			map[string]any{"json": map[string]any{"x": "y"}}, // 缺 path，忽略
			"not-a-map", // 非法条目，忽略
		},
	})
	if len(s.SyncFiles) != 1 {
		t.Fatalf("sync_files 应解析出 1 条, got %d", len(s.SyncFiles))
	}
	f := s.SyncFiles[0]
	if f.Path != "/app/.od/media-config.json" {
		t.Fatalf("path 解析错误: %q", f.Path)
	}
	prov := f.JSON["providers"].(map[string]any)["volcengine"].(map[string]any)
	if prov["baseUrl"] != "https://ark.example/api/v3" {
		t.Fatalf("json 嵌套解析错误: %v", prov)
	}
}

func TestMergeJSONInto(t *testing.T) {
	dst := map[string]any{
		"providers": map[string]any{
			"volcengine": map[string]any{"apiKey": "stored-key", "baseUrl": "https://old"},
			"openai":     map[string]any{"apiKey": "keep-me"},
		},
	}
	mergeJSONInto(dst, map[string]any{
		"providers": map[string]any{
			"volcengine": map[string]any{"baseUrl": "https://new"},
		},
	})
	volc := dst["providers"].(map[string]any)["volcengine"].(map[string]any)
	if volc["baseUrl"] != "https://new" {
		t.Fatalf("src 应覆盖 dst: %v", volc)
	}
	if volc["apiKey"] != "stored-key" {
		t.Fatalf("未涉及的键应保留: %v", volc)
	}
	if dst["providers"].(map[string]any)["openai"] == nil {
		t.Fatal("其他 provider 应保留")
	}
}

func TestHasNonEmptyValue(t *testing.T) {
	empty := map[string]any{
		"providers": map[string]any{
			"volcengine": map[string]any{"baseUrl": ""},
		},
	}
	if hasNonEmptyValue(empty) {
		t.Fatal("全空字符串树应为 false")
	}
	if !hasNonEmptyValue(map[string]any{"a": map[string]any{"b": "x"}}) {
		t.Fatal("存在非空叶子应为 true")
	}
	if !hasNonEmptyValue(map[string]any{"n": float64(1), "b": false}) {
		t.Fatal("非字符串叶子（数字/布尔）应为 true")
	}
}

func TestServiceContainerName(t *testing.T) {
	// 确定性命名（无 pid 后缀）：同机多实例复用同一容器（固定端口本就强制共享）。
	if got := serviceContainerName("open_design"); got != "bma-plugin-svc-open_design" {
		t.Fatalf("容器名错误: %q", got)
	}
	// 非法字符净化（bundle 风格 id 含 /）。
	if got := serviceContainerName("a/b"); strings.Contains(got, "/") {
		t.Fatalf("容器名未净化: %q", got)
	}
}

func TestServiceRunArgs(t *testing.T) {
	s := serviceSettingsFromMap(map[string]any{
		"image":   "img:tag",
		"env":     map[string]any{"B": "2", "A": "1"},
		"ports":   []any{"127.0.0.1:7456:7456"},
		"volumes": []any{"data:/app/.od"},
		"args":    []any{"--no-open"},
	})
	args := serviceRunArgs("bma-plugin-svc-x-1", s)
	got := strings.Join(args, " ")
	want := "run -d --name bma-plugin-svc-x-1 --add-host host.docker.internal:host-gateway " +
		"-p 127.0.0.1:7456:7456 -v data:/app/.od -e A=1 -e B=2 img:tag --no-open"
	if got != want {
		t.Fatalf("docker run 参数不符:\n got: %s\nwant: %s", got, want)
	}
}

func TestServicePluginInitRequiresImage(t *testing.T) {
	p := newServicePlugin("x", map[string]any{}, nil)
	if err := p.Init(context.Background(), Deps{}); err == nil {
		t.Fatal("缺 image 应报错")
	}
	p = newServicePlugin("x", map[string]any{"image": "img"}, nil)
	if err := p.Init(context.Background(), Deps{}); err != nil {
		t.Fatalf("有 image 不应报错: %v", err)
	}
	// service 插件不注入工具。
	if tools := p.Tools(); len(tools) != 0 {
		t.Fatalf("service 插件 Tools 应为空, got %d", len(tools))
	}
}

func TestServicePluginManifest(t *testing.T) {
	p := newServicePlugin("open_design", map[string]any{
		"image":        "img",
		"url":          "http://localhost:7456",
		"requires_env": []any{"OD_API_TOKEN"},
	}, nil)
	m := p.Manifest()
	if m.Kind != KindService {
		t.Fatalf("Kind 应为 service, got %q", m.Kind)
	}
	if m.URL != "http://localhost:7456" {
		t.Fatalf("Manifest.URL 错误: %q", m.URL)
	}
	// RequiresEnv 缺失时 MissingEnv 应报出。
	t.Setenv("OD_API_TOKEN", "")
	if missing := m.MissingEnv(); len(missing) != 1 || missing[0] != "OD_API_TOKEN" {
		t.Fatalf("MissingEnv 错误: %v", missing)
	}
}

func TestManagerServiceKindLifecycle(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg, WithConfig(&config.PluginsConfig{Plugins: map[string]config.PluginConfig{
		"open_design": {
			Kind: "service",
			Settings: map[string]any{
				"image": "img",
				"url":   "http://localhost:7456",
			},
		},
	}}))
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	info, ok := mgr.Get("open_design")
	if !ok {
		t.Fatal("实例未创建")
	}
	if info.Kind != "service" || info.State != StateRegistered {
		t.Fatalf("状态错误: %+v", info)
	}
	if info.URL != "http://localhost:7456" {
		t.Fatalf("Info.URL 错误: %q", info.URL)
	}
	// enabled 缺省 false：未触碰 Docker，无工具注册。
	if len(info.Tools) != 0 {
		t.Fatalf("不应注册任何工具, got %v", info.Tools)
	}
}
