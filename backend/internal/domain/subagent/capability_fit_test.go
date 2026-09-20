package subagent

// 派前能力校验（P0-2e，2026-09-18 实测修复）单测：任务文本工具需求信号 × 角色
// 工具面匹配。覆盖 checkRoleTaskFit 与 roleHasTool 的判定矩阵——
// 实测场景：meta 给 scout 派"用 PowerShell Get-Content 读文件"，3 个 scout 全无
// RunCommand 一波全灭；该校验在 dispatchOne 入口零消耗拒绝并提示改派方向。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestRoleHasTool(t *testing.T) {
	// 空工具表 = 全量暴露（domain 等未声明 tools 的角色视为具备一切核心工具）。
	full := &types.RoleDefinition{ID: "domain"}
	if !roleHasTool(full, "RunCommand") || !roleHasTool(full, "WriteFile") {
		t.Fatal("空工具表角色应视为全量暴露")
	}
	// 显式白名单：大小写不敏感匹配。
	scout := &types.RoleDefinition{ID: "scout", Tools: []string{"ReadFile", "ListDir", "SearchInFiles"}}
	if roleHasTool(scout, "ReadFile") != true {
		t.Fatal("白名单内工具应判定具备")
	}
	if roleHasTool(scout, "RunCommand") {
		t.Fatal("scout 无 RunCommand 应判定不具备")
	}
	if roleHasTool(scout, "writefile") {
		t.Fatal("大小写不敏感：scout 无 WriteFile 应判定不具备")
	}
	// 具备执行+写能力的角色放行。
	light := &types.RoleDefinition{ID: "light", Tools: []string{"ReadFile", "WriteFile", "EditFile", "RunCommand"}}
	if !roleHasTool(light, "RunCommand") || !roleHasTool(light, "WriteFile") {
		t.Fatal("light 同时具备 RunCommand/WriteFile")
	}
}

func TestCheckRoleTaskFit(t *testing.T) {
	scout := &types.RoleDefinition{ID: "scout", Tools: []string{"ReadFile", "ListDir", "SearchInFiles"}}
	codeAssistant := &types.RoleDefinition{ID: "code_assistant", Tools: []string{"ReadFile", "WriteFile", "EditFile", "RunCommand", "SearchInFiles"}}
	domain := &types.RoleDefinition{ID: "domain"} // 空表 = 全量

	cases := []struct {
		name    string
		role    *types.RoleDefinition
		task    string
		wantErr string // 空 = 应通过
	}{
		{"scout+powershell命令", scout, "用 PowerShell Get-Content 读取 D:\\x\\y.go 并分析", "RunCommand"},
		{"scout+中文命令信号", scout, "请执行命令统计该目录文件数", "RunCommand"},
		{"scout+go测试指令", scout, "对该模块跑 go test ./... 并回报失败用例", "RunCommand"},
		{"scout+写文件要求", scout, "把审查结果写入文件 findings.md", "WriteFile"},
		{"scout+纯定位任务放行", scout, "用 SearchInFiles 定位 handleToolEvent 的定义文件与行号", ""},
		{"code_assistant+命令任务放行", codeAssistant, "用 PowerShell Get-Content 读取并修复", ""},
		{"code_assistant+写文件任务放行", codeAssistant, "把结果写入文件 report.md", ""},
		{"domain+任何任务放行", domain, "用 PowerShell 读取并写入文件再 go test", ""},
		{"scout+URL无信号放行", scout, "分析 https://example.com/a.go 的设计思路", ""},
	}
	for _, c := range cases {
		got := checkRoleTaskFit(c.role, c.task)
		if c.wantErr == "" {
			if got != "" {
				t.Fatalf("%s：期望通过，实际拒绝：%s", c.name, got)
			}
			continue
		}
		if got == "" {
			t.Fatalf("%s：期望拒绝（缺 %s），实际通过", c.name, c.wantErr)
		}
		if !strings.Contains(got, c.wantErr) {
			t.Fatalf("%s：拒绝文案应点名 %s，实际：%s", c.name, c.wantErr, got)
		}
		if !strings.Contains(got, "改派") {
			t.Fatalf("%s：拒绝文案应给出改派方向，实际：%s", c.name, got)
		}
	}
}
