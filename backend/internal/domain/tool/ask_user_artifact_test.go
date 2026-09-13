package tool

// ask_user_artifact_test.go 验证 ask_user 工具 artifacts 参数（评审卡内嵌演示产物）
// 的解析与单题透传：合法条目透传、缺 kind/path 丢弃、批量模式不透传。

import (
	"context"
	"testing"
)

// TestAskUserTool_ArtifactsPassThrough 单题模式 artifacts 解析后透传给 hook；
// 缺 kind/path 的条目与非对象垃圾条目静默丢弃（降级为不带产物提问，不报错）。
func TestAskUserTool_ArtifactsPassThrough(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var got []AskUserArtifact
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		got = opts.Artifacts
		return "通过", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"question": "请查验演示视频",
		"artifacts": []any{
			map[string]any{"kind": "video", "path": ".bma/demo/demo.webm", "title": "演示视频", "caption": "逐页操作", "mime": "video/webm"},
			map[string]any{"kind": "", "path": ".bma/demo/x.webm"}, // 缺 kind 丢弃
			map[string]any{"kind": "video"},                        // 缺 path 丢弃
			"垃圾条目",
		},
	})
	if !res.Success {
		t.Fatalf("ask_user should succeed, got: %+v", res)
	}
	if len(got) != 1 {
		t.Fatalf("artifacts 应仅保留 1 条合法产物, got %+v", got)
	}
	if got[0].Kind != "video" || got[0].Path != ".bma/demo/demo.webm" ||
		got[0].Title != "演示视频" || got[0].Caption != "逐页操作" || got[0].MIME != "video/webm" {
		t.Fatalf("artifact 字段透传错误: %+v", got[0])
	}
}

// TestAskUserTool_ArtifactsBatchIgnored 批量模式（questions>1，未接线批量 hook
// 回退逐题）不透传 artifacts（产物仅单题模式支持）。
func TestAskUserTool_ArtifactsBatchIgnored(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var calls int
	r.SetAskUserHook(func(ctx context.Context, question string, opts AskUserOptions) (string, error) {
		calls++
		if len(opts.Artifacts) != 0 {
			t.Fatalf("批量模式不应透传 artifacts, got %+v", opts.Artifacts)
		}
		return "好", nil
	})
	res, _ := r.Dispatch(context.Background(), "ask_user", map[string]any{
		"questions": []any{
			map[string]any{"question": "问题一？"},
			map[string]any{"question": "问题二？"},
		},
		"artifacts": []any{
			map[string]any{"kind": "video", "path": ".bma/demo/demo.webm"},
		},
	})
	if !res.Success {
		t.Fatalf("ask_user should succeed, got: %+v", res)
	}
	if calls != 2 {
		t.Fatalf("批量回退应逐题调用 2 次, got %d", calls)
	}
}
