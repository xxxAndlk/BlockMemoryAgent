package subagent

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SalvageExtractor 从失败子 Agent 的输出中提取打捞摘要（已读文件清单 + 已得结论 + 卡点）。
// 与 FactExtractor 同签名但 prompt 面向失败打捞；bootstrap 注入轻量模型实现。
// 提取失败/超时由调用方回退末条 assistant 文本截断，不阻塞失败主流程。
type SalvageExtractor interface {
	Extract(ctx context.Context, text, goal, roleID string) ([]string, error)
}

const (
	// salvageSlotPrefix 是失败打捞共享槽位 key 前缀：<parentID>:salvage:<domain>。
	// 与自由槽位（WriteSharedMemory）共用 sharedMem 后端；buildSharedPrefix 跳过
	// 该前缀的槽位（不向所有子 Agent 通用注入），由同域重派显式读回（withPriorSalvage）。
	salvageSlotPrefix = "salvage:"
	// salvageMaxRunes 是打捞摘要写入槽位/追加进任务文本的最大 rune 数。
	salvageMaxRunes = 2000
	// salvageLLMTimeout 打捞轻量调用的超时：超过即回退文本截断，不拖慢失败回灌。
	salvageLLMTimeout = 5 * time.Second
	// salvagePrefixMarker 是打捞摘要追加进父 mailbox 失败消息时的标记。
	salvagePrefixMarker = "【失败打捞】\n"
)

// salvageFailure 在子 Agent 失败路径调用（超时/被杀/循环守卫终止/通用错误）：
// 从失败结果中提取打捞摘要，双路送达——
//  1. 写入共享槽位 <parentID>:salvage:<domain>（供同域重派经 withPriorSalvage 带前序摘要）；
//  2. 返回摘要文本，调用方追加进父 mailbox 失败消息（【失败打捞】标记）。
//
// 提取策略：salvageExtractor 已注入且有历史文本时先调 LLM 提取；
// 失败/超时/无历史回退 partial（末条 assistant 文本截断）。任何错误只记日志不阻塞主流程。
// domain 为空时不写槽位（无法按键），仍返回摘要供 mailbox 追加。
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
	// 仅在有真实历史时调 LLM 打捞提取；kill 场景（History nil）直接用回退文本，
	// 避免对"心跳超时已取消"这类无信息文本空跑轻量模型。
	if d.salvageExtractor != nil && result.History != nil {
		scCtx, cancel := context.WithTimeout(ctx, salvageLLMTimeout)
		facts, err := d.salvageExtractor.Extract(scCtx, text, "", roleDef.ID)
		cancel()
		if err == nil && len(facts) > 0 {
			salvage = strings.Join(facts, "\n")
		} else {
			log.Printf("[subagent] salvage extract failed, fallback partial: sub=%s err=%v facts=%d", subAgentID, err, len(facts))
		}
	}
	salvage = truncateRunes(strings.TrimSpace(salvage), salvageMaxRunes)
	if salvage == "" {
		return ""
	}

	if domain != "" && d.sharedMem != nil {
		key := parentID + ":" + salvageSlotPrefix + strings.TrimSpace(domain)
		if err := d.sharedMem.Set(ctx, key, salvage); err != nil {
			log.Printf("[subagent] salvage slot write failed: sub=%s key=%s err=%v", subAgentID, key, err)
		}
	}
	return salvage
}

// withPriorSalvage 检查同父同 domain 是否有 Failed/Cancelled 兄弟节点；有则读取其
// 打捞摘要槽位 <parentID>:salvage:<domain>，以【前序探索摘要】前缀追加到新任务文本末尾，
// 使同域重派从机制上不重复探索（不依赖 MetaAgent 记性）。无兄弟/摘要为空返回原任务。
func (d *Dispatcher) withPriorSalvage(ctx context.Context, parentID, domain, task string) string {
	domain = strings.TrimSpace(domain)
	if domain == "" || d.sharedMem == nil || d.treeFn == nil {
		return task
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		if i := strings.Index(parentID, "/"); i > 0 {
			sid = parentID[:i]
		}
	}
	t := d.treeFn(sid)
	if t == nil {
		return task
	}
	found := false
	for _, n := range t.Snapshot() {
		if n.ParentID != parentID || n.Role != "domain" || strings.TrimSpace(n.Domain) != domain {
			continue
		}
		if n.Status == orchestrator.StatusFailed || n.Status == orchestrator.StatusCancelled {
			found = true
			break
		}
	}
	if !found {
		return task
	}
	val, err := d.sharedMem.Get(ctx, parentID+":"+salvageSlotPrefix+domain)
	if err != nil || strings.TrimSpace(val) == "" {
		return task
	}
	salvage := truncateRunes(strings.TrimSpace(val), salvageMaxRunes)
	if salvage == "" {
		return task
	}
	log.Printf("[subagent] prior salvage injected: parent=%s domain=%s salvage_len=%d", parentID, domain, len(salvage))
	return task + "\n\n【前序探索摘要】\n" + salvage
}
