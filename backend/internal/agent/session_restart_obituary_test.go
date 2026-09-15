// session_restart_obituary_test.go 重启讣告测试（TODO 第16项 T19）：
// 派发事件优先 > 工具调用 > 用户输入 > 空事件流静默；detail_json.domain 拼入展示名。
package agent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

func TestRestartObituary(t *testing.T) {
	cases := []struct {
		name    string
		events  []internalEvent
		wantSub string // 期望讣告包含的子串
	}{
		{
			name: "最后一条子任务派发优先",
			events: []internalEvent{
				{Type: eventkind.ToolCall, Tool: "ReadFile", ToolPath: "a.go"},
				{Type: eventkind.Message, Message: "实现登录页", Kind: "sub_agent_dispatch",
					Tool: "ui_assistant", DetailJSON: `{"domain":"前端工程师"}`},
			},
			wantSub: "派发子任务: 「前端工程师」实现登录页",
		},
		{
			name:    "无派发回落最后一次工具调用",
			events:  []internalEvent{{Type: eventkind.ToolCall, Tool: "EditFile", ToolPath: "main.go"}},
			wantSub: "最后动作: EditFile → main.go",
		},
		{
			name:    "无工具回落最后一条用户消息",
			events:  []internalEvent{{Type: eventkind.UserMessage, Message: "把首页改成暗色主题"}},
			wantSub: "最后输入: 把首页改成暗色主题",
		},
		{
			name:    "空事件流静默",
			events:  nil,
			wantSub: "",
		},
		{
			name: "最新工具调用优先于更早的",
			events: []internalEvent{
				{Type: eventkind.ToolCall, Tool: "SearchInFiles"},
				{Type: eventkind.ToolCall, Tool: "RunCommand", ToolPath: "go test ./..."},
			},
			wantSub: "最后动作: RunCommand → go test ./...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restartObituary(tc.events)
			if tc.wantSub == "" {
				if got != "" {
					t.Fatalf("obituary = %q, want 空串", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("obituary = %q, want 包含 %q", got, tc.wantSub)
			}
		})
	}
}
