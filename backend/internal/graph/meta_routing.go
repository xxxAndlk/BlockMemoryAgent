package graph

import (
	"strings"
	"unicode/utf8"
)


// shouldDirectExecute 判断是否为"查询/搜索/分析"类简单任务，
// 这类任务应跳过 DomainAgent 子任务拆解，直接把 goal 作为单个子任务交给
// 一个 Assistant 用 HTTPGet / Web 搜索类工具完成，而非写脚本。
//
// 判定规则：命中查询/搜索/资讯关键词，且未命中"创建/编写/运行/实现"等
// 明确需要落盘或执行代码的动作词。
func (n *MetaAgentNode) shouldDirectExecute(goal string) bool {
	if goal == "" {
		return false
	}
	goalLower := strings.ToLower(goal)

	// 查询/搜索/资讯类关键词
	queryPatterns := []string{
		"查询", "查一下", "查下", "了解", "获取", "搜集", "收集",
		"搜索", "搜一下", "搜索一下", "检索", "查找",
		"新闻", "资讯", "行情", "股价", "股票", "汇率", "天气",
		"最新", "近期", "最近", "今天", "昨日", "当前",
		"原因", "为什么", "为何", "怎么样", "如何看",
		"分析", "解读", "总结", "汇总",
		"是什么", "什么是", "介绍一下", "解释",
		"news", "search", "query", "lookup", "find", "latest", "recent", "today",
	}
	hitQuery := false
	for _, p := range queryPatterns {
		if strings.Contains(goalLower, p) {
			hitQuery = true
			break
		}
	}
	if !hitQuery {
		return false
	}

	// 明确需要写代码/运行程序的动作词：命中则不视为直接执行类
	actionPatterns := []string{
		"写一个", "写个", "编写", "实现", "开发", "创建一个", "创建个",
		"生成", "制作", "搭建", "部署",
		"运行", "执行", "启动", "跑一下",
		"修改", "重构", "优化代码", "修复",
		"贪吃蛇", "小游戏", "游戏",
	}
	for _, p := range actionPatterns {
		if strings.Contains(goalLower, p) {
			return false
		}
	}
	return true
}

// isSimpleQuestion 判断是否为简单直接问题（仅寒暄/自我介绍类）。
//
// 职责：
//   - 命中指代/历史/操作类关键词 → 非简单问题（需走完整 Graph）
//   - 命中明确寒暄/自我介绍模式 → 简单问题
//   - 极短且纯 ASCII（如 "ping"）→ 简单问题
//   - 中文短句一律不视为简单问题
//
// 参数：
//   - goal：用户目标
//
// 返回：是简单问题返回 true。
//
// 设计意图：之前的实现用 len(goal) < 30 字节判定，对中文极不靠谱——
// "贪吃蛇小游戏在哪" 这种指代类问题（8 汉字 = 24 字节）会被误判成
// 简单问题，直接跳过 Graph 走 LLM 一问一答，既不读历史也不调工具，
// 表现为"Agent 失忆"。
//
// 现在：必须命中明确的寒暄模式，并且不含任何指代/查找/操作词。
func (n *MetaAgentNode) isSimpleQuestion(goal string) bool {
	// 转小写做大小写不敏感匹配
	goalLower := strings.ToLower(goal)

	// 指代/历史/操作类关键词：命中即视为非简单问题，需走完整 Graph
	referencePatterns := []string{
		"在哪", "哪里", "刚才", "上次", "之前", "上次", "之前", "那个",
		"这个", "刚才", "记得", "记忆", "历史", "之前",
		"文件", "代码", "目录", "项目", "找", "查找", "搜索",
		"写", "创建", "修改", "删除", "运行", "执行",
	}
	for _, p := range referencePatterns {
		// 命中任一指代词即视为非简单问题
		if strings.Contains(goalLower, p) {
			return false
		}
	}

	// 仅在命中明确寒暄/自我介绍模式时才视为简单问题。
	// hello/hi/hey 必须作为整词（前后非字母数字）匹配，避免 "创建hello.txt"
	// / "hey-check 工具" 这类实际任务被误判为寒暄。
	simplePatterns := []string{
		"你是什么", "你是谁", "什么模型", "你好",
		"叫什么名字", "介绍自己", "自我介绍", "能做什么", "有什么功能",
	}
	for _, p := range simplePatterns {
		// 命中寒暄模式即视为简单问题
		if strings.Contains(goalLower, p) {
			return true
		}
	}
	// 整句就是英文寒暄词（如 "hello" / "hi there"）
	if isGreetingOnly(goalLower) {
		return true
	}

	// 极短且纯 ASCII（如 "ping"）仍视为简单；中文短句一律不在此列
	if utf8.RuneCountInString(goal) <= 6 && isASCII(goal) {
		return true
	}
	return false
}

// isGreetingOnly 判断 goal 是否整句就是一个英文寒暄词（可带标点/空格）。
//
// 例如 "hello" / "hi there" / "hey!"。
// 避免 "创建hello.txt" 这种实际任务被当作寒暄。
//
// 参数：
//   - goal：用户目标（已转小写）
//
// 返回：是纯寒暄返回 true。
func isGreetingOnly(goal string) bool {
	// 去首尾空白并转小写
	g := strings.TrimSpace(strings.ToLower(goal))
	greetings := []string{"hello", "hi", "hey", "hi there", "hey there"}
	for _, gr := range greetings {
		// 整句匹配
		if g == gr {
			return true
		}
		// 允许尾部标点
		if strings.HasPrefix(g, gr) {
			rest := strings.TrimSpace(g[len(gr):])
			// 尾部为空或全是标点
			if rest == "" || allPunct(rest) {
				return true
			}
		}
	}
	return false
}

// allPunct 判断字符串是否全由标点符号组成。
// 覆盖 ASCII 标点与中文常见标点（！。？，）。
//
// 参数：
//   - s：待判断的字符串
//
// 返回：全标点且非空返回 true。
func allPunct(s string) bool {
	for _, r := range s {
		// 检查是否在 ASCII 标点区间或中文标点
		if !((r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~') ||
			r == '！' || r == '。' || r == '？' || r == '，') {
			return false
		}
	}
	return s != ""
}

// isASCII 判断字符串是否全为 ASCII 字符（码点 <= 127）。
// 用于区分纯英文短句与中文短句。
//
// 参数：
//   - s：待判断的字符串
//
// 返回：全 ASCII 返回 true。
func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
// humanClarifyEnabled 返回是否启用人机对话（特性5）。
// 默认 true；可被 AgentCfg.HumanClarifyEnabled 关闭。
func (n *MetaAgentNode) humanClarifyEnabled() bool {
	if n.rt == nil || n.rt.AgentCfg == nil {
		return true // 默认启用
	}
	return n.rt.AgentCfg.HumanClarifyEnabled
}