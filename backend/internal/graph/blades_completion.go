package graph

// 本文件承载 blades 工具循环的"完成门控"辅助函数。
// 从 llm_tools.go 拆出（P0-3）：判断任务是否要求写文件 + 检查是否已有成功 WriteFile。

import (
	"strings"
)

// taskRequiresWriteFile 判断任务是否要求创建/写入文件。
// 用于"完成门控"：此类任务必须见到成功的 WriteFile 才算完成。
//
// 职责：基于关键词匹配判断任务是否涉及文件创建/写入动作。
// 参数：
//   - task：任务描述文本。
//
// 返回：true 表示该任务必须通过 WriteFile 工具落盘才算完成。
// 副作用：无。
// 并发安全：纯函数。
func taskRequiresWriteFile(task string) bool {
	// 统一小写以做大小写不敏感匹配
	lower := strings.ToLower(task)
	// 动词集合：表示"创建/写入"类动作
	verbs := []string{
		"写", "创建", "生成", "实现", "编写", "开发", "保存", "落盘",
		"write", "create", "generate", "implement", "build", "save",
	}
	// 文件类名词/扩展名集合：表示产出物是文件/代码
	fileHints := []string{
		"文件", "代码", "脚本", "程序", "游戏", "页面", "demo", "示例",
		"接口", "配置", "服务", "模块", "库", "工具",
		"file", "code", "script", "program", "game", "page", "server", "api", "config", "module", "lib",
		".go", ".py", ".js", ".ts", ".html", ".css", ".md", ".json", ".yaml", ".yml", ".sql",
	}
	// 第一步：必须命中至少一个写入类动词
	hasVerb := false
	for _, v := range verbs {
		if strings.Contains(lower, v) {
			hasVerb = true
			break
		}
	}
	if !hasVerb {
		return false // 无写入动词，不算写文件任务
	}
	// 第二步：必须同时命中文件类名词或扩展名
	for _, h := range fileHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// hasWriteFileResult 检查工具结果中是否已有成功的 WriteFile。
//
// 职责：扫描工具结果列表，确认是否存在成功的 WriteFile 调用记录。
// 参数：
//   - results：本次 agent 执行产生的全部工具结果。
//
// 返回：存在成功 WriteFile 时返回 true。
// 副作用：无。
// 并发安全：只读切片，安全。
func hasWriteFileResult(results []*ToolResult) bool {
	for _, r := range results {
		// 工具名为 WriteFile 且 Success 为 true 才算数
		if r.Tool == "WriteFile" && r.Success {
			return true
		}
	}
	return false
}
