// Package memory block_vector.go 提供特性3（domainAgent 后向量检索）所需的
// 块记忆记录结构与 global_knowledge 表之间的转换。
//
// 伪嵌入实现抽到 internal/embed 包，避免 store ↔ memory 循环依赖。
package memory

import (
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// BlockMemoryRecord 块记忆条目：domainAgent 完成一个 SessionBlock 后归档的
// 领域/目标/结果摘要，写入 global_knowledge 表（KnowledgeType="block_memory"）。
type BlockMemoryRecord struct {
	SessionID string    // 所属会话
	Domain    string    // 领域名
	Goal      string    // 领域目标
	Summary   string    // 任务结果摘要
	CreatedAt time.Time // 归档时间
}

// ToKnowledgeRecord 把块记忆转为 global_knowledge 表记录。
// Content 拼装为可读文本，便于检索后直接注入 prompt。
func (r *BlockMemoryRecord) ToKnowledgeRecord(dim int) *types.KnowledgeRecord {
	content := strings.Join([]string{
		"领域:", r.Domain,
		"目标:", r.Goal,
		"结果:", r.Summary,
	}, "\n")
	meta := map[string]any{
		"session_id": r.SessionID,
		"domain":     r.Domain,
		"goal":       r.Goal,
	}
	return &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		TopicID:       r.SessionID,
		Content:       content,
		Embedding:     embed.PseudoEmbed(content, dim),
		Meta:          meta,
		CreatedAt:     r.CreatedAt,
	}
}

