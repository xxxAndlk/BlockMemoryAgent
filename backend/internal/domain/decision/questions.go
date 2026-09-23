package decision

// questions.go 六切入点的提问构造（TODO #23③）。
// 候选集在此集中持有（结构性防幻觉：模型输出集外值由 answerValid 丢弃）；
// 提问文案只描述判据，状态片段统一走 compactRunes 截断防超长。

// 任务分诊（① sendMessageFull）候选集：规模性质 / 所需工具面。
const (
	TaskNatureQuick  = "quick"  // 速答（纯文本问答，零工具）
	TaskNatureSingle = "single" // 单 agent 可完成
	TaskNatureMulti  = "multi"  // 需多域编排

	ToolFaceNone  = "none"  // 纯文本
	ToolFaceRead  = "read"  // 只读（读文件/搜索/检索）
	ToolFaceWrite = "write" // 文件写改
	ToolFaceExec  = "exec"  // 命令执行/构建/测试
)

// 失败处置（② 失败消息组装）候选集。
const (
	DispRetry     = "retry"     // 重跑
	DispRelegate  = "redelegate" // 降级/重派他手
	DispEscalate  = "escalate"  // 上报升级
	DispSuspend   = "suspend"   // 挂起等人
)

// 派发门灰区（③ dispatchOne 规则簇之后）候选集：规则正则覆盖不到的需求信号。
const (
	GapNone        = "none"
	GapNeedsBrowser = "needs_browser"
	GapNeedsMCP     = "needs_mcp"
	GapNeedsVision  = "needs_vision"
)

// 档位建议（⑥ gear_signals 同位，只读）候选集，与 gear 枚举同名。
const (
	GearFast    = "fast"
	GearDaily   = "daily"
	GearCluster = "cluster"
)

// TaskTriageQuestions ①任务级意图分诊三问（一次 provider 往返批量答）。
// content 为用户输入原文（截断进 Context）。
func TaskTriageQuestions(content string) []Question {
	c := compactRunes(content, 3000)
	return []Question{
		{
			Key: "task_nature", Point: PointIntentKind, Primitive: PrimitiveChoice,
			Prompt:     "该用户任务的规模性质？quick=纯文本问答可直接回答；single=单个执行 Agent 可独立完成；multi=需要多个领域 Agent 编排协作",
			Candidates: []string{TaskNatureQuick, TaskNatureSingle, TaskNatureMulti},
			Context:    c,
		},
		{
			Key: "tool_face", Point: PointToolFace, Primitive: PrimitiveChoice,
			Prompt:     "完成该任务所需的工具面？none=纯文本；read=只读文件/搜索；write=需写改文件；exec=需执行命令/构建/测试",
			Candidates: []string{ToolFaceNone, ToolFaceRead, ToolFaceWrite, ToolFaceExec},
			Context:    c,
		},
		{
			Key: "need_clarify", Point: PointNeedClarify, Primitive: PrimitiveNoul,
			Prompt:  "任务描述是否模糊到必须先向用户澄清关键缺口才能开工（目标/范围/验收标准缺失）？",
			Context: c,
		},
	}
}

// FailureDispositionQuestion ②失败处置路由一问。fail 摘要（kind/错误/partial）进 Context。
func FailureDispositionQuestion(failSummary string) Question {
	return Question{
		Key: "disposition", Point: PointFailureDisp, Primitive: PrimitiveChoice,
		Prompt:     "子 Agent 失败后的处置路由建议？retry=同任务重跑；redelegate=降级或改派其他执行者；escalate=上报上级/用户仲裁；suspend=挂起等待人工",
		Candidates: []string{DispRetry, DispRelegate, DispEscalate, DispSuspend},
		Context:    compactRunes(failSummary, 3000),
	}
}

// DispatchGapQuestion ③派发门灰区需求信号一问（规则簇后的补充拦截建议）。
func DispatchGapQuestion(roleID, task string) Question {
	return Question{
		Key: "gap", Point: PointDispatchGap, Primitive: PrimitiveChoice,
		Prompt: "派发目标任务与角色工具面之间是否存在规则未覆盖的能力缺口信号？none=无缺口；needs_browser=需要浏览器操作；needs_mcp=需要 MCP/外部服务工具；needs_vision=需要读图/视觉能力",
		Candidates: []string{GapNone, GapNeedsBrowser, GapNeedsMCP, GapNeedsVision},
		Context:    compactRunes("role="+roleID+"\ntask="+task, 3000),
	}
}

// UptakeScoreQuestions ④黑板摄取相关性打分（逐候选一问，单次批量往返）。
// keys[i] 与 recs[i] 对齐（"rec:<i>"）。
func UptakeScoreQuestions(recContents []string, task string) []Question {
	qs := make([]Question, 0, len(recContents))
	for i, c := range recContents {
		qs = append(qs, Question{
			Key:        "rec:" + itoa(i),
			Point:      PointUptakeScore,
			Primitive:  PrimitiveScore,
			Prompt:     "该记忆条目与当前任务的相关性（0=无关应丢弃，1=强相关必注入）",
			Context:    compactRunes("task="+task+"\nrecord="+c, 1500),
		})
	}
	return qs
}

// ExtractWorthQuestion ⑤沉淀提取预判（省无效 LLM 提取调用）。
func ExtractWorthQuestion(goal, roleID string) Question {
	return Question{
		Key: "worth", Point: PointExtractWorth, Primitive: PrimitiveNoul,
		Prompt:  "该 Agent 输出是否含跨任务可复用的事实/决策/契约（值得花一次提取调用）？纯完成状态/过程叙述/验证声明判 no",
		Context: compactRunes("goal="+goal+"\nrole="+roleID, 1500),
	}
}

// SalvageWorthQuestion ⑤打捞提取预判（kill 场景跳过已是规则版先例，此处管有历史场景）。
func SalvageWorthQuestion(roleID string) Question {
	return Question{
		Key: "worth", Point: PointSalvageWorth, Primitive: PrimitiveNoul,
		Prompt:  "该失败输出是否含可复用打捞价值（已读文件清单/已得结论/卡点定位，值得花一次提取调用）？纯失败通知/无信息文本判 no",
		Context: "role=" + roleID,
	}
}

// GearHintQuestion ⑥档位建议（只读影子，永不做自动选档——auto 退役决策不破）。
func GearHintQuestion(content string) Question {
	return Question{
		Key: "gear", Point: PointGearHint, Primitive: PrimitiveChoice,
		Prompt:     "该任务的合理执行档位？fast=轻问答；daily=单执行者日常任务；cluster=多域大编排",
		Candidates: []string{GearFast, GearDaily, GearCluster},
		Context:    compactRunes(content, 2000),
	}
}

// itoa 零依赖整数转字符串（避免 fmt 进热点路径）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
