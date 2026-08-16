package tool

// destructive.go 破坏性工具分级 + 生产边界确认（TODO #17 P1，AICP「破坏性操作权限隔离」轻量版）。
//
// 分级：
//   - 工具静态标记：WriteFile 实现 DestructiveTool 接口（Destructive()=true）。
//   - RunCommand 动态判定：命中危险命令模式（git push / rm -rf / drop table 等）恒为破坏性，
//     与工作目录无关；生产目录下写类命令（rm/mv/cp/touch/mkdir 等）亦为破坏性。
//
// 边界：
//   - production_workdir 配置非空且工具工作目录位于其下时，破坏性调用经 Registry.approvalHook
//     推「需确认」事件，上层暂停会话等用户确认；非生产目录照常自主。
//   - 比 AICP 双签轻：只在边界触发，不阻塞常规编码流。

import (
	"context"
	"encoding/json"
	"strings"
)

// DestructiveTool 是工具的可选接口：标记该工具为破坏性操作（不可逆 / 影响共享状态）。
// 命中生产边界（production_workdir）时需用户确认后才执行；未实现该接口的工具视为非破坏性。
type DestructiveTool interface {
	Destructive() bool
}

// ApprovalHookFunc 是破坏性操作的用户确认回调：返回 true 放行、false 拒绝、error 中止。
// nil（默认）= 全放行，零行为变化。由上层（ReactService）注入：命中边界时暂停会话
// 推「需确认」事件，等用户答复后把结果作为本次工具调用的裁决返回。
type ApprovalHookFunc func(ctx context.Context, toolName string, args map[string]any) (bool, error)

// dangerousCommandPatterns 是 RunCommand 的危险命令模式（小写子串匹配）。
// 命中即视为破坏性操作，无论工作目录是否生产环境，均要求用户确认。
// 覆盖方向：远端/共享状态影响（git push/merge）、不可逆删除（rm -rf/drop table）、
// 系统级变更（shutdown/format）、包发布（npm publish）。fail-safe 方向：误匹配只多一次确认。
var dangerousCommandPatterns = []string{
	// git 远端/破坏性操作
	"git push", "git merge", "git rebase", "git reset", "git clean", "git checkout -f", "git stash drop",
	// 不可逆删除
	"rm -rf", "rm -r ", "rm -f ", "rmdir /s", "rmdir /q", "rd /s ", "deltree", "del /s", "del /f", "erase /f", "remove-item -recurse", "remove-item -force",
	// 系统级
	"format ", "diskpart", "shutdown", "restart-computer", "stop-computer",
	"drop database", "drop table", "drop schema", "truncate table",
	// 包发布/安装
	"make install", "make uninstall", "npm publish", "npm unpublish", "pip uninstall",
	// 进程/账号/注册表强操作
	"kill -9", "taskkill /f", "net user", "net localgroup", "sc delete", "reg delete", "reg add hklm",
	"curl --request delete", "curl -x delete", "invoke-webrequest -method delete",
}

// isDangerousCommand 判断 RunCommand 命令是否命中危险模式（小写子串匹配）。
// 空命令返回 false。
func isDangerousCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	if c == "" {
		return false
	}
	for _, p := range dangerousCommandPatterns {
		if strings.Contains(c, p) {
			return true
		}
	}
	return false
}

// inProductionWorkDir 判断工具工作目录是否位于生产环境目录（production_workdir）之下。
// production 为空（未配置）或 workDir 为空时返回 false（不启用生产边界确认）。
func inProductionWorkDir(workDir, production string) bool {
	if production == "" || workDir == "" {
		return false
	}
	workDir = strings.TrimSuffix(workDir, "/")
	workDir = strings.TrimSuffix(workDir, "\\")
	production = strings.TrimSuffix(production, "/")
	production = strings.TrimSuffix(production, "\\")
	if workDir == production {
		return true
	}
	return strings.HasPrefix(workDir, production+"/") || strings.HasPrefix(workDir, production+"\\")
}

// ApprovalMessage 生成推给用户的确认文案（导出供会话层推送「需确认」事件）。
// 文案必须包含工具名与关键参数，让用户能判断"要确认的是什么操作"：
// 已知工具取关键参数（文件路径/命令），其余工具（如插件工具）附完整参数 JSON（截断）。
func ApprovalMessage(toolName string, args map[string]any) string {
	desc := "工具 " + toolName + " 为破坏性操作"
	switch toolName {
	case "WriteFile", "EditFile":
		if p, _ := args["path"].(string); p != "" {
			desc = "将写入文件 " + p
		}
	case "RunCommand":
		if c, _ := args["command"].(string); c != "" {
			desc = "将执行命令 " + truncate(c, 120)
		}
	default:
		if b, err := json.Marshal(args); err == nil && len(args) > 0 {
			desc += "，参数: " + truncate(string(b), 160)
		}
	}
	return "【需确认】" + desc + "，且当前处于生产环境/命中危险命令模式。回复「确认」执行，「拒绝」取消；其余答复按拒绝处理。"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
