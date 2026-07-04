package graph

import (
	"encoding/json"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		validate func(t *testing.T, got string)
	}{
		{
			name:  "plain object",
			input: `{"name":"db","goal":"check"}`,
			validate: func(t *testing.T, got string) {
				if got != `{"name":"db","goal":"check"}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "markdown code block",
			input: "Some text\n```json\n{\"name\":\"db\"}\n```\nmore text",
			validate: func(t *testing.T, got string) {
				if got != `{"name":"db"}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "single quotes",
			input: "```json\n{'name':'db','goal':'check'}\n```",
			validate: func(t *testing.T, got string) {
				var v map[string]string
				if err := json.Unmarshal([]byte(got), &v); err != nil {
					t.Fatalf("parse failed: %v", got)
				}
				if v["name"] != "db" {
					t.Fatalf("unexpected value: %v", v)
				}
			},
		},
		{
			name:  "trailing comma",
			input: `[{"name":"db",},{"name":"ui",}]`,
			validate: func(t *testing.T, got string) {
				var v []map[string]string
				if err := json.Unmarshal([]byte(got), &v); err != nil {
					t.Fatalf("parse failed: %v", got)
				}
				if len(v) != 2 {
					t.Fatalf("expected 2 items, got %d", len(v))
				}
			},
		},
		{
			name:  "comments",
			input: "// leading comment\n[{\"name\":\"db\"}] /* trailing */",
			validate: func(t *testing.T, got string) {
				var v []map[string]string
				if err := json.Unmarshal([]byte(got), &v); err != nil {
					t.Fatalf("parse failed: %v", got)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractJSON(c.input)
			c.validate(t, got)
		})
	}
}
