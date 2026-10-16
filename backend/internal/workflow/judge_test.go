package workflow

// judge_test.go 模型评委 JSON 解析与提示词组装单元测试（M1 验收面）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseJudgeOutput(t *testing.T) {
	// 带围栏的标准输出。
	res, err := parseJudgeOutput("```json\n{\"items\":[{\"item\":\"相位正确\",\"pass\":true,\"reason\":\"提示词已声明\"},{\"item\":\"无白边\",\"pass\":false,\"reason\":\"未覆盖\"}]}\n```")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if res.Pass || len(res.Items) != 2 || res.Items[0].Item != "相位正确" || res.Items[1].Pass {
		t.Fatalf("解析不符: %+v", res)
	}
	// 前后有噪声文本 + pass 为字符串。
	res, err = parseJudgeOutput("判定如下：{\"items\":[{\"item\":\"a\",\"pass\":\"true\",\"reason\":\"ok\"}]} 完毕")
	if err != nil || !res.Pass || len(res.Items) != 1 {
		t.Fatalf("噪声文本解析不符: %+v err=%v", res, err)
	}
	// 非 JSON。
	if _, err := parseJudgeOutput("我看图觉得还行"); err == nil {
		t.Fatalf("非 JSON 应报错")
	}
	// 空 items。
	if _, err := parseJudgeOutput(`{"items":[]}`); err == nil {
		t.Fatalf("空 items 应报错")
	}
}

func TestBuildJudgePromptContainsInputs(t *testing.T) {
	r, err := ParseRecipe([]byte(recipeFixture))
	if err != nil {
		t.Fatal(err)
	}
	item := RosterItem{ID: "ice_dragon", Name: "冰霜巨龙"}
	stage := r.StageByID("walk")
	synth := &Synthesis{OutputPath: "assets/units/ice_dragon/walk/ice_dragon_walk.png",
		PromptText: "行走循环", AnchorPaths: []string{"art/style_anchor.png"}}
	p := buildJudgePrompt(r, item, stage, synth, r.Check.Judge)
	for _, want := range []string{"冰霜巨龙", "walk", "行走循环", "art/style_anchor.png", r.Check.Judge[0], `"items"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("评委提示词缺 %q:\n%s", want, p)
		}
	}
}

// fakeJudgeModel 固定返回一份评委 JSON。
type fakeJudgeModel struct{ resp string }

func (f *fakeJudgeModel) CallLightweightWithRetry(_ context.Context, _ string) (string, error) {
	return f.resp, nil
}

func TestJudgeUnitPassAndFail(t *testing.T) {
	r, err := ParseRecipe([]byte(recipeFixture))
	if err != nil {
		t.Fatal(err)
	}
	item := RosterItem{ID: "a"}
	stage := r.StageByID("walk")
	synth := &Synthesis{OutputPath: "x.png", PromptText: "p"}
	pass := JudgeUnit(context.Background(), &fakeJudgeModel{resp: `{"items":[{"item":"x","pass":true,"reason":"ok"}]}`},
		2*time.Second, r, item, stage, synth)
	if !pass.Pass || pass.Err != "" {
		t.Fatalf("评委应通过: %+v", pass)
	}
	fail := JudgeUnit(context.Background(), &fakeJudgeModel{resp: `{"items":[{"item":"x","pass":false,"reason":"漂移"}]}`},
		2*time.Second, r, item, stage, synth)
	if fail.Pass || len(fail.Items) != 1 {
		t.Fatalf("评委应不通过: %+v", fail)
	}
	// 评委不可用（返回非 JSON）。
	bad := JudgeUnit(context.Background(), &fakeJudgeModel{resp: "not json"}, 2*time.Second, r, item, stage, synth)
	if bad.Err == "" {
		t.Fatalf("非 JSON 应标记 Err（走人审）: %+v", bad)
	}
	// 清单为空：跳过（Pass=true 不发调用）。
	r.Check.Judge = nil
	skip := JudgeUnit(context.Background(), nil, 0, r, item, stage, synth)
	if !skip.Pass || skip.Err != "" {
		t.Fatalf("空清单应跳过: %+v", skip)
	}
}
