package bootstrap

// memory_lifecycle_wire.go —— 长期记忆生命周期装配辅助（2026-10-24 撤回与污染修复）。
//
// 职责：
//  1. 显式记忆写入（remember_preference / 项目约定工具）-> 结构化条目，
//     激活前过防复活检查（命中 tombstone 落 candidate，不进入注入视图）；
//  2. Markdown -> items 的一次性迁移（用户画像 / 项目偏好各一份）。

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/store"
)

// writeExplicitMemoryItem 把一条用户显式写入的偏好/约定落成结构化记忆条目。
// 作用域与 memory_key 由内容推断；命中已撤回内容时只落 candidate（不注入），
// 并在 events 里记 resurrection_suspect。
func writeExplicitMemoryItem(ctx context.Context, lc *agent.MemoryLifecycle, scope, projectKey, origin, content string) {
	if lc == nil || !lc.Enabled() || strings.TrimSpace(content) == "" {
		return
	}
	lc.RecordExplicitItem(ctx, agent.MemoryScopeContext{
		ProjectKey: projectKey,
		SessionID:  tool.SessionIDFromContext(ctx),
	}, scope, origin, content)
}

// importMemoryFromMarkdown 把一份 Markdown 记忆文件导入长期记忆（幂等：已有条目则跳过）。
// 在装配期调用，失败只告警（不阻断启动：活动记忆仍可正常工作）。
func importMemoryFromMarkdown(lc *agent.MemoryLifecycle, path, scope, projectKey string) {
	if lc == nil || path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // 文件不存在 = 无历史记忆，零副作用
	}
	owner := ""
	n, reason := lc.ImportMarkdown(context.Background(), owner, scope,
		store.NormalizeProjectKey(projectKey), string(data))
	if reason != "" && reason != "already imported" {
		log.Printf("[bootstrap] long-term memory import skipped (%s %s): %s", scope, path, reason)
		return
	}
	if n > 0 {
		log.Printf("[bootstrap] long-term memory imported: scope=%s items=%d path=%s", scope, n, path)
	}
}
