package workflow

// recipe_test.go 配方解析/校验/落盘版本规则的单元测试（M1 验收面）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recipeFixture = `
id: beast_motion
version: 3
title: 异兽动作量产线
tool: ui_design__od_image_generate
deliverable:
  type: sprite_sheet
  frames: 8
  cell: 128x128
  alpha: true
  format: png
  path: "assets/units/{{id}}/{{stage}}/"
stages:
  - { id: concept, anchor_out: true }
  - { id: walk, anchor_in: [style, family, concept], anchor_out: true }
  - { id: attack, anchor_in: [style, family, concept, walk] }
anchors:
  style: art/style_anchor.png
  family: art/family_{{family}}.png
invariants: >
  三头身 Q 版比例，赛璐璐上色。
slots: [id, name, element, family]
params: { size: 1024, reroll: 2, seed_policy: reroll_jitter, concurrency: 2 }
check:
  program: ["exists", "path_template", "format", "dimensions"]
  judge: ["四肢交替相位正确且可循环"]
  human: ["动作张力"]
`

func TestParseRecipeValid(t *testing.T) {
	r, err := ParseRecipe([]byte(recipeFixture))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if r.ID != "beast_motion" || r.Version != 3 {
		t.Fatalf("id/version 不符: %+v", r)
	}
	if len(r.Stages) != 3 || r.Stages[1].ID != "walk" {
		t.Fatalf("stages 解析不符: %+v", r.Stages)
	}
	if !r.Stages[0].AnchorOut || len(r.Stages[2].AnchorIn) != 4 {
		t.Fatalf("锚点声明解析不符: %+v", r.Stages)
	}
	if r.RerollLimit() != 2 {
		t.Fatalf("reroll 应为 2, got %d", r.RerollLimit())
	}
	if len(r.ProgramChecks()) != 4 {
		t.Fatalf("program checks 应为 4")
	}
}

func TestParseRecipeRejects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"缺 id", "tool: t\ndeliverable: {type: image}\nstages: [{id: a}]", "id"},
		{"缺 tool", "id: x\ndeliverable: {type: image}\nstages: [{id: a}]", "tool"},
		{"未知交付形态", "id: x\ntool: t\ndeliverable: {type: gizmo}\nstages: [{id: a}]", "不支持"},
		{"空 stages", "id: x\ntool: t\ndeliverable: {type: image}", "stages"},
		{"重复阶段", "id: x\ntool: t\ndeliverable: {type: image}\nstages: [{id: a}, {id: a}]", "重复"},
		{"未知校验器", "id: x\ntool: t\ndeliverable: {type: image}\nstages: [{id: a}]\ncheck: {program: [magic]}", "未知校验器"},
		{"路径穿越", "id: x\ntool: t\ndeliverable: {type: image, path: \"../out/\"}\nstages: [{id: a}]", "路径"},
		{"非法阶段 id", "id: x\ntool: t\ndeliverable: {type: image}\nstages: [{id: \"a b\"}]", "非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRecipe([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("应拒绝但通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestSaveRecipeYAMLVersionBump(t *testing.T) {
	dir := t.TempDir()
	// 首次落盘：version 0 → 1。
	r1, err := SaveRecipeYAML(dir, strings.Replace(recipeFixture, "version: 3", "version: 0", 1))
	if err != nil {
		t.Fatalf("首次落盘失败: %v", err)
	}
	if r1.Version != 1 {
		t.Fatalf("新文件版本应为 1, got %d", r1.Version)
	}
	// 二次落盘同版本 → 自动 +1。
	r2, err := SaveRecipeYAML(dir, strings.Replace(recipeFixture, "version: 3", "version: 1", 1))
	if err != nil {
		t.Fatalf("二次落盘失败: %v", err)
	}
	if r2.Version != 2 {
		t.Fatalf("已存在文件应自动 +1 → 2, got %d", r2.Version)
	}
	// 高版本保留。
	r3, err := SaveRecipeYAML(dir, strings.Replace(recipeFixture, "version: 3", "version: 9", 1))
	if err != nil {
		t.Fatalf("三次落盘失败: %v", err)
	}
	if r3.Version != 9 {
		t.Fatalf("更高版本应保留, got %d", r3.Version)
	}
	// 目录内文件齐备且可再读。
	loaded, err := LoadRecipe(dir, "beast_motion")
	if err != nil || loaded.Version != 9 {
		t.Fatalf("回读失败: %v version=%d", err, loaded.Version)
	}
	if _, err := os.Stat(filepath.Join(WorkflowsDir(dir), "beast_motion.yaml")); err != nil {
		t.Fatalf("文件不存在: %v", err)
	}
}

func TestListRecipesAndSummary(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveRecipeYAML(dir, recipeFixture); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	// 放一个损坏文件：列表不整体失败，该项带 error。
	if err := os.WriteFile(filepath.Join(WorkflowsDir(dir), "broken.yaml"), []byte("::: not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ListRecipes(dir)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 项, got %d", len(got))
	}
	if got[0].ID != "beast_motion" || got[0].Stages != 3 || got[0].Error != "" {
		t.Fatalf("beast_motion 摘要不符: %+v", got[0])
	}
	if got[1].ID != "broken" || got[1].Error == "" {
		t.Fatalf("broken 应带错误摘要: %+v", got[1])
	}
}

func TestListRecipesMissingDir(t *testing.T) {
	got, err := ListRecipes(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(got) != 0 {
		t.Fatalf("目录不存在应返回空列表: %v %v", got, err)
	}
}

func TestExpandTemplateAndOutputPath(t *testing.T) {
	item := RosterItem{ID: "ice_dragon", Name: "冰霜巨龙", Fields: map[string]string{"family": "dragon"}}
	deliv := Deliverable{Type: "sprite_sheet", Format: "png", Path: "assets/units/{{id}}/{{stage}}/"}
	got := expectedOutputPath(deliv, item, "walk", 1)
	want := "assets/units/ice_dragon/walk/ice_dragon_walk.png"
	if got != want {
		t.Fatalf("路径模板展开不符: got %q want %q", got, want)
	}
	// 重摇 attempt≥2 加后缀（不覆盖被否 attempt 的产物）。
	if got := expectedOutputPath(deliv, item, "walk", 2); !strings.Contains(got, "ice_dragon_walk_a2.png") {
		t.Fatalf("重摇产物应带 _a2 后缀: %q", got)
	}
	// family 槽位替换。
	anchors := expandTemplate("art/family_{{family}}.png", item, "walk")
	if anchors != "art/family_dragon.png" {
		t.Fatalf("槽位替换不符: %q", anchors)
	}
	// 模板直接给文件名时原样使用。
	deliv2 := Deliverable{Type: "image", Format: "png", Path: "out/hero.png"}
	if got := expectedOutputPath(deliv2, item, "concept", 1); got != "out/hero.png" {
		t.Fatalf("文件名模板应原样: %q", got)
	}
}
