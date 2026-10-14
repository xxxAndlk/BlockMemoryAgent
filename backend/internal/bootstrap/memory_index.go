package bootstrap

// memory_index.go 记忆索引槽装配（TODO #20③+#22③）：会话启动把 global_knowledge
// 沉淀渲染成一行式索引注入系统提示（行数/runes 双配额），详情走向量召回。
// 信任分层：untrusted 围栏（WrapUntrusted 标记）行结构性丢弃——不进 curated 索引、
// 不自动注入（#22③ provenance 门 + 召回循环防护）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// memoryIndexSummaryRunes 单条索引摘要上限（一行式，长文截断）。
const memoryIndexSummaryRunes = 120

// blockMemoryIndexMinScore 是【沉淀索引】目标相关召回的相似度下限。
// 对齐 store 侧 crossSessionBlockMemoryScore=0.5 的实测口径（bge-m3：同项目
// 相关事实 0.50-0.69，弱相关跨任务 ~0.47 及以下）——索引按目标注入每个会话，
// 宁缺毋滥，弱相关不进。
const blockMemoryIndexMinScore = 0.5

// memoryIndexFloorEntries 是相关召回不足时的补足下限：按最近访问顺序补近期条目，
// 保证索引槽在冷目标（闲聊式 goal）下依然有内容可看；补足部分不占满配额。
const memoryIndexFloorEntries = 20

// newMemoryIndexProvider 构造【沉淀索引】渲染器：knowledge 表沉淀 → 过滤 untrusted
// 围栏行 → 一行式摘要 → RenderMemoryIndex 双配额渲染（超限自带重写指令）。
//
// 召回策略（2026-10-13 修复）：优先按会话目标 goal 做向量相关召回（score >=
// blockMemoryIndexMinScore）。原实现按 last_accessed 全局取前 N 条——实证把大量
// 与当前会话无关的沉淀（其它项目的教材改写/新闻/RSS 记忆）注入每个会话，模型
// 在无关条目上混线（"删除微信插件"会话里捞出桥接项目条目转述进结论）。
// 相关召回不足 floor 条时按原全局最近访问顺序补足；goal 为空/embed 失败时整体
// 回落原行为（可用性优先，宁可后退不可不注入）。
//
// 用户画像不入索引——react_agent 的全量【用户画像】段已单独注入 MetaAgent 系统提示，
// 索引再放一条是每轮重复 token（TODO #24 批二⑨）。
func newMemoryIndexProvider(ks *store.KnowledgeStore, maxLines, maxRunes int) func(goal string) string {
	lineCap := maxLines
	if lineCap <= 0 {
		lineCap = 200
	}
	return func(goal string) string {
		if ks == nil {
			return ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var recs []*types.KnowledgeRecord
		if goal != "" {
			if rel, err := ks.SearchBlockMemoryRelevant(ctx, goal, lineCap, blockMemoryIndexMinScore); err == nil {
				recs = rel
			}
		}
		// 相关召回不足（弱目标/无沉淀/embed 失败）：按最近访问顺序补足到下限。
		// 多取一倍再过滤 untrusted，保证配额内的行都是可信沉淀（同原实现）。
		if len(recs) < memoryIndexFloorEntries {
			if recent, err := ks.ListIndexEntries(ctx, lineCap*2); err == nil {
				have := make(map[int64]bool, len(recs))
				for _, r := range recs {
					if r != nil {
						have[r.ID] = true
					}
				}
				for _, r := range recent {
					if len(recs) >= memoryIndexFloorEntries {
						break
					}
					if r != nil && !have[r.ID] {
						recs = append(recs, r)
					}
				}
			}
		}
		if len(recs) == 0 {
			return ""
		}
		entries := make([]agent.MemoryIndexEntry, 0, len(recs))
		for _, r := range recs {
			if r == nil || agent.ContainsUntrustedFence(r.Content) {
				// provenance 门：untrusted 内容结构性禁止进 curated 层与自动注入。
				continue
			}
			summary := strings.ReplaceAll(strings.TrimSpace(r.Content), "\n", " ")
			if rr := []rune(summary); len(rr) > memoryIndexSummaryRunes {
				summary = string(rr[:memoryIndexSummaryRunes]) + "…"
			}
			entries = append(entries, agent.MemoryIndexEntry{Type: string(r.KnowledgeType), Summary: summary})
		}
		block, _ := agent.RenderMemoryIndex(entries, maxLines, maxRunes)
		if block == "" {
			return ""
		}
		return fmt.Sprintf("【记忆索引】\n%s", block)
	}
}
