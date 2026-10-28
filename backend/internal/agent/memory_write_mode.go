package agent

// memory_write_mode.go —— 长期记忆写入模式（2026-10-24 媒体/记忆质量修复 P0）。
//
// 事故背景（doc/计划_媒体动画与记忆质量修复.md §2.7）：SessionEvolver 会话结束会把
// 「用户偏好 / 项目经验 / 经验技能」三类候选直接 Merge 进现行记忆，缺少确定性准入：
//   - 一次性任务指令（"用 D 版""不走视频抽帧"）被写成跨项目永久偏好；
//   - 试验参数（单底图 warp 的 duty/bob 值）与失败半成品被写进项目经验，此后每轮回注；
//   - 模型自报"已完成"就晋升为经验，用户随后骂"做的什么玩意"的会话同样入库。
// 现行记忆每轮全文注入，一次误写就是长期污染（33h/1005 次请求复注的教训）。
//
// 三档（config memory.write_mode）：
//   off      entirely skip SessionEvolver（不调用轻量模型，零成本零写入）；
//   shadow   跑进化但只写 evolution_log 候选（可审计、可人工采纳），不碰任何现行记忆；
//   enforce  允许写入，但必须过确定性准入门（isLongTermPreference/admitProjectLesson）。
//
// 注意：空值（测试/直接构造 ReactService）按 enforce 解释，保持"老代码行为不变"；
// config 层 applyMemoryDefaults 默认填 shadow，因此线上首启即 shadow。

import (
	"strings"
)

// 长期记忆写入模式取值（与 config.MemoryConfig.WriteMode 一一对应）。
const (
	MemoryWriteModeOff     = "off"
	MemoryWriteModeShadow  = "shadow"
	MemoryWriteModeEnforce = "enforce"
)

// SetMemoryWriteMode 注入长期记忆写入模式（bootstrap 按 config.memory.write_mode 装配）。
// 非法值按 shadow 处理（保守：宁可只出候选，也不要误写现行记忆）。
func (s *ReactService) SetMemoryWriteMode(mode string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case MemoryWriteModeOff:
		s.memoryWriteMode = MemoryWriteModeOff
	case MemoryWriteModeEnforce:
		s.memoryWriteMode = MemoryWriteModeEnforce
	case "", MemoryWriteModeShadow:
		s.memoryWriteMode = MemoryWriteModeShadow
	default:
		s.memoryWriteMode = MemoryWriteModeShadow
	}
}

// MemoryWriteMode 返回当前写入模式（未注入时按 enforce：保持老代码/单测语义）。
func (s *ReactService) MemoryWriteMode() string {
	if s == nil || s.memoryWriteMode == "" {
		return MemoryWriteModeEnforce
	}
	return s.memoryWriteMode
}

// longTermIntentMarkers 用户偏好晋升为"跨会话长期偏好"所需的明确长期意图标记。
// 判据来自用户原话而不是模型自评：没有这些话，就只是一次性任务选择。
var longTermIntentMarkers = []string{
	"以后", "今后", "始终", "永远", "每次", "一直", "长期", "记住",
	"我的偏好", "我习惯", "我通常", "我一般", "请不要", "都请", "统一用",
}

// oneShotMarkers 一次性任务选择标记：命中即拒绝晋升为全局偏好（哪怕模型写了"偏好"）。
// 这些词出现在原话里说明选择绑定在本次任务/本次素材上。
var oneShotMarkers = []string{
	"这一版", "这次", "本次", "当前这个", "先这样", "临时", "先用", "试用",
	"这个角色", "这张图", "这套素材", "这个项目",
}

// isLongTermPreference 长期偏好确定性准入：必须带明确长期意图，且不含一次性任务标记。
// 用于 enforce 模式下写全局画像（user_profile.md）。
func isLongTermPreference(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	if containsAnyMarker(s, oneShotMarkers) {
		return false
	}
	return containsAnyMarker(s, longTermIntentMarkers)
}

// 会话结束状态（EvolveInput.Outcome 取值，见 evolveSession 调用点）。
const (
	evolveOutcomeSuccess  = "success"
	evolveOutcomeMixed    = "mixed"
	evolveOutcomeFailed   = "failed"
	evolveOutcomePartial  = "partial"
	evolveOutcomeCanceled = "canceled"
)

// admitProjectLesson 项目经验确定性准入（enforce）：
//   - mixed/failed 会话只留失败档案，不晋升为现行经验（"指标全过但视觉不合格"同理）；
//   - 被用户否决的会话已在 evolveSession 里提前 return，此处不重复判；
//   - 内容为空或只是过程叙述（无验证证据低，交由技能/账本承载）不写。
func admitProjectLesson(outcome, lesson string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case evolveOutcomeSuccess, evolveOutcomePartial, "completed", "":
		return strings.TrimSpace(lesson) != ""
	default:
		return false
	}
}

// memoryModeAllowsWrite 现行记忆写入是否被模式允许（off/shadow 为 false）。
func memoryModeAllowsWrite(mode string) bool {
	return mode == MemoryWriteModeEnforce
}

// admitPreferences 过滤全局偏好增量：只保留带明确长期意图、且不是一次性任务选择的条目。
func admitPreferences(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if isLongTermPreference(p) {
			out = append(out, p)
		}
	}
	return out
}

// admitProjectLessons 过滤项目经验增量（mixed/failed 会话不晋升，见 admitProjectLesson）。
func admitProjectLessons(outcome string, in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if admitProjectLesson(outcome, l) {
			out = append(out, l)
		}
	}
	return out
}

// admitSkillOutcome 经验技能沉淀的状态闸门：只有成功/部分成功的会话才配固化工艺模板。
func admitSkillOutcome(outcome string) bool {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case evolveOutcomeSuccess, evolveOutcomePartial, "completed", "":
		return true
	default:
		return false
	}
}

// logShadowCandidates shadow 模式下把进化的三类候选逐项写 evolution_log（type=shadow_*）。
// 只写审计表，不改现行记忆；人工确认后可手动采纳（或切 enforce 后重跑）。
func (s *ReactService) logShadowCandidates(sessionID, outcome string, out *EvolveOutput, rejected []string) {
	if out == nil || s.evolutionLog == nil {
		return
	}
	for _, p := range cleanIncrements(out.UserPrefs) {
		s.logEvolution(sessionID, "shadow_user_pref", "用户画像候选", p+"（shadow·仅供人工采纳）")
	}
	for _, l := range cleanIncrements(out.ProjectLessons) {
		s.logEvolution(sessionID, "shadow_project_lesson", "项目经验候选", l+"（shadow·仅供人工采纳）")
	}
	for _, sk := range out.Skills {
		s.logEvolution(sessionID, "shadow_skill", "经验技能候选", strings.TrimSpace(sk.Name)+" — "+strings.TrimSpace(sk.Title)+"（shadow·仅供人工采纳）")
	}
	if len(rejected) > 0 {
		s.logEvolution(sessionID, "shadow_rejected", "否决候选", strings.Join(rejected, "；"))
	}
}
