package tool

// shared_memory.go 提供 WriteSharedMemory 工具：主线程 Agent（MetaAgent/DomainAgent）
// 把派发前采集到的关键上下文（文件路径、行号、签名、前置结论）写入 KV 共享记忆，
// 协程 Agent（被 call_sub_agent 派发的子 Agent）经 dispatcher.injectKVMemory 自动读取，
// 实现"主 Agent 浅读一次、子 Agent 不重读全文件"的 token 节约语义。
//
// 缓存一致性方案（详见 doc/计划_共享记忆一致性.md）：
//   - Layer 1: 入参结构化，KV value 存 JSON {files, content, mtime}，为失效提供 path 反查索引；
//   - Layer 2: WriteFile 成功后 hook 失效引用同 path 的 KV entry（registry.go invalidateSharedMemoryForPath）；
//   - Layer 3: Dispatcher.injectKVMemory 拼接前 stat 各 path 对比 mtime，不匹配丢弃 KV 降级 fresh read。
//
// 权限模型：仅暴露给 MetaAgent（meta 角色白名单含 WriteSharedMemory）；
// 固定助手与 DomainAgent 的白名单不含此工具，调度器侧 Dispatch 路径不受白名单限制
// 但 LLM 看不到该工具 schema，无法主动调用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SharedMemoryStore 是 WriteSharedMemory 工具依赖的 KV 接口，含写入与失效所需方法。
// memory.InMemoryKV 隐式实现该接口；接口分离避免 tool 反向依赖 memory 包。
type SharedMemoryStore interface {
	// Set 写入键值对；只读实例返回错误。
	Set(ctx context.Context, key, value string) error
	// Get 按键读取记忆值，不存在返回空串与 nil error。
	Get(ctx context.Context, key string) (string, error)
	// Delete 删除键，键不存在幂等返回 nil。
	Delete(ctx context.Context, key string) error
	// Keys 返回当前内存中所有键的快照，供失效逻辑反查遍历。
	Keys(ctx context.Context) []string
}

// SharedEntry 是 KV value 的结构化格式。JSON 序列化后存入 sharedKV。
// Files 字段双用：既是 path 反查索引（Layer 2 失效用），又携带 mtime 戳（Layer 3 校验用）。
// 旧格式（纯字符串 content）在 injectKVMemory 中通过 JSON 解析失败兜底兼容。
type SharedEntry struct {
	// Files 是该共享记忆涉及的文件路径到写入时 mtime（Unix 秒）的映射。
	// path 用 filepath.Clean 规范化为绝对路径，保证 WriteFile hook 与 injectKVMemory 比较一致。
	Files map[string]int64 `json:"files,omitempty"`
	// Content 是关键上下文摘要文本，由 LLM 自由组织（路径/行号/签名/前置结论/验收标准）。
	Content string `json:"content"`
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
		"被派发的子 Agent 会自动读取，无需重读全文件。仅 MetaAgent 可用。" +
		"files 字段填涉及的文件路径列表，写入时会记录 mtime；任一文件被 WriteFile 修改后，" +
		"该共享记忆自动失效，下次派发子 Agent 不再注入旧摘要。" +
		"\n覆盖语义：同一 key 的写入会覆盖前一次内容（不追加）。默认 key=\"shared\"；" +
		"如需分多段写入（如 spec/acceptance/data），用不同 key 各写一次，子 Agent 会按字典序拼装全部命名槽位。"
}

// Execute 写入共享记忆。
// 入参 content 为待写入的上下文文本；files 为涉及的文件路径列表（可选）；
// key 为命名槽位（可选，默认 "shared"）。
// 最终存储键为 "<agentID>:<key>"，与 dispatcher.injectKVMemory 的前缀枚举一致。
// value 为 SharedEntry 的 JSON 序列化，含 files 的 mtime 戳供失效与校验。
// 同一 key 的写入覆盖前一次内容（不追加）；不同 key 各自独立存储。
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

	// 解析 files 列表（可选）。LLM 可能传 []any 或不传。
	files := parseFilesArg(args["files"])

	// 解析命名槽位 key（可选，默认 "shared"）。规范化：去空白、禁止含 ":" 避免键解析歧义。
	slot, _ := args["key"].(string)
	slot = strings.TrimSpace(slot)
	if slot == "" {
		slot = "shared"
	}
	if strings.ContainsAny(slot, ": \t\n\r") {
		return &Result{Tool: "WriteSharedMemory", Error: "key must not contain ':', whitespace"}
	}

	// 构建 SharedEntry：对每个 path stat 记录 mtime，path 规范化为绝对路径。
	// stat 失败（文件不存在）的 path 跳过，不进入 Files 索引；其失效与校验不覆盖该 path。
	entry := SharedEntry{Content: content, Files: make(map[string]int64, len(files))}
	for _, p := range files {
		ap, mt, ok := statFile(p)
		if !ok {
			continue
		}
		entry.Files[ap] = mt
	}

	val, err := json.Marshal(entry)
	if err != nil {
		return &Result{Tool: "WriteSharedMemory", Error: fmt.Sprintf("marshal: %v", err)}
	}
	key := agentID + ":" + slot
	if err := t.store.Set(ctx, key, string(val)); err != nil {
		return &Result{Tool: "WriteSharedMemory", Error: fmt.Sprintf("set: %v", err)}
	}
	return &Result{
		Tool:    "WriteSharedMemory",
		Success: true,
		Output:  fmt.Sprintf("shared memory written (key=%s, %d chars, %d files tracked)", key, len(content), len(entry.Files)),
	}
}

// writeSharedMemoryInput 是 WriteSharedMemory 工具的入参结构。
// 通过 json tag 暴露给 blades schema 生成器。
type writeSharedMemoryInput struct {
	// Content 是待写入的共享上下文文本。
	Content string `json:"content" description:"待写入共享记忆的上下文文本：关键文件路径、行号、函数签名、前置结论、验收标准。子 Agent 会自动读取，不要塞原始文件全文。"`
	// Files 是涉及的文件路径列表，用于自动失效与版本校验。任一文件被 WriteFile 修改后该记忆自动失效。可空。
	Files []string `json:"files" description:"涉及的文件路径列表（相对或绝对）。任一文件被 WriteFile 修改后该共享记忆自动失效，避免子 Agent 读到旧摘要。可空。"`
	// Key 是命名槽位，默认 "shared"。同 key 写入覆盖前值；不同 key 各自独立存储，子 Agent 会按字典序拼装全部槽位。
	Key string `json:"key" description:"命名槽位（默认 shared）。同 key 写入覆盖前值；如需分多段写入（spec/acceptance/data）用不同 key 各写一次。可空。"`
}

// parseFilesArg 从 args["files"] 提取字符串列表，兼容 []any / []string / 缺省。
func parseFilesArg(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, p := range vv {
			if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// statFile 规范化 path 为绝对路径并取 mtime（Unix 秒）。
// 文件不存在或 stat 失败返回 ok=false，调用方跳过该 path。
func statFile(p string) (absPath string, mtime int64, ok bool) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", 0, false
	}
	ap, err := filepath.Abs(filepath.Clean(trimmed))
	if err != nil {
		return "", 0, false
	}
	fi, err := os.Stat(ap)
	if err != nil {
		return "", 0, false
	}
	return ap, fi.ModTime().Unix(), true
}

// errSharedMemoryReadOnly 在 store 为只读 KVMemory 时返回（由 Set 返回 errReadOnly）。
// 工具侧不直接判断，仅作为文档提示。
var errSharedMemoryReadOnly = errors.New("shared memory is read-only")

// _ 确保 errSharedMemoryReadOnly 被 errors.Is 链路覆盖（避免 unused 警告）。
var _ = errSharedMemoryReadOnly
