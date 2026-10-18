package bootstrap

// skill_evict.go 经验技能库确定性冷淘汰（2026-10-17）。
//
// 动机：learned_skills 只增不淘，僵尸技能（从未使用/常年不用）永远占住 max_count 的坑，
// 满库后新经验只能走 skill_dropped。冷淘汰按确定性规则（不依赖 LLM）把最冷的 enabled
// 技能归档腾位：第一档 use_count==0 且创建超期；第二档 use_count<=idleMaxUse 且最近使用
// （从未使用按创建时间）超期。归档=软删（SetEnabled(false) + 技能池退池 + evolution_log
// 审计），文件保留可手工复活。

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/internal/skill"
)

// archiveColdSkills 确定性冷淘汰：挑最冷的 need 个 enabled 技能归档
//（SetEnabled(false) + 技能池退池 + evolution_log kind=skill_evict），返回实际归档数。
// 供两处复用：evolver 满库腾位、consolidate 每日兜底。归档=软删（文件保留可手工复活）。
func archiveColdSkills(ctx context.Context, skills learnedSkillStoreOps, pool *skill.Pool, need, coldDays, idleDays, idleMaxUse int) int {
	if skills == nil || need <= 0 {
		return 0
	}
	cands, err := skills.ListColdEnabled(ctx, need, coldDays, idleDays, idleMaxUse)
	if err != nil {
		log.Printf("[skill-evict] list cold skills failed: %v", err)
		return 0
	}
	evicted := 0
	for _, c := range cands {
		if evicted >= need {
			break
		}
		lastUsed := c.CreatedAt
		if c.LastUsedAt != nil {
			lastUsed = *c.LastUsedAt
		}
		if err := skills.SetEnabled(ctx, c.Name, false); err != nil {
			log.Printf("[skill-evict] disable cold skill failed (skip one): name=%s err=%v", c.Name, err)
			continue
		}
		if pool != nil {
			pool.Remove(c.Name)
		}
		if err := skills.AppendEvolutionLog(ctx, "skill_evict", c.Name,
			fmt.Sprintf("满库冷淘汰（use_count=%d，最近使用/创建=%s）：归档腾位，文件保留可手工复活",
				c.UseCount, lastUsed.Format(time.DateTime)), ""); err != nil {
			log.Printf("[skill-evict] evolution log failed (non-fatal): name=%s err=%v", c.Name, err)
		}
		log.Printf("[skill-evict] cold skill archived: name=%s use_count=%d last_used=%s",
			c.Name, c.UseCount, lastUsed.Format(time.DateTime))
		evicted++
	}
	return evicted
}
