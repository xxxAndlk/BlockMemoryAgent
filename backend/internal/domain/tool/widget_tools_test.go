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
	// 成功后向当前会话推结构化热刷新信号（前端按 {type:"__widget_reload__"} 识别）。
	if len(*pushes) != 1 || (*pushes)[0] != `session-x|wechat-qr|{"__widget_reload__":true}` {
		t.Fatalf("应推 __widget_reload__: %v", *pushes)
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

// ctxWithSession 是带会话上下文的 ctx（工具经 SessionIDFromContext 读会话）。
func ctxWithSession() context.Context { return WithSessionID(context.Background(), "session-x") }

// TestWidgetPushData_ForwardsAndValidates 钉死 widget_push_data 契约：校验部件存在/
// 启用、经 pushFn+SessionIDFromContext 推送、无会话上下文报 validation_rejected、
// 未接线报错。
func TestWidgetPushData_ForwardsAndValidates(t *testing.T) {
	reg, mgr, pushes := newWidgetTestEnv(t)
	if _, err := mgr.Save("qr", widgets.Manifest{Name: "q", Entry: "index.html"},
		[]widgets.FileInput{{Path: "index.html", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SetEnabled("qr", true); err != nil {
		t.Fatal(err)
	}

	res, err := reg.Dispatch(ctxWithSession(), "widget_push_data", map[string]any{"widget_id": "qr", "payload": `{"n":1}`})
	if err != nil || !res.Success {
		t.Fatalf("应成功: %+v err=%v", res, err)
	}
	if len(*pushes) != 1 || (*pushes)[0] != `session-x|qr|{"n":1}` {
		t.Fatalf("应转发推送: %v", *pushes)
	}

	// 不存在的部件 / 未启用部件 / 空 widget_id / 无会话上下文。
	res, _ = reg.Dispatch(ctxWithSession(), "widget_push_data", map[string]any{"widget_id": "ghost", "payload": "{}"})
	if res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("部件不存在应 validation_rejected: %+v", res)
	}
	res, _ = reg.Dispatch(ctxWithSession(), "widget_push_data", map[string]any{"widget_id": "", "payload": "{}"})
	if res.Success {
		t.Fatal("空 widget_id 应拒绝")
	}
	res, _ = reg.Dispatch(context.Background(), "widget_push_data", map[string]any{"widget_id": "qr", "payload": "{}"})
	if res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("无会话上下文应 validation_rejected: %+v", res)
	}

	// 未接线（新 Registry 不注入）。
	bare := NewBuiltinRegistry(t.TempDir(), nil, nil)
	res, _ = bare.Dispatch(ctxWithSession(), "widget_push_data", map[string]any{"widget_id": "qr", "payload": "{}"})
	if res.Success || !strings.Contains(res.Error, "未接线") {
		t.Fatalf("未接线应报未接线错误: %+v", res)
	}
}

// TestWidgetSaveIncremental 钉死 widget_save 增量模式：upsert/delete 与 files 互斥、
// 局部改不丢其余文件、不存在的删除路径报错。
func TestWidgetSaveIncremental(t *testing.T) {
	reg, mgr, pushes := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{Name: "c", Entry: "index.html"},
		[]widgets.FileInput{{Path: "index.html", Content: "v1"}, {Path: "app.js", Content: "js1"}}); err != nil {
		t.Fatal(err)
	}

	res, err := reg.Dispatch(ctxWithSession(), "widget_save", map[string]any{
		"id":     "card",
		"upsert": []any{map[string]any{"path": "app.js", "content": "js2"}},
		"delete": []any{"index.html"},
	})
	// 删除入口文件且未指定新 entry 应报错（entry 必须在文件列表中）。
	if err == nil && res.Success {
		t.Fatal("删除入口文件应报错")
	}

	res, err = reg.Dispatch(ctxWithSession(), "widget_save", map[string]any{
		"id":     "card",
		"upsert": []any{map[string]any{"path": "app.js", "content": "js2"}, map[string]any{"path": "extra.css", "content": "x"}},
	})
	if err != nil || !res.Success {
		t.Fatalf("增量保存应成功: %+v err=%v", res, err)
	}
	info, _ := mgr.Get("card")
	if len(info.Files) != 3 {
		t.Fatalf("应保留 index.html + 更新 app.js + 新增 extra.css: %v", info.Files)
	}
	if data, _ := os.ReadFile(filepath.Join(mgr.Dir(), "card", "app.js")); string(data) != "js2" {
		t.Fatalf("upsert 内容不符: %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(mgr.Dir(), "card", "index.html")); string(data) != "v1" {
		t.Fatalf("未列出的文件应保持原样: %q", string(data))
	}
	if len(*pushes) != 1 || (*pushes)[0] != `session-x|card|{"__widget_reload__":true}` {
		t.Fatalf("增量保存也应推热刷新: %v", *pushes)
	}

	// files 与 upsert 互斥。
	res, _ = reg.Dispatch(ctxWithSession(), "widget_save", map[string]any{
		"id": "card", "upsert": []any{map[string]any{"path": "a", "content": "b"}},
		"files": []any{map[string]any{"path": "a", "content": "b"}},
	})
	if res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("files 与 upsert 互斥: %+v", res)
	}
	// 待删除文件不存在。
	res, _ = reg.Dispatch(ctxWithSession(), "widget_save", map[string]any{
		"id": "card", "delete": []any{"nope.js"},
	})
	if res.Success {
		t.Fatal("删除不存在文件应报错")
	}
}

// TestWidgetSaveOwner 钉死 owner 注入：ownerResolver(sessionID) 填充进 Manifest 并透出。
func TestWidgetSaveOwner(t *testing.T) {
	reg, mgr, _ := newWidgetTestEnv(t)
	reg.SetWidgetOwnerResolver(func(sessionID string) string {
		if sessionID == "session-x" {
			return "user-42"
		}
		return ""
	})
	res, err := reg.Dispatch(ctxWithSession(), "widget_save", map[string]any{
		"id": "card", "files": []any{map[string]any{"path": "index.html", "content": "x"}},
	})
	if err != nil || !res.Success {
		t.Fatalf("保存应成功: %+v err=%v", res, err)
	}
	info, _ := mgr.Get("card")
	if info.Owner != "user-42" {
		t.Fatalf("owner 应注入: %+v", info)
	}
}

// TestWidgetDeleteAndRestore 钉死 widget_delete / widget_restore 契约：软删回执带
// 回收站路径与恢复方式、恢复成功回执、列表空回收站、非法条目报错。
func TestWidgetDeleteAndRestore(t *testing.T) {
	reg, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{Name: "c", Entry: "index.html"},
		[]widgets.FileInput{{Path: "index.html", Content: "x"}}); err != nil {
		t.Fatal(err)
	}

	res, err := reg.Dispatch(context.Background(), "widget_delete", map[string]any{"id": "card"})
	if err != nil || !res.Success {
		t.Fatalf("删除应成功: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, ".trash") || !strings.Contains(res.Output, "widget_restore") {
		t.Fatalf("回执应带回收站路径与恢复方式: %q", res.Output)
	}
	if _, ok := mgr.Get("card"); ok {
		t.Fatal("软删后应不可见")
	}

	// 不带 name：列回收站条目。
	res, _ = reg.Dispatch(context.Background(), "widget_restore", map[string]any{})
	if !res.Success || !strings.Contains(res.Output, "card-del-") {
		t.Fatalf("应列出回收站条目: %q", res.Output)
	}
	// 取条目名恢复。
	list := mgr.TrashList()
	if len(list) != 1 {
		t.Fatalf("回收站应有 1 条: %+v", list)
	}
	res, err = reg.Dispatch(context.Background(), "widget_restore", map[string]any{"name": list[0].Name})
	if err != nil || !res.Success {
		t.Fatalf("恢复应成功: %+v err=%v", res, err)
	}
	if _, ok := mgr.Get("card"); !ok {
		t.Fatal("恢复后应可见")
	}
	// 非法条目名。
	res, _ = reg.Dispatch(context.Background(), "widget_restore", map[string]any{"name": "../evil"})
	if res.Success {
		t.Fatal("非法条目名应拒绝")
	}
}
