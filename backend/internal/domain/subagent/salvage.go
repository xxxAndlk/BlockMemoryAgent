package subagent

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SalvageExtractor 从失败子 Agent 的输出中提取打捞摘要（已读文件清单 + 已得结论 + 卡点）。
// 与 FactExtractor 同签名但 prompt 面向失败打捞；bootstrap 注入轻量模型实现。
// 提取失败/超时由调用方回退末条 assistant 文本截断，不阻塞失败主流程。
type SalvageExtractor interface {
	Extract(ctx context.Context, text, goal, roleID string) ([]string, error)
}

const (
	// salvageSlotPrefix 是失败打捞共享槽位 key 前缀的**遗留命名空间标记**（TODO #24 批二⑩）：
	// 槽位读写已删（黑板通道实测覆盖 100%，2026-09-24 验证 slot 触发率为零），
	// 此常量仅保留给 buildSharedPrefix 跳过历史遗留槽位值（防旧数据经通用共享注入外泄）。
	salvageSlotPrefix = "salvage:"
	// salvageRoleScopePrefix 是叶子派发的打捞 scope 前缀（domain 为空时用 roleID 键）。
	salvageRoleScopePrefix = "role."
	// salvageMaxRunes 是打捞摘要写黑板/追加进任务文本的最大 rune 数。
	salvageMaxRunes = 2000
	// salvagePrefixMarker 是打捞摘要追加进父 mailbox 失败消息时的标记。
	salvagePrefixMarker = "【失败打捞】\n"
)

// salvageScope 计算打捞记录的 scope 键：domain 派发用领域简称，
// 叶子派发（domain 空）用 "role:<roleID>"。两条路径（写/读）必须同键。
func salvageScope(domain, roleID string) string {
	if s := strings.TrimSpace(domain); s != "" {
		return s
	}
	return salvageRoleScopePrefix + strings.TrimSpace(roleID)
}

// salvageFailure 在子 Agent 失败路径调用（超时/被杀/循环守卫终止/通用错误）：
// 从失败结果中提取打捞摘要，双路送达——
//  1. 写黑板块记忆（outcome=fail，task_domain=scope；供同域/同角色重派经
//     withPriorSalvage 带前序摘要。slot 通道已删，TODO #24 批二⑩）；
//  2. 返回摘要文本，调用方追加进父 mailbox 失败消息（【失败打捞】标记）。
//
// 提取策略：salvageExtractor 已注入且有历史文本时先调 LLM 提取；
// 失败/超时/无历史回退 partial（末条 assistant 文本截断）。任何错误只记日志不阻塞主流程。
func (d *Dispatcher) salvageFailure(ctx context.Context, parentID, subAgentID string, roleDef types.RoleDefinition, domain string, result agent.ReactResult, partial string) string {
	text := ""
	if result.History != nil {
		text = strings.TrimSpace(agent.LastAssistantText(result.History))
	}
	if text == "" {
		text = strings.TrimSpace(partial)
	}
	if text == "" {
		return ""
	}

	salvage := text
	extracted := false
	// 仅在有真实历史时调 LLM 打捞提取；kill 场景（History nil）直接用回退文本，
	// 避免对"心跳超时已取消"这类无信息文本空跑轻量模型。
	if d.salvageExtractor != nil && result.History != nil {
		// 决策层⑤打捞提取预判（TODO #23 切入点5）：kill 场景跳过已是规则版先例，
		// 此处 Noul 预判再省一层无效提取调用。影子期照常提取补对拍真值。
		skip, decAns := d.decisionGateSalvageWorth(ctx, subAgentID, roleDef.ID, text)
		if skip {
			log.Printf("[subagent] decision salvage_worth=no, skip extraction: sub=%s role=%s", subAgentID, roleDef.ID)
			return ""
		}
		// 超时取配置值（默认 30s，思考型模型场景 60s+）：思考型模型首 token 就要数十秒，
		// 旧 5s 硬编码致打捞提取全超时降级 facts=0（TODO #33 事故链），超时兜底保留。
		timeout := d.salvageTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		scCtx, cancel := context.WithTimeout(ctx, timeout)
		facts, err := d.salvageExtractor.Extract(scCtx, text, "", roleDef.ID)
		cancel()
		if err == nil && len(facts) > 0 {
			salvage = strings.Join(facts, "\n")
			extracted = true
		} else {
			log.Printf("[subagent] salvage extract failed, fallback partial: sub=%s err=%v facts=%d", subAgentID, err, len(facts))
		}
		d.observeExtractOutcome(ctx, subAgentID, decAns, extracted)
	}
	// 打捞兜底防污染（2026-09-16）：打捞 LLM 未提取成功时，回退文本可能整段是失败通知
	// 原文（"[failure kind=killed retryable=false] 子 Agent ... 被停止"），对后续召回
	// 零价值（存量实证：44 条 salvage 块记忆里 13 条是这类原文）。此时不写槽位也不写黑板。
	if !extracted && strings.Contains(salvage, "[failure kind=") {
		log.Printf("[subagent] salvage fallback is failure notice, skip sediment: sub=%s", subAgentID)
		return ""
	}
	salvage = truncateRunes(strings.TrimSpace(salvage), salvageMaxRunes)
	if salvage == "" {
		return ""
	}

	// scope：domain 派发用领域简称；叶子派发用 "role.<roleID>"（2026-08-19 补位：
	// 心跳误杀的叶子（如战斗实体首任 10 分钟被杀）重派此前不读打捞摘要，从零重跑
	// 损失约 20 分钟；叶子也写黑板，重派同角色时带回前序摘要）。
	scope := salvageScope(domain, roleDef.ID)
	// 黑板模式单通道：把打捞摘要写黑板块记忆（outcome=fail），供
	// withPriorSalvage 经 BlackboardSearcher.Query 按 scope 检索。
	// 经 d.saver 落库（embedding 由 blockMemorySaver.Save 处理）；best-effort 不阻塞主流程。
	if d.saver != nil && d.writeEnabled {
		sid := tool.SessionIDFromContext(ctx)
		rec := &types.KnowledgeRecord{
			KnowledgeType: enums.KnowledgeTypeBlockMemory,
			Content:       salvage,
			Meta: map[string]any{
				"goal":         truncateRunes(salvage, blockMemoryGoalMaxRunes),
				"domain":       roleDef.ID,
				"task_domain":  scope,
				"parent_id":    parentID,
				"session_id":   sid,
				"sub_agent_id": subAgentID,
				"source":       "salvage",
				"outcome":      blockOutcomeFail,
				"reuse_count":  0,
			},
			CreatedAt: time.Now(),
		}
		d.saveBlockRecord(ctx, rec, "salvage")
	}
	return salvage
}

// withPriorSalvage 检查同父同 scope 是否有前序失败/部分打捞记录，以【前序探索摘要】前缀
// 追加到新任务文本末尾，使重派从机制上不重复探索（不依赖 MetaAgent 记性）。无记录返回原任务。
// scope 规则（2026-08-19 扩展）：domain 派发用领域简称；叶子派发用 roleID（同角色重派
// 带回前序摘要--心跳误杀/超时杀掉的叶子重派此前从零重跑，损失 20 分钟量级）。
//
// 黑板单通道（启用；#24 批二⑩ 删 slot 回退）：BlackboardSearcher 按 scope
// （parent_id + task_domain）查 fail/partial 打捞记录（salvageFailure 落库）；
// 未实现 / 0 命中 / 出错时零注入（原 slot 回退实测触发率为零，已删）。
func (d *Dispatcher) withPriorSalvage(ctx context.Context, parentID, domain, roleID, task string) string {
	scope := salvageScope(domain, roleID)
	if scope == salvageRoleScopePrefix {
		return task // roleID 也为空的防御分支
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	bb, ok := d.searcher.(BlackboardSearcher)
	if !ok || sid == "" {
		return task
	}
	recs, err := bb.Query(ctx, sid, parentID, scope, "", 3, "")
	if err != nil {
		return task
	}
	var lines []string
	for _, r := range recs {
		if blockOutcomeRank(r.Meta) > 0 { // fail/partial
			if t := strings.TrimSpace(r.Content); t != "" {
				lines = append(lines, truncateRunes(t, salvageMaxRunes/2))
			}
		}
	}
	if len(lines) == 0 {
		return task
	}
	if len(lines) > 2 {
		lines = lines[:2]
	}
	log.Printf("[subagent] prior salvage from blackboard: parent=%s scope=%s hits=%d", parentID, scope, len(lines))
	return task + "\n\n【前序探索摘要】\n" + strings.Join(lines, "\n")
}
