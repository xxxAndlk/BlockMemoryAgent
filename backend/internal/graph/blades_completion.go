package graph

// 本文件承载 blades 工具循环的"完成门控"辅助函数。
// 职责：检测 LLM 是否在最终输出中声称已写入文件，但实际上没有成功的 WriteFile 工具调用。

import (
	"strings"
)

// outputClaimsWriteFile 判断 LLM 输出文本是否声称已完成文件写入。
//
// 设计意图：作为完成门控的触发条件。只有 LLM 自己在最终回答中明确说"已写/已创建"时，
// 我们才检查它是否真的调用了 WriteFile；避免对纯问答/读文件类任务误触发。
//
// 参数：text - LLM 最终输出文本。
// 返回：true 表示文本包含写文件相关声明。
func outputClaimsWriteFile(text string) bool {
	lower := strings.ToLower(text)
	claims := []string{
		"已写", "已写入", "已创建", "已生成", "已保存", "已落盘",
		"wrote", "written", "created", "saved", "generated",
	}
	for _, c := range claims {
		if strings.Contains(lower, c) {
			return true
		}
	}
	return false
}

// hasWriteFileResult 检查工具结果中是否已有成功的 WriteFile。
//
// 职责：扫描工具结果列表，确认是否存在成功的 WriteFile 调用记录。
// 参数：results - 本次 agent 执行产生的全部工具结果。
// 返回：存在成功 WriteFile 时返回 true。
func hasWriteFileResult(results []*ToolResult) bool {
	for _, r := range results {
		if r.Tool == "WriteFile" && r.Success {
			return true
		}
	}
	return false
}
