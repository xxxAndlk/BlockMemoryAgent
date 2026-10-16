package workflow

// roster_test.go 名册解析（内联 JSON / YAML 文件 / stages 裁剪 / 去重）单元测试。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRosterInlineJSON(t *testing.T) {
	raw := `[{"id":"ice_dragon","name":"冰霜巨龙","element":"ice","family":"dragon","stages":["concept","walk"]},
	         {"id":"flame_turret","name":"烈焰炮台","element":"fire","body_plan":"三足"}]`
	items, err := ParseRoster(raw, t.TempDir())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 件, got %d", len(items))
	}
	if items[0].Name != "冰霜巨龙" || items[0].Fields["element"] != "ice" || items[0].Fields["family"] != "dragon" {
		t.Fatalf("首件字段不符: %+v", items[0])
	}
	if len(items[0].Stages) != 2 || items[0].Stages[0] != "concept" {
		t.Fatalf("stages 裁剪不符: %+v", items[0].Stages)
	}
	if items[1].Fields["body_plan"] != "三足" {
		t.Fatalf("任意槽位应进 Fields: %+v", items[1].Fields)
	}
}

func TestParseRosterWrapperAndFile(t *testing.T) {
	dir := t.TempDir()
	yml := `
roster:
  - { id: frost_wolf, name: 霜狼, element: ice, family: wolf }
  - { id: flame_turret, name: 烈焰炮台, stages: [concept, attack, vfx] }
`
	path := filepath.Join(WorkflowsDir(dir), "roster_demo.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := ParseRoster(".bma/workflows/roster_demo.yaml", dir)
	if err != nil {
		t.Fatalf("文件名册解析失败: %v", err)
	}
	if len(items) != 2 || items[0].ID != "frost_wolf" {
		t.Fatalf("名册不符: %+v", items)
	}
	// 顶层数组形态。
	path2 := filepath.Join(WorkflowsDir(dir), "roster_arr.yaml")
	if err := os.WriteFile(path2, []byte("- {id: a}\n- {id: b}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if items, err := ParseRoster(".bma/workflows/roster_arr.yaml", dir); err != nil || len(items) != 2 {
		t.Fatalf("顶层数组名册解析失败: %v %+v", err, items)
	}
}

func TestParseRosterRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空", ""},
		{"缺 id", `[{"name":"无名"}]`},
		{"重复 id", `[{"id":"a"},{"id":"a"}]`},
		{"非法 id", `[{"id":"a b"}]`},
		{"文件不存在", ".bma/workflows/nope.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRoster(tc.raw, t.TempDir()); err == nil {
				t.Fatalf("应拒绝但通过了: %s", tc.raw)
			}
		})
	}
}

func TestValidateRosterAgainstRecipe(t *testing.T) {
	r, err := ParseRecipe([]byte(recipeFixture))
	if err != nil {
		t.Fatal(err)
	}
	ok := []RosterItem{{ID: "a", Stages: []string{"concept", "walk"}}, {ID: "b"}}
	if err := validateRosterAgainstRecipe(ok, r); err != nil {
		t.Fatalf("合法名册被拒: %v", err)
	}
	bad := []RosterItem{{ID: "a", Stages: []string{"concept", "fly"}}}
	if err := validateRosterAgainstRecipe(bad, r); err == nil {
		t.Fatalf("引用不存在阶段应被拒")
	}
}

func TestRosterItemJSONRoundTrip(t *testing.T) {
	raw := `{"id":"ice_dragon","name":"冰霜巨龙","element":"ice","stages":["walk"]}`
	var item RosterItem
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	out, err := item.MarshalJSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back RosterItem
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.ID != item.ID || back.Fields["element"] != "ice" || len(back.Stages) != 1 {
		t.Fatalf("往返不符: %+v → %s → %+v", item, out, back)
	}
}
