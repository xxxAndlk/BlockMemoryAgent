package workflow

// freeze_test.go workflow_freeze 轨迹提取与草案生成单元测试（M3 验收面）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errFake = errors.New("模型不可用")

type fakeTraceLoader struct{ calls []TraceCall }

func (f *fakeTraceLoader) LoadTrace(_ context.Context, _ string) ([]TraceCall, error) {
	return f.calls, nil
}

// fakeParamModel 模拟参数化模型：返回固定的 JSON 切分结果。
type fakeParamModel struct{ resp string }

func (f *fakeParamModel) CallLightweightWithRetry(_ context.Context, _ string) (string, error) {
	return f.resp, nil
}

func TestFreezeDraftFromTrace(t *testing.T) {
	dir := t.TempDir()
	trace := &fakeTraceLoader{calls: []TraceCall{
		{Tool: "ReadFile", Args: map[string]any{"path": "x"}, Success: true, Seq: 1}, // 非生成工具，应剔除
		{Tool: "ui_design__od_image_generate", Args: map[string]any{"prompt": "冰霜巨龙的行走循环，三头身 Q 版", "save_as": "assets/ice/hero.png", "reference": "art/style_anchor.png"}, Output: `{"path":"assets/ice/hero.png"}`, Success: true, Seq: 2},
		{Tool: "ui_design__od_image_generate", Args: map[string]any{"prompt": "冰霜巨龙的攻击动作，三头身 Q 版", "save_as": "assets/ice/attack.png"}, Output: `{"path":"assets/ice/attack.png"}`, Success: true, Seq: 3},
		{Tool: "ui_design__od_image_generate", Args: map[string]any{"prompt": "失败的调用"}, Output: `{"error":"x"}`, Success: false, Seq: 4}, // 失败调用，应剔除
	}}
	param := `{"title":"冰霜系动作量产线","invariants":"三头身 Q 版，赛璐璐上色","slots":["id","name","element"],
	  "stages":[{"id":"walk","instruction":"行走循环"},{"id":"attack","instruction":"攻击动作"}],
	  "anchors":{"style":"art/style_anchor.png"},"judge":["动作可循环"],"deliverable_type":"sprite_sheet","frames":8,"cell":"128x128"}`
	eng := NewEngine(Config{}, nil, nil, &fakeParamModel{resp: param})
	eng.WithTraceLoader(trace)
	yamlText, info, err := eng.FreezeDraft(context.Background(), FreezeSpec{
		SessionID: "session-1", Workdir: dir, Title: "冰霜系动作量产线", RecipeID: "frost_motion",
	})
	if err != nil {
		t.Fatalf("生成草案失败: %v", err)
	}
	r, err := ParseRecipe([]byte(yamlText))
	if err != nil {
		t.Fatalf("草案不可解析: %v\n%s", err, yamlText)
	}
	if r.ID != "frost_motion" || r.Tool != "ui_design__od_image_generate" {
		t.Fatalf("id/tool 不符: %+v", r)
	}
	if len(r.Stages) != 2 || r.Stages[0].ID != "walk" {
		t.Fatalf("stages 不符: %+v", r.Stages)
	}
	if r.Anchors["style"] != "art/style_anchor.png" {
		t.Fatalf("锚点不符: %+v", r.Anchors)
	}
	if r.Deliverable.Type != "sprite_sheet" || r.Deliverable.Frames != 8 || r.Deliverable.Cell != "128x128" {
		t.Fatalf("交付契约不符: %+v", r.Deliverable)
	}
	if info.SourceCalls != 2 {
		t.Fatalf("应纳入 2 次成功生成调用, got %d", info.SourceCalls)
	}
	// 落盘（确认后的动作）→ version=1。
	saved, err := SaveRecipeYAML(dir, yamlText)
	if err != nil || saved.Version != 1 {
		t.Fatalf("落盘失败或版本不符: %v v=%d", err, saved.Version)
	}
}

func TestFreezeDraftNoGenerateCalls(t *testing.T) {
	eng := NewEngine(Config{}, nil, nil, &fakeParamModel{})
	eng.WithTraceLoader(&fakeTraceLoader{calls: []TraceCall{{Tool: "ReadFile", Success: true}}})
	_, _, err := eng.FreezeDraft(context.Background(), FreezeSpec{SessionID: "s", Workdir: t.TempDir(), Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "生成调用") {
		t.Fatalf("无生成调用应报错: %v", err)
	}
}

func TestFreezeDraftFallbackWhenModelFails(t *testing.T) {
	dir := t.TempDir()
	trace := &fakeTraceLoader{calls: []TraceCall{
		{Tool: "ui_design__od_image_generate", Args: map[string]any{"prompt": "英雄概念图", "save_as": "out/hero.png"}, Output: `{"path":"out/hero.png"}`, Success: true},
	}}
	errModel := &fakeModels{fn: func(string) (string, error) { return "", errFake }}
	eng := NewEngine(Config{}, nil, nil, errModel)
	eng.WithTraceLoader(trace)
	yamlText, info, err := eng.FreezeDraft(context.Background(), FreezeSpec{SessionID: "s", Workdir: dir, Title: "demo"})
	if err != nil {
		t.Fatalf("回退骨架应仍能出草案: %v", err)
	}
	r, err := ParseRecipe([]byte(yamlText))
	if err != nil {
		t.Fatalf("回退草案不可解析: %v\n%s", err, yamlText)
	}
	if r.ID != "demo" || len(r.Stages) == 0 {
		t.Fatalf("回退草案结构不符: %+v", r)
	}
	if len(info.Notes) == 0 {
		t.Fatalf("回退应带 Notes 说明")
	}
}

func TestLooksLikeRecipeYAML(t *testing.T) {
	if !looksLikeRecipeYAML("id: x\ntool: t\ndeliverable: {type: image}\nstages: [{id: a}]") {
		t.Fatalf("应识别为 YAML 草案")
	}
	if looksLikeRecipeYAML("确认") || looksLikeRecipeYAML("no thanks") {
		t.Fatalf("普通答复不应识别为 YAML")
	}
}

func TestMergeYAMLOverrides(t *testing.T) {
	base := "id: x\ntool: t\nstages: [{id: a}]\ndeliverable: {type: image, format: png}\n"
	merged, err := mergeYAMLOverrides(base, "invariants: 三头身\nparams: {reroll: 3}")
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if !strings.Contains(merged, "三头身") || !strings.Contains(merged, "reroll: 3") {
		t.Fatalf("覆盖段未生效:\n%s", merged)
	}
	// 覆盖不得改 id。
	merged, err = mergeYAMLOverrides(base, "id: other")
	if err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if strings.Contains(merged, "other") {
		t.Fatalf("id 不应被覆盖:\n%s", merged)
	}
}
