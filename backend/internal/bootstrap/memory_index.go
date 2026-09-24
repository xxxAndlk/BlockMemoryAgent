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
)

// memoryIndexSummaryRunes 单条索引摘要上限（一行式，长文截断）。
const memoryIndexSummaryRunes = 120

// newMemoryIndexProvider 构造【沉淀索引】渲染器：knowledge 表沉淀 → 过滤 untrusted
// 围栏行 → 一行式摘要 → RenderMemoryIndex 双配额渲染（超限自带重写指令）。
// 用户画像不入索引——react_agent 的全量【用户画像】段已单独注入 MetaAgent 系统提示，
// 索引再放一条是每轮重复 token（TODO #24 批二⑨）。
func newMemoryIndexProvider(ks *store.KnowledgeStore, maxLines, maxRunes int) func() string {
	return func() string {
		if ks == nil {
			return ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// 多取一倍再过滤 untrusted，保证配额内的行都是可信沉淀。
		recs, err := ks.ListIndexEntries(ctx, maxLines*2)
		if err != nil || len(recs) == 0 {
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
