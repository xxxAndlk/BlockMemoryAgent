package tool

// widget_tools_test.go 钉死 widget_save / widget_list 的契约：
// widget_save 写盘后文件可读回、更新保留 enabled、成功后经 pushFn 推
// {"__reload__":true} 热刷新信号（带 ctx 里的 sessionID）；widget_list 列出部件。
// 这条链路是"Agent 自写界面 → 用户端卡片自动热更新"的闭环源头。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/widgets"
)

// newWidgetTestEnv 构造带真实 widgets.Manager 的 Registry + 记录推送的 pushFn。
func newWidgetTestEnv(t *testing.T) (reg *Registry, mgr *widgets.Manager, pushes *[]string) {
	t.Helper()
	m, err := widgets.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	pushes = &[]string{}
	reg = NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.SetWidgetManager(m, func(sessionID, widgetID, payload string) {
		*pushes = append(*pushes, sessionID+"|"+widgetID+"|"+payload)
	})
	return reg, m, pushes
}

func TestWidgetSave_WritesFilesAndPushesReload(t *testing.T) {
	reg, mgr, pushes := newWidgetTestEnv(t)

	ctx := WithSessionID(context.Background(), "session-x")
	res, err := reg.Dispatch(ctx, "widget_save", map[string]any{
		"id":   "wechat-qr",
		"name": "微信二维码",
		"files": []any{
			map[string]any{"path": "index.html", "content": "<html>qr</html>"},
			map[string]any{"path": "app.js", "content": "// qr"},
		},
	})
	if err != nil || !res.Success {
		t.Fatalf("widget_save 应成功: res=%+v err=%v", res, err)
	}
	// 写盘可读回（管理页/文件服务读的就是磁盘）。
	data, err := os.ReadFile(filepath.Join(mgr.Dir(), "wechat-qr", "app.js"))
	if err != nil || string(data) != "// qr" {
		t.Fatalf("文件读回不符: %q err=%v", string(data), err)
	}
	// 成功后向当前会话推 __reload__ 热刷新信号。
	if len(*pushes) != 1 || (*pushes)[0] != `session-x|wechat-qr|{"__reload__":true}` {
		t.Fatalf("应推 __reload__: %v", *pushes)
	}
	// 回执带 ShowArtifact 调用示例。
	if !strings.Contains(res.Output, "ShowArtifact(kind=widget") {
		t.Fatalf("回执应带调用示例: %q", res.Output)
	}
	// 清单落盘：默认 entry=index.html、默认停用。
	info, ok := mgr.Get("wechat-qr")
	if !ok || info.Entry != "index.html" || info.Enabled || info.Name != "微信二维码" {
		t.Fatalf("清单不符: %+v ok=%v", info, ok)
	}
}

func TestWidgetSave_UpdatePreservesEnabled(t *testing.T) {
	reg, mgr, _ := newWidgetTestEnv(t)
	ctx := WithSessionID(context.Background(), "s1")
	save := func() *Result {
		res, _ := reg.Dispatch(ctx, "widget_save", map[string]any{
			"id":    "card",
			"files": []any{map[string]any{"path": "index.html", "content": "v"}},
		})
		return res
	}
	if res := save(); !res.Success {
		t.Fatalf("首次保存: %+v", res)
	}
	if _, err := mgr.SetEnabled("card", true); err != nil {
		t.Fatal(err)
	}
	if res := save(); !res.Success {
		t.Fatalf("二次保存: %+v", res)
	}
	if info, _ := mgr.Get("card"); !info.Enabled {
		t.Fatal("更新应保留 enabled=true")
	}
}

func TestWidgetSave_ValidationRejected(t *testing.T) {
	reg, _, pushes := newWidgetTestEnv(t)
	ctx := WithSessionID(context.Background(), "s1")

	cases := []map[string]any{
		{"id": "Bad_ID", "files": []any{map[string]any{"path": "index.html"}}},
		{"id": "ok", "files": []any{map[string]any{"path": "../x.js"}}},
		{"id": "ok", "files": []any{map[string]any{"path": "index.html"}}, "entry": "main.html"},
		{"id": "ok", "files": []any{}},
	}
	for i, args := range cases {
		res, _ := reg.Dispatch(ctx, "widget_save", args)
		if res.Success || res.Category != ResultCategoryValidationRejected {
			t.Fatalf("case %d 应 validation_rejected: %+v", i, res)
		}
	}
	// 校验拒绝不得推热刷新。
	if len(*pushes) != 0 {
		t.Fatalf("校验拒绝不应推送: %v", *pushes)
	}
}

func TestWidgetSave_NoSessionNoPush(t *testing.T) {
	reg, _, pushes := newWidgetTestEnv(t)
	// 无 sessionID 上下文：保存成功但不推送（程序化/测试调用路径）。
	res, _ := reg.Dispatch(context.Background(), "widget_save", map[string]any{
		"id":    "card",
		"files": []any{map[string]any{"path": "index.html", "content": "v"}},
	})
	if !res.Success {
		t.Fatalf("保存应成功: %+v", res)
	}
	if len(*pushes) != 0 {
		t.Fatalf("无会话上下文不应推送: %v", *pushes)
	}
}

func TestWidgetSave_NotWired(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), nil, nil) // 不注入 mgr
	res, _ := reg.Dispatch(context.Background(), "widget_save", map[string]any{
		"id":    "card",
		"files": []any{map[string]any{"path": "index.html", "content": "v"}},
	})
	if res.Success || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("未接线应报未接线错误: %+v", res)
	}
}

func TestWidgetList_Output(t *testing.T) {
	reg, mgr, _ := newWidgetTestEnv(t)

	// 空列表：给创建引导而非干巴巴的"无"。
	res, _ := reg.Dispatch(context.Background(), "widget_list", map[string]any{})
	if !res.Success || !strings.Contains(res.Output, "widget_save") {
		t.Fatalf("空列表应带创建引导: %q", res.Output)
	}

	if _, err := mgr.Save("wechat-qr", widgets.Manifest{Name: "微信二维码", Description: "扫码画布"},
		[]widgets.FileInput{{Path: "index.html", Content: "<html></html>"}}); err != nil {
		t.Fatal(err)
	}
	res, _ = reg.Dispatch(context.Background(), "widget_list", map[string]any{})
	if !res.Success {
		t.Fatalf("widget_list 应成功: %+v", res)
	}
	for _, want := range []string{"wechat-qr", "微信二维码", "扫码画布", "未启用", "entry=index.html"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("列表输出缺 %q: %q", want, res.Output)
		}
	}
}
