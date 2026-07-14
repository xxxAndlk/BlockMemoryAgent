package jsonutil

import (
	"encoding/json"
	"testing"
)

// TestExtractJSON 验证 ExtractJSON 在不同输入与选项组合下的行为，
// 包括普通对象、markdown 代码块、单引号、尾部逗号、注释、空选项等场景。
func TestExtractJSON(t *testing.T) {
	// 完整选项：启用所有修复能力。
	fullOpts := ExtractOptions{
		StripComments:     true,
		FixSingleQuotes:   true,
		FixTrailingCommas: true,
		AllowArray:        true,
	}

	// 测试用例集合，每个用例包含名称、输入、选项与校验函数。
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
				// 普通对象无需修复，应原样返回。
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
				// 应剥离围栏并返回 JSON 主体。
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
				// 单引号边界应被替换为双引号，使 JSON 可解析。
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
				// 尾部逗号应被移除，数组应包含两个元素。
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
				// 注释应被移除，数组可正常解析。
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
				// 空选项下只提取对象，不做额外修复，结果应原样返回。
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
				// 空选项仍应剥离 markdown 围栏。
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
				// 空选项下从文本中提取 JSON 对象。
				if got != `{"passed":true}` {
					t.Fatalf("unexpected: %s", got)
				}
			},
		},
	}

	// 遍历所有用例，使用 t.Run 生成子测试。
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 调用被测函数。
			got := ExtractJSON(c.input, c.opts)
			// 执行用例自定义校验。
			c.validate(t, got)
		})
	}
}
