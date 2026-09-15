package tool

import "testing"

// TestBareToolName 验证 MCP 插件前缀剥离：带 "<插件ID>__" 前缀的本地注册名还原为
// 远端原始名；无前缀名（内置工具/改名前历史记录）原样返回。
func TestBareToolName(t *testing.T) {
	cases := map[string]string{
		"ui_preview__browser_navigate":        "browser_navigate",
		"host_computer_use__browser_find":     "browser_find",
		"ui_design__od_image_generate":        "od_image_generate",
		"browser_navigate":                    "browser_navigate", // 无前缀原样
		"RunCommand":                          "RunCommand",
		"":                                    "",
		"plug__":                              "plug__", // 前缀后为空不脱壳（防御）
		"host_computer_use__computer_use_gui": "computer_use_gui",
	}
	for in, want := range cases {
		if got := BareToolName(in); got != want {
			t.Errorf("BareToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
