package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
)

// AgentRegistryStore 是 Agent 注册表与决策日志相关的 PostgreSQL 存储子层。
// 职责: agent_registry 表的 UPSERT/读取，以及 decision_logs 表的写入。
type AgentRegistryStore struct {
	db *sql.DB // 共享连接池
}

// Register 注册或更新 Agent 元信息 (UPSERT)。
// 参数:
//   - id, name, description, moduleID: 基础标识
//   - keywords, dependencies, capabilities: 三个标签切片,分别序列化为 JSONB
//
// 返回: SQL 执行错误。
// 设计意图: 让 MetaAgent 在动态创建 Agent 时持久化注册信息。
func (s *AgentRegistryStore) Register(ctx context.Context, id, name, description, moduleID string, keywords, dependencies, capabilities []string) error {
	// 三个切片分别 JSON 序列化,空切片写成 "[]"
	kw, _ := json.Marshal(keywords)
	deps, _ := json.Marshal(dependencies)
	caps, _ := json.Marshal(capabilities)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_registry (id, name, description, module_id, keywords, dependencies, capabilities, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (id) DO UPDATE
		SET name = $2, description = $3, module_id = $4, keywords = $5, dependencies = $6, capabilities = $7
	`, id, name, description, moduleID, kw, deps, caps)
	return err
}

// GetAll 列出全部已注册 Agent。
// 返回: 每行以 map[string]any 形式返回,keywords 等仍为 JSON 字节。
// 设计意图: 给路由/调度器读取注册表,字段保持原始 JSONB 字节由调用方按需解析。
func (s *AgentRegistryStore) GetAll(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, module_id, keywords, dependencies, capabilities
		FROM agent_registry
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, name, description, moduleID string
		var keywords, dependencies, capabilities []byte
		if err := rows.Scan(&id, &name, &description, &moduleID, &keywords, &dependencies, &capabilities); err != nil {
			// 单行扫描失败跳过,继续累积其他行
			continue
		}
		results = append(results, map[string]any{
			"id":           id,
			"name":         name,
			"description":  description,
			"module_id":    moduleID,
			"keywords":     keywords,
			"dependencies": dependencies,
			"capabilities": capabilities,
		})
	}
	return results, rows.Err()
}

// SaveDecisionLog 记录一条决策日志。
// 参数:
//   - topicID, agentID, decision: 决策主体与文本
//   - context: 附加上下文,序列化为 JSONB
//
// 返回: SQL 执行错误。
// 副作用: created_at 由数据库 NOW() 生成。
func (s *AgentRegistryStore) SaveDecisionLog(ctx context.Context, topicID, agentID, decision string, context map[string]any) error {
	ctxData, _ := json.Marshal(context)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO decision_logs (topic_id, agent_id, decision, context, created_at)
		VALUES ($1, $2, $3, $4, NOW())
	`, topicID, agentID, decision, ctxData)
	return err
}
