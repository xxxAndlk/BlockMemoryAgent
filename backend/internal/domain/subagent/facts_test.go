package subagent

import (
	"testing"
)

func TestParseFactsJSON_PlainArray(t *testing.T) {
	got, err := parseFactsJSON(`["fact1","fact2","fact3"]`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != "fact1" || got[2] != "fact3" {
		t.Errorf("got = %v", got)
	}
}

func TestParseFactsJSON_MarkdownFence(t *testing.T) {
	in := "```json\n[\"a\", \"b\"]\n```"
	got, err := parseFactsJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got = %v", got)
	}
}

func TestParseFactsJSON_BareFence(t *testing.T) {
	in := "```\n[\"x\"]\n```"
	got, err := parseFactsJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("got = %v", got)
	}
}

func TestParseFactsJSON_SurroundingText(t *testing.T) {
	in := "Here are the facts:\n[\"a\",\"b\"]\nThat's all."
	got, err := parseFactsJSON(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestParseFactsJSON_Empty(t *testing.T) {
	if _, err := parseFactsJSON(""); err == nil {
		t.Error("expected err for empty input")
	}
	if _, err := parseFactsJSON("   "); err == nil {
		t.Error("expected err for whitespace-only input")
	}
}

func TestParseFactsJSON_NoArray(t *testing.T) {
	if _, err := parseFactsJSON("just text, no array"); err == nil {
		t.Error("expected err for no-array input")
	}
}

func TestParseFactsJSON_EmptyArray(t *testing.T) {
	got, err := parseFactsJSON("[]")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestParseFactsJSON_FiltersEmptyStrings(t *testing.T) {
	got, err := parseFactsJSON(`["real", "", "  ", "end"]`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (filtered)", len(got))
	}
	if got[0] != "real" || got[1] != "end" {
		t.Errorf("got = %v", got)
	}
}

func TestParseFactsJSON_UnicodeContent(t *testing.T) {
	got, err := parseFactsJSON(`["用户偏好公制单位", "项目使用 Go 1.25"]`)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != "用户偏好公制单位" {
		t.Errorf("got[0] = %q", got[0])
	}
}
