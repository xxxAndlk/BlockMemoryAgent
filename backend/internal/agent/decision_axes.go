package agent

// decision_axes.go —— 多轴决策账本（2026-10-24 媒体/记忆质量修复 P1）。
//
// 事故背景（会话 #65/#66）：单路线账本把任何 route 级指令都当成"推翻整条路线"：
//   - "重新 AI 生图" 把动画路线一起作废；
//   - "你不用开始工作，你告诉我这个 skill 干嘛用的" 撤销了执行路线；
//   - 视角/审批门/验收标准等不同维度的约束被同一条指令误伤。
// 本文件把"路线"拆成互不覆盖的决策轴：同一轴内新指令只替换该轴旧行
//（SupersedeAxis），跨轴同时有效。注入侧按轴渲染 + 确定性预算裁剪（绝不整体尾巴一刀）。
//
// 轴取值见 store.AxisXxx（execution_mode / asset_source / animation_method / identity /
// view / approval_anchor / approval_keyframe / acceptance / delivery）。

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/store"
)

// answerOnlyMarkers 命中即改 execution_mode=answer_only（只回答，不动手不写文件）。
// 2026-10-23 实证：「你不用开始工作，你告诉我这个 skill 干嘛用的」曾被当成路线级改向，
// 把整条动画路线作废。回答类指令只允许改执行模式这一轴。
var answerOnlyMarkers = []string{
	"不用开始", "不要开始", "不开始工作", "先别开始", "别开始", "先别做", "先别动手",
	"不要动手", "不用动手", "只回答", "回答问题即可", "解释一下", "讲解一下",
	"干嘛用", "干什么用", "做什么用的", "是什么用的", "先别执行", "不要执行",
}

// axisMarkerGroups 轴关键词分组，按优先级从高到低匹配（先命中者即该条决策的轴）。
// 顺序是有意的：
//   - animation_method 先于 asset_source：任务书常同时写"AI 生一张图 + 部位拼接"，
//     决定路线的是动画方式；把任务书判到素材轴会让后来的"改用 16 帧图集"无法替换它。
//   - execution_mode 最优先：回答类指令语义上不属于任何路线。
var axisMarkerGroups = []struct {
	axis    string
	markers []string
}{
	{store.AxisView, []string{"视角", "机位", "正面视", "侧面视", "背视", "俯视", "仰视", "四分之三", "45度", "45°", "正视", "侧视", "平视"}},
	{store.AxisAnimationMethod, []string{"抽帧", "逐帧", "帧渲染", "序列帧", "图集", "骨架", "拼接", "骨骼", "蒙皮", "形变", "warp", "remap", "关键姿态", "关键帧", "动作帧", "舞步", "奔跑动画", "动画方式", "视频生成", "ai视频", "i2v", "首帧"}},
	{store.AxisAssetSource, []string{"生图", "生成图", "重生图", "重新生成", "ai画", "ai 生", "已有素材", "现有素材", "素材复用", "素材来源", "拆部位", "画一张", "出图"}},
	{store.AxisIdentity, []string{"用 d 版", "用d版", "d 版", "d版", "a版", "b版", "c版", "立绘", "角色设计", "形象设计", "设定图", "参考资产", "角色合同", "锚点图", "这只角色", "同一只", "角色设定"}},
	{store.AxisDelivery, []string{"交付", "成品", "打包交", "交付形式", "上线"}},
}

// classifyDecisionAxis 按内容判定决策轴；未命中返回空轴（普通指令，跨轴不覆盖）。
//
// 注意：这里只做"这条指令想改哪个维度"的粗分类——判错轴比判错路线轻，
// 因为跨轴不互相作废；真正的路线互斥仍由 planConflictWarning 提示。
func classifyDecisionAxis(content string) string {
	s := strings.ToLower(strings.TrimSpace(content))
	if s == "" {
		return store.AxisGeneral
	}
	if containsAnyMarker(s, answerOnlyMarkers) {
		return store.AxisExecutionMode
	}
	for _, g := range axisMarkerGroups {
		if containsAnyMarker(s, g.markers) {
			return g.axis
		}
	}
	return store.AxisGeneral
}

// isConstraintAxis 是否属于"约束轴"：这些轴上的用户表述会自动登记为该轴现行选择
// （kind=route），同轴内新表述替换旧表述。审批门/验收标准/无轴不在此列。
func isConstraintAxis(axis string) bool {
	switch axis {
	case store.AxisExecutionMode, store.AxisAssetSource, store.AxisAnimationMethod,
		store.AxisIdentity, store.AxisView, store.AxisDelivery:
		return true
	default:
		return false
	}
}

// axisLabel 轴的中文展示名（账本注入用）。
func axisLabel(axis string) string {
	switch axis {
	case store.AxisExecutionMode:
		return "执行模式"
	case store.AxisAssetSource:
		return "素材来源"
	case store.AxisAnimationMethod:
		return "动画方式"
	case store.AxisIdentity:
		return "角色/参考资产"
	case store.AxisView:
		return "视角"
	case store.AxisApprovalAnchor:
		return "锚点审批"
	case store.AxisApprovalKeyframe:
		return "关键姿态审批"
	case store.AxisAcceptance:
		return "验收标准"
	case store.AxisDelivery:
		return "交付形式"
	default:
		return ""
	}
}

// approvalContent 生成审批行内容（只留状态 + 参考路径，便于"换参考图才失效"判定）。
func approvalContent(approved bool, ref string) string {
	state := "rejected"
	if approved {
		state = "approved"
	}
	if strings.TrimSpace(ref) == "" {
		return state
	}
	return state + " ref=" + strings.TrimSpace(ref)
}

// approvalRef 从审批行内容里取参考路径（"approved ref=<path>" / "rejected ref=<path>"）。
func approvalRef(content string) string {
	const mark = "ref="
	i := strings.Index(content, mark)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(content[i+len(mark):])
}

// approvalStateOf 审批行内容 → approved|rejected|pending。
func approvalStateOf(content string) string {
	switch {
	case strings.HasPrefix(content, "approved"):
		return "approved"
	case strings.HasPrefix(content, "rejected"):
		return "rejected"
	default:
		return "pending"
	}
}

// invalidateApprovalsLocked 换参考资产时把旧的锚点/关键姿态审批标 superseded。
// 调用方必须持有 dc.mu 写锁。判据：审批行记录的 ref 与本次新参考路径不同。
// 规则（§5.1 行为规则 6）：改路线不得隐式清除批准；换参考图 hash 才使旧批准失效。
func (s *ReactService) invalidateApprovalsLocked(ctx context.Context, sessionID string, dc *sessionDecisionCache, refs []string) {
	newRefs := map[string]bool{}
	for _, r := range refs {
		newRefs[r] = true
	}
	for _, axis := range []string{store.AxisApprovalAnchor, store.AxisApprovalKeyframe} {
		stale := false
		for i := range dc.items {
			d := &dc.items[i]
			if d.Axis != axis || d.Kind != store.DecisionKindDirective || d.Status != store.DecisionStatusActive {
				continue
			}
			oldRef := approvalRef(d.Content)
			if oldRef == "" || newRefs[oldRef] {
				continue // 无参考记录或无变化：保留批准
			}
			d.Status = store.DecisionStatusSuperseded
			stale = true
		}
		if stale && s.decisionStore != nil {
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = s.decisionStore.SupersedeAxis(ictx, sessionID, axis, 0, []string{store.DecisionKindDirective})
			cancel()
		}
	}
}

// RecordMediaGateApproval 记录锚点/关键姿态审批门状态（媒体审批流程调用）。
// stage=anchor|keyframe；approved 由用户答复决定；ref 是本次被审的参考图路径。
func (s *ReactService) RecordMediaGateApproval(ctx context.Context, sessionID, stage string, approved bool, ref string) {
	var axis string
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "anchor":
		axis = store.AxisApprovalAnchor
	case "keyframe":
		axis = store.AxisApprovalKeyframe
	default:
		return
	}
	if s == nil || s.store == nil {
		return
	}
	s.store.mu.Lock()
	session := s.store.sessions[sessionID]
	s.store.mu.Unlock()
	if session == nil {
		return
	}
	s.hydrateDecisions(ctx, session)
	s.recordDecisionAxis(ctx, session, store.DecisionKindDirective, axis, approvalContent(approved, ref), "asset_gate")
}

// CurrentMediaGateState 返回某审批门当前状态（approved|rejected|pending）。
// ref 非空时只看该参考图的审批（旧批准对不同参考图不生效 → 视作 pending）。
func (s *ReactService) CurrentMediaGateState(sessionID, stage, ref string) string {
	if s == nil || s.store == nil || sessionID == "" {
		return "pending"
	}
	axis := store.AxisApprovalAnchor
	if strings.EqualFold(strings.TrimSpace(stage), "keyframe") {
		axis = store.AxisApprovalKeyframe
	}
	s.store.mu.Lock()
	session := s.store.sessions[sessionID]
	s.store.mu.Unlock()
	if session == nil {
		return "pending"
	}
	dc := sessionDecisionCacheOf(session)
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	state := "pending"
	for _, d := range dc.items {
		if d.Axis != axis || d.Kind != store.DecisionKindDirective || d.Status != store.DecisionStatusActive {
			continue
		}
		if ref != "" {
			if oldRef := approvalRef(d.Content); oldRef != "" && oldRef != ref {
				continue
			}
		}
		state = approvalStateOf(d.Content)
	}
	return state
}

// parseAssetGateClarify 从资产审批提问里解析 stage 与参考产物路径（2026-10-24）。
// 约定（提示词强制）：问题以「【资产审批·anchor】」或「【资产审批·keyframe】」开头，
// Detail 里有一行「产物：<工作目录相对路径>」。解析失败返回 ok=false（不硬造审批）。
func parseAssetGateClarify(pc *ClarifyRequest) (stage, ref string, ok bool) {
	if pc == nil {
		return "", "", false
	}
	head := pc.Question
	if i := strings.Index(head, "【资产审批"); i >= 0 {
		head = head[i:]
	} else {
		return "", "", false
	}
	switch {
	case strings.Contains(head, "keyframe"):
		stage = "keyframe"
	case strings.Contains(head, "anchor"):
		stage = "anchor"
	default:
		return "", "", false
	}
	for _, line := range strings.Split(pc.Detail, "\n") {
		t := strings.TrimSpace(line)
		for _, prefix := range []string{"产物：", "产物:", "参考图：", "参考图:"} {
			if strings.HasPrefix(t, prefix) {
				ref = strings.TrimSpace(strings.TrimPrefix(t, prefix))
			}
		}
		if ref != "" {
			break
		}
	}
	return stage, ref, true
}

// ledgerSection 注入块的一段；required=true 的段永不因预算被跳过。
type ledgerSection struct {
	required bool
	text     string
}

// assembleLedger 确定性预算装配：按给定顺序追加，可选段放不下就**跳过该段**，
// 绝不最后把整个块尾巴一刀切掉（一刀切会恰好砍掉最新的轴/指令——事故根因之一）。
// 必留段已各自带长度上限；required 超预算时才做兜底截断。
func assembleLedger(sections []ledgerSection, max int) string {
	var b strings.Builder
	used := 0
	for _, s := range sections {
		if s.text == "" {
			continue
		}
		n := utf8.RuneCountInString(s.text)
		if !s.required && max > 0 && used+n > max {
			continue
		}
		b.WriteString(s.text)
		used += n
	}
	out := strings.TrimRight(b.String(), "\n")
	if max > 0 && utf8.RuneCountInString(out) > max {
		out = ledgerEllipsis(out, max)
	}
	return out
}
