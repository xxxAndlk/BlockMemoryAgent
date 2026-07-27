package tool

// shared_memory.go 提供 WriteSharedMemory 工具：主线程 Agent（MetaAgent/DomainAgent）
// 把派发前采集到的关键上下文（文件路径、行号、签名、前置结论）写入 KV 共享记忆，
// 协程 Agent（被 call_sub_agent 派发的子 Agent）经 dispatcher.injectKVMemory 自动读取，
// 实现"主 Agent 浅读一次、子 Agent 不重读全文件"的 token 节约语义。
//
// 权限模型：仅暴露给 MetaAgent（meta 角色白名单含 WriteSharedMemory）；
// 固定助手与 DomainAgent 的白名单不含此工具，调度器侧 Dispatch 路径不受白名单限制
// 但 LLM 看不到该工具 schema，无法主动调用。

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SharedMemoryStore 是 WriteSharedMemory 工具依赖的 KV 写入接口。
// memory.InMemoryKV 隐式实现该接口；接口分离避免 tool 反向依赖 memory 包。
type SharedMemoryStore interface {
	// Set 写入键值对；只读实例返回错误。
	Set(ctx context.Context, key, value string) error
}

// writeSharedMemoryTool 是 WriteSharedMemory 工具的封装。
type writeSharedMemoryTool struct {
	store SharedMemoryStore
}

// Name 返回工具标准名称 WriteSharedMemory。
func (t *writeSharedMemoryTool) Name() string { return "WriteSharedMemory" }

// Aliases 返回 WriteSharedMemory 的别名列表。
func (t *writeSharedMemoryTool) Aliases() []string {
	return []string{"write_shared_memory", "writeSharedMemory"}
}

// Description 返回工具的人类可读描述，供 schema 与 UI 展示。
func (t *writeSharedMemoryTool) Description() string {
	return "把派发前采集到的关键上下文（文件路径、行号、签名、前置结论）写入共享记忆，" +
		"被派发的子 Agent 会自动读取，无需重读全文件。仅 MetaAgent 可用。"
}

// Execute 写入共享记忆。
// 入参 content 为待写入的上下文文本；key 由调用方 agentID 派生（"<agentID>:shared"），
// 与 dispatcher.injectKVMemory 的读取键一致。
func (t *writeSharedMemoryTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.store == nil {
		return &Result{Tool: "WriteSharedMemory", Error: "shared memory store not configured"}
	}
	content, _ := args["content"].(string)
	if strings.TrimSpace(content) == "" {
		return &Result{Tool: "WriteSharedMemory", Error: "content is required"}
	}
	agentID := AgentIDFromContext(ctx)
	if agentID == "" {
		return &Result{Tool: "WriteSharedMemory", Error: "missing agent identity in context"}
	}
	key := agentID + ":shared"
	if err := t.store.Set(ctx, key, content); err != nil {
		return &Result{Tool: "WriteSharedMemory", Error: fmt.Sprintf("set: %v", err)}
	}
	return &Result{
		Tool:    "WriteSharedMemory",
		Success: true,
		Output:  fmt.Sprintf("shared memory written (key=%s, %d chars)", key, len(content)),
	}
}

// writeSharedMemoryInput 是 WriteSharedMemory 工具的入参结构。
// 通过 json tag 暴露给 blades schema 生成器。
type writeSharedMemoryInput struct {
	// Content 是待写入的共享上下文文本。
	Content string `json:"content" description:"待写入共享记忆的上下文文本：关键文件路径、行号、函数签名、前置结论、验收标准。子 Agent 会自动读取，不要塞原始文件全文。"`
}

// errSharedMemoryReadOnly 在 store 为只读 KVMemory 时返回（由 Set 返回 errReadOnly）。
// 工具侧不直接判断，仅作为文档提示。
var errSharedMemoryReadOnly = errors.New("shared memory is read-only")

// _ 确保 errSharedMemoryReadOnly 被 errors.Is 链路覆盖（避免 unused 警告）。
var _ = errSharedMemoryReadOnly
