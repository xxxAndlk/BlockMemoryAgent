package agent

// gear_selector.go 规则选档器（TODO #14 会话三档控制，自动档 auto 的决策内核）：
// 明确闲聊信号（短句、无路径/文件扩展名、无工程动词）→ fast；其余一律 cluster。
// 设计原则零回归：不确定的任务走 cluster（现行为），宁可多等不可浅答——TODO 原文
// 明确警告"选快档得浅答案"比"多等一会"更伤。误判兜底：手动切档任意向 + escalate
// 自动升档（fast→cluster 只升不降）。

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// maxChattyGoalRunes 闲聊目标长度上限（rune）：超过即不可能是一句话问答/快问快答。
const maxChattyGoalRunes = 40

// fileExtRe 文件扩展名特征（".go" ".docx" "3.5" 等）：点号后跟 1-6 位字母数字。
// 命中视为任务载体特征 → cluster。
var fileExtRe = regexp.MustCompile(`\.[A-Za-z0-9]{1,6}\b`)

// actionVerbs 工程/行动动词表（中英，子串匹配）：出现任一即视为任务型目标 → cluster。
// 刻意从宽——误命中把闲聊判成 cluster 只是多等一会（安全向）；
// 漏判把任务判成 fast 会得到浅答案（伤害向，绝不放过）。
var actionVerbs = []string{
	// 中文：写改修删建 + 构建运维链路
	"写", "改", "修", "删", "建", "实现", "开发", "重构", "修复", "构建", "编译",
	"部署", "发布", "运行", "执行", "测试", "排查", "诊断", "调试", "分析", "生成",
	"添加", "新增", "迁移", "升级", "接入", "对接", "安装", "配置", "爬取", "抓取",
	"翻译", "总结", "搜索", "检索", "统计", "画", "做", "调", "改一下", "优化",
	// 英文：子串匹配（误命中如 pruning → cluster，安全向）
	"write", "fix", "build", "compile", "deploy", "run", "test", "refactor",
	"implement", "create", "add ", "delete", "remove", "update", "migrate",
	"install", "generate", "analyze", "parse", "debug", "search", "translate",
	"summarize", "optimiz", "config",
}

// SelectGear 按任务目标文本规则选档（TODO #14）：明确闲聊信号 → fast，其余 cluster。
// 仅在会话档位为 auto（含未设置）时由 runSession 每轮调用；固化 fast 后的后续轮
// 允许升 cluster（只升不降由调用方裁决），cluster 恒保持。
func SelectGear(goal string) string {
	if isChattyGoal(goal) {
		return tool.GearFast
	}
	return tool.GearCluster
}

// isChattyGoal 判定目标文本是否为闲聊/快问快答：短（≤maxChattyGoalRunes）∧
// 无路径/URL/文件扩展名 ∧ 无工程动词。
func isChattyGoal(goal string) bool {
	g := strings.TrimSpace(goal)
	if g == "" || utf8.RuneCountInString(g) > maxChattyGoalRunes {
		return false
	}
	// 路径分隔符 / URL：任务载体特征。
	if strings.ContainsAny(g, "/\\") || strings.Contains(g, "http:") || strings.Contains(g, "https:") {
		return false
	}
	// 文件扩展名（含 "3.5" 这类数字点号）：宁可误判 cluster。
	if fileExtRe.MatchString(g) {
		return false
	}
	lower := strings.ToLower(g)
	for _, v := range actionVerbs {
		if strings.Contains(lower, v) {
			return false
		}
	}
	return true
}
