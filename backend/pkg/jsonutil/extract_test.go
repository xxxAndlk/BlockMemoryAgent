package jsonutil

import (
	"encoding/json"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	fullOpts := ExtractOptions{
		StripComments:     true,
		FixSingleQuotes:   true,
		FixTrailingCommas: true,
		AllowArray:        true,
	}

	cases := []struct {
		name     string
		input    string
		opts     ExtractOptions
		validate func(t *testing.T, got string)
	}{
		{
			name:  "plain object",
			input: `{"name":"db","goal":"check"}`,
			opts:  fullOpts,
			validate: func(t *testing.T, got string) {
				if got != `{"name":"db","goal":"check"}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "markdown code block",
			input: "Some text\n```json\n{\"name\":\"db\"}\n```\nmore text",
			opts:  fullOpts,
			validate: func(t *testing.T, got string) {
				if got != `{"name":"db"}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "single quotes",
			input: "```json\n{'name':'db','goal':'check'}\n```",
			opts:  fullOpts,
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
			opts:  fullOpts,
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
			opts:  fullOpts,
			validate: func(t *testing.T, got string) {
				var v []map[string]string
				if err := json.Unmarshal([]byte(got), &v); err != nil {
					t.Fatalf("parse failed: %v", got)
				}
			},
		},
		{
			name:  "empty options object only",
			input: `{"passed":true}`,
			opts:  ExtractOptions{},
			validate: func(t *testing.T, got string) {
				if got != `{"passed":true}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "empty options markdown",
			input: "```json\n{\"passed\":false}\n```",
			opts:  ExtractOptions{},
			validate: func(t *testing.T, got string) {
				if got != `{"passed":false}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
		{
			name:  "empty options with surrounding text",
			input: `ok {"passed":true} done`,
			opts:  ExtractOptions{},
			validate: func(t *testing.T, got string) {
				if got != `{"passed":true}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractJSON(c.input, c.opts)
			c.validate(t, got)
		})
	}
}
