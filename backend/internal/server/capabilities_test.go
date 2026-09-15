package server

// capabilities_test.go 装机能力自检（TODO #18-1 T28）：
// 零真实 PG/Redis/插件——nil 依赖全字段降级不 panic；带 workDir 的可写探针；
// 插件聚合口径（停用不计入问题清单）经注入真实 plugins.Manager + 缺 env 清单验证。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// getCapabilities 起测试路由请求 /api/capabilities 并解出 items。
func getCapabilities(t *testing.T, h *APIHandler) []CapabilityItem {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/capabilities", h.CapabilitiesHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []CapabilityItem `json:"items"`
		AllOK bool             `json:"all_ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body %s", err, w.Body.String())
	}
	if len(resp.Items) != 6 {
		t.Fatalf("期望 6 项自检，got %d: %+v", len(resp.Items), resp.Items)
	}
	if resp.AllOK {
		t.Fatalf("nil 依赖下 all_ok 不应为 true: %+v", resp.Items)
	}
	return resp.Items
}

func capabilityOf(items []CapabilityItem, name string) CapabilityItem {
	for _, it := range items {
		if it.Name == name {
			return it
		}
	}
	return CapabilityItem{Name: "missing:" + name}
}

// TestCapabilities_NilDepsDegrade 验证零注入依赖时六项自检全部降级返回、无 panic。
func TestCapabilities_NilDepsDegrade(t *testing.T) {
	items := getCapabilities(t, &APIHandler{})
	for _, name := range []string{"llm", "postgres", "redis", "embed", "plugins", "workdir"} {
		if capabilityOf(items, name).Name != name {
			t.Fatalf("缺 %s 项: %+v", name, items)
		}
	}
	if it := capabilityOf(items, "postgres"); it.OK || it.Hint == "" {
		t.Fatalf("nil pg 应 ok=false 且带修复提示: %+v", it)
	}
	if it := capabilityOf(items, "llm"); it.OK {
		t.Fatalf("nil 模型工厂应 ok=false: %+v", it)
	}
}

// TestCapabilities_WorkDirProbe 验证工作目录可写探针：临时目录 OK，指向文件的目录 FAIL。
func TestCapabilities_WorkDirProbe(t *testing.T) {
	dir := t.TempDir()
	h := &APIHandler{}
	h.SetDefaultWorkDir(dir)
	if it := h.capabilityWorkDir(); !it.OK {
		t.Fatalf("临时目录应可写: %+v", it)
	}
	// 探针文件应已清理
	if _, err := os.Stat(filepath.Join(dir, ".bma_capability_probe.tmp")); !os.IsNotExist(err) {
		t.Fatalf("探针文件未清理")
	}

	// 用一个文件路径冒充目录 → os.WriteFile 失败 → ok=false 且带提示
	notDir := filepath.Join(dir, "iam_a_file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2 := &APIHandler{}
	h2.SetDefaultWorkDir(notDir)
	if it := h2.capabilityWorkDir(); it.OK || it.Hint == "" {
		t.Fatalf("不可写目录应 ok=false 且带提示: %+v", it)
	}
}

// TestCapabilities_EmbedKeys 验证嵌入项按 provider 分档：pseudo 免 Key 直通，
// openai 无 Key/端点报缺失并给 hint，base_url 兜底直通。
func TestCapabilities_EmbedKeys(t *testing.T) {
	h := &APIHandler{}
	h.SetRoleConfig(&pkgconfig.RoleConfigFile{})

	h.roleCfg.Embed = types.EmbedConfig{Provider: "pseudo"}
	if it := h.capabilityEmbed(); !it.OK {
		t.Fatalf("pseudo 应直通: %+v", it)
	}

	h.roleCfg.Embed = types.EmbedConfig{Provider: "openai", Model: "text-embedding-3-small"}
	if it := h.capabilityEmbed(); it.OK || len(it.Missing) == 0 {
		t.Fatalf("openai 无 Key 应报缺失: %+v", it)
	}

	h.roleCfg.Embed = types.EmbedConfig{Provider: "openai", BaseURL: "http://127.0.0.1:9999/v1"}
	if it := h.capabilityEmbed(); !it.OK {
		t.Fatalf("本地端点免 Key 应直通: %+v", it)
	}
}
