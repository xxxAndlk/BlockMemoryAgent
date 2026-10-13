package tool

// show_artifact_test.go 钉死 ShowArtifact 的契约：登记工作区相对路径 + MIME，
// 内联 HTML 落盘到 .bma/artifacts，越界/类型不符一律 validation_rejected。
// 这条链路是"对话栏能不能看到效果图/HTML 预览"的写侧源头。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dispatchShowArtifact 在临时工作目录里跑一次工具调用。
func dispatchShowArtifact(t *testing.T, dir string, args map[string]any) *Result {
	t.Helper()
	r := NewBuiltinRegistry(dir, nil, nil)
	res, err := r.Dispatch(WithWorkDir(context.Background(), dir), "ShowArtifact", args)
	if err != nil {
		t.Fatalf("Dispatch 出错: %v", err)
	}
	return res
}

func TestShowArtifact_ImageByPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets", "img")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "hero.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := dispatchShowArtifact(t, dir, map[string]any{
		"kind": "image", "path": "assets/img/hero.png", "title": "主界面效果图",
	})
	if !res.Success {
		t.Fatalf("应成功: %+v", res)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("应登记 1 个成果, got %+v", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Kind != "image" || a.Path != "assets/img/hero.png" || a.MIME != "image/png" || a.Title != "主界面效果图" {
		t.Fatalf("成果字段不符: %+v", a)
	}
	if strings.Contains(a.Path, "\\") {
		t.Fatalf("路径应为正斜杠（前端拼 URL 用）: %q", a.Path)
	}
	if !strings.Contains(res.Output, "主界面效果图") {
		t.Fatalf("回执应带标题: %q", res.Output)
	}
}

func TestShowArtifact_InlineHTMLWritesToArtifactsDir(t *testing.T) {
	dir := t.TempDir()
	const html = "<html><body><h1>草案</h1></body></html>"
	res := dispatchShowArtifact(t, dir, map[string]any{"kind": "html", "content": html, "title": "原型 v1"})
	if !res.Success {
		t.Fatalf("内联 HTML 应成功: %+v", res)
	}
	a := res.Artifacts[0]
	if a.Kind != "html" || a.MIME != "text/html" {
		t.Fatalf("成果字段不符: %+v", a)
	}
	if !strings.HasPrefix(a.Path, ".bma/artifacts/") || !strings.HasSuffix(a.Path, ".html") {
		t.Fatalf("内联 HTML 应落 .bma/artifacts: %q", a.Path)
	}
	// 落盘内容一致（对话栏 iframe 读的就是这份）。
	abs := filepath.Join(dir, filepath.FromSlash(a.Path))
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("落盘文件不可读: %v", err)
	}
	if string(data) != html {
		t.Fatalf("落盘内容不符: %q", string(data))
	}
}

func TestShowArtifact_RejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 工作目录外的文件：即便路径合法存在，也不得展示（对话栏只能引用工作区内文件）。
	outside := filepath.Join(filepath.Dir(dir), "outside.png")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"kind 非法", map[string]any{"kind": "exe", "path": "a.png"}},
		{"缺来源", map[string]any{"kind": "image"}},
		{"文件不存在", map[string]any{"kind": "image", "path": "nope.png"}},
		{"扩展名与 kind 不符", map[string]any{"kind": "video", "path": "a.png"}},
		{"越界绝对路径", map[string]any{"kind": "image", "path": outside}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchShowArtifact(t, dir, tc.args)
			if res.Success || res.Category != ResultCategoryValidationRejected {
				t.Fatalf("应 validation_rejected, got success=%t category=%s err=%s",
					res.Success, res.Category, res.Error)
			}
			if len(res.Artifacts) != 0 {
				t.Fatalf("失败不得登记成果: %+v", res.Artifacts)
			}
		})
	}
}

// TestShowArtifact_ResultJSONCarriesArtifacts 验证成果随 Result 序列化进入事件 Detail
//（registry.emitResult 走的就是这条 marshal 路径，前端 detail_json 读的也是它）。
func TestShowArtifact_ResultJSONCarriesArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.jpg"), []byte("j"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := dispatchShowArtifact(t, dir, map[string]any{"kind": "image", "path": "shot.jpg", "caption": "截图"})
	raw, err := marshalNoHTMLEscape(res)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"artifacts"`, `"kind":"image"`, `"path":"shot.jpg"`, `"caption":"截图"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("Result JSON 缺 %s:\n%s", want, s)
		}
	}
	// 二进制绝不进 JSON（只带路径）。
	if strings.Contains(s, "base64") {
		t.Fatalf("Result JSON 不应含 base64 内容:\n%s", s)
	}
}


// dispatchShowArtifactWidget 在注入了 widgetLookup 的 Registry 上跑一次 kind=widget 调用。
func dispatchShowArtifactWidget(t *testing.T, lookup func(id string) (string, bool, bool), args map[string]any) *Result {
	t.Helper()
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetWidgetLookup(lookup)
	res, err := r.Dispatch(context.Background(), "ShowArtifact", args)
	if err != nil {
		t.Fatalf("Dispatch 出错: %v", err)
	}
	return res
}

// TestShowArtifact_WidgetKind 钉死 kind=widget 契约：引用已启用部件登记
// widget:// 工件、data 透传进 Result JSON（detail_json 链路）；
// 部件不存在/停用/未接线一律 validation_rejected。
func TestShowArtifact_WidgetKind(t *testing.T) {
	enabledLookup := func(id string) (string, bool, bool) {
		if id == "wechat-qr" {
			return "微信二维码", true, true
		}
		if id == "disabled-card" {
			return "停用卡片", false, true
		}
		return "", false, false
	}

	// 正常登记：Path 为 widget://<id>，data/title/caption 透传。
	res := dispatchShowArtifactWidget(t, enabledLookup, map[string]any{
		"kind": "widget", "widget_id": "wechat-qr", "title": "扫码登录", "caption": "二维码实时刷新",
		"data": `{"url":"https://x/qr"}`,
	})
	if !res.Success {
		t.Fatalf("应成功: %+v", res)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("应登记 1 个成果: %+v", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Kind != "widget" || a.Path != "widget://wechat-qr" || a.Title != "扫码登录" ||
		a.Caption != "二维码实时刷新" || a.Data != `{"url":"https://x/qr"}` {
		t.Fatalf("成果字段不符: %+v", a)
	}
	// data 透传进 Result JSON（registry.emitResult → 事件 detail_json 的 marshal 路径）。
	raw, err := marshalNoHTMLEscape(res)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); !strings.Contains(s, `"data":"{\"url\":\"https://x/qr\"}"`) {
		t.Fatalf("Result JSON 应携带 data:\n%s", s)
	}

	// 拒绝场景：部件不存在 / 停用 / 缺 widget_id / 未接线 / data 超限。
	t.Run("部件不存在", func(t *testing.T) {
		res := dispatchShowArtifactWidget(t, enabledLookup, map[string]any{"kind": "widget", "widget_id": "ghost"})
		if res.Success || res.Category != ResultCategoryValidationRejected || !strings.Contains(res.Error, "不存在") {
			t.Fatalf("应拒绝: %+v", res)
		}
	})
	t.Run("部件停用", func(t *testing.T) {
		res := dispatchShowArtifactWidget(t, enabledLookup, map[string]any{"kind": "widget", "widget_id": "disabled-card"})
		if res.Success || res.Category != ResultCategoryValidationRejected || !strings.Contains(res.Error, "停用") {
			t.Fatalf("应拒绝: %+v", res)
		}
	})
	t.Run("缺 widget_id", func(t *testing.T) {
		res := dispatchShowArtifactWidget(t, enabledLookup, map[string]any{"kind": "widget"})
		if res.Success || res.Category != ResultCategoryValidationRejected {
			t.Fatalf("应拒绝: %+v", res)
		}
	})
	t.Run("未接线", func(t *testing.T) {
		res := dispatchShowArtifactWidget(t, nil, map[string]any{"kind": "widget", "widget_id": "wechat-qr"})
		if res.Success || res.Category != ResultCategoryValidationRejected || !strings.Contains(res.Error, "未接线") {
			t.Fatalf("应拒绝并提示未接线: %+v", res)
		}
	})
	t.Run("data 超限", func(t *testing.T) {
		res := dispatchShowArtifactWidget(t, enabledLookup, map[string]any{
			"kind": "widget", "widget_id": "wechat-qr", "data": strings.Repeat("x", artifactWidgetDataMaxBytes+1),
		})
		if res.Success || res.Category != ResultCategoryValidationRejected {
			t.Fatalf("应拒绝: %+v", res)
		}
	})
}
