package tool

// shared_memory.go 提供 WriteSharedMemory / ReadSharedMemory 工具：主线程 Agent（MetaAgent/DomainAgent）
// 把派发前采集到的关键上下文（文件路径、行号、签名、前置结论）写入共享记忆，
// 协程 Agent（被 call_sub_agent 派发的子 Agent）经 dispatcher.injectKVMemory 自动读取，
// 实现"主 Agent 浅读一次、子 Agent 不重读全文件"的 token 节约语义。
// ReadSharedMemory 供主 Agent 派发/返工前读回已写入槽位（Meta 无 ReadFile 的信息源）。
//
// 存储后端：FileSharedMemoryStore 落盘到 <workDir>/.bma/shared/<hex(agentID)>__<slot>.md，
// MD 格式：YAML frontmatter（agent/slot/files mtime）+ body（content 原文）。
//
// 缓存一致性方案：
//   - Layer 1: 入参结构化，MD frontmatter 存 files mtime map，为失效提供 path 反查索引；
//   - Layer 2: WriteFile 成功后 hook 失效引用同 path 的 entry（registry.go invalidateSharedMemoryForPath）；
//   - Layer 3: Dispatcher.injectKVMemory 拼接前 stat 各 path 对比 mtime，不匹配丢弃降级 fresh read。
//
// 权限模型：仅暴露给 MetaAgent（meta 角色白名单含 WriteSharedMemory）；
// DomainAgent 也可用（domain 模板白名单含此工具）；固定助手不暴露。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SharedMemoryStore 是 WriteSharedMemory/WriteSpec 工具依赖的存储接口。
// FileSharedMemoryStore 实现该接口（落盘到 .bma/shared/）；测试用 fakeSharedMemoryStore。
// 接口分离避免 tool 反向依赖 memory 包。
type SharedMemoryStore interface {
	// Set 写入键值对；只读实例返回错误。
	Set(ctx context.Context, key, value string) error
	// Get 按键读取记忆值，不存在返回空串与 nil error。
	Get(ctx context.Context, key string) (string, error)
	// Delete 删除键，键不存在幂等返回 nil。
	Delete(ctx context.Context, key string) error
	// Keys 返回所有键的快照，供失效逻辑反查遍历与 injectKVMemory 枚举槽位。
	Keys(ctx context.Context) []string
}

// ErrVersionConflict 是 SetIfVersion 的冲突哨兵：当前版本与期望版本不一致，
// 说明期间有并发写入（兄弟 Agent 覆盖），调用方应放弃写入或重读后重试。
var ErrVersionConflict = errors.New("shared memory version conflict")

// versionedSharedMemoryStore 是 SharedMemoryStore 的可选扩展接口：
// 带版本期望的 CAS 写入（乐观锁，等价 AICP 黑板 Lua 原子写 + version 字段）。
// 未实现该接口的存储后端退化为普通 Set 覆盖写（行为不变）。
type versionedSharedMemoryStore interface {
	// SetIfVersion 仅当当前版本等于 expectVersion 时写入（并自增至新版本返回）。
	// 无既有值时版本视为 0。版本不一致返回 ErrVersionConflict。
	SetIfVersion(ctx context.Context, key, value string, expectVersion int) (int, error)
}

// sharedMemoryVersion 读取存储中 key 当前值的版本号（无值/非 MD 格式视为 0）。
func sharedMemoryVersion(ctx context.Context, store SharedMemoryStore, key string) int {
	cur, err := store.Get(ctx, key)
	if err != nil || strings.TrimSpace(cur) == "" {
		return 0
	}
	if fm, _, ok := DecodeSharedMD(cur); ok {
		return fm.Version
	}
	return 0
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
// value 为 MD 字符串（encodeSharedMD 生成），frontmatter 含 files mtime 供失效与校验。
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

	// 构建 files mtime 索引：stat 各 path，规范化为绝对路径。
	// stat 失败（文件不存在）的 path 跳过，不进入 Files 索引；其失效与校验不覆盖该 path。
	filesMtime := make(map[string]int64, len(files))
	for _, p := range files {
		ap, mt, ok := statFile(p)
		if !ok {
			continue
		}
		filesMtime[ap] = mt
	}

	// 编码为 MD：frontmatter 含 agent/slot/files mtime，body 为 content 原文。
	md := encodeSharedMD(agentID, slot, filesMtime, content)

	key := agentID + ":" + slot
	// 单写多读 slot 的写入方校验：file_tree 等共享 slot 只允许首个写入者更新，
	// 兄弟 Agent 拿到 parentID 也能写任意 key，防互相覆盖（AICP 黑板 per-agent 前缀隔离轻量版）。
	if err := t.checkSlotOwnership(ctx, key, slot, agentID); err != nil {
		return &Result{Tool: "WriteSharedMemory", Error: err.Error()}
	}
	// 乐观锁写入：存储后端支持版本 CAS 时走 SetIfVersion（冲突重读重试一次），否则退化普通覆盖。
	newVersion, err := t.writeWithCAS(ctx, key, md)
	if err != nil {
		return &Result{Tool: "WriteSharedMemory", Error: fmt.Sprintf("set: %v", err)}
	}
	return &Result{
		Tool:    "WriteSharedMemory",
		Success: true,
		Output:  fmt.Sprintf("shared memory written (key=%s, version=%d, %d chars, %d files tracked)", key, newVersion, len(content), len(filesMtime)),
	}
}

// singleWriterSlots 是单写多读的共享记忆 slot：仅首个写入者（AgentID）可更新，
// 兄弟 Agent 只读。防拿到 parentID 的任意子 Agent 覆盖共享探索成果
// （实证风险：file_tree 并发互覆导致后续兄弟 Agent 读旧树）。
var singleWriterSlots = map[string]bool{"file_tree": true}

// checkSlotOwnership 校验单写多读 slot 的写入权：slot 已由其他 Agent 写入时拒绝，
// 提示只读复用。多写 slot（非 singleWriterSlots）不做 owner 校验（靠 CAS 防并发覆盖）。
func (t *writeSharedMemoryTool) checkSlotOwnership(ctx context.Context, key, slot, agentID string) error {
	if !singleWriterSlots[slot] || t.store == nil {
		return nil
	}
	existing, err := t.store.Get(ctx, key)
	if err != nil || strings.TrimSpace(existing) == "" {
		return nil // 无既有值：首个写入者，允许。
	}
	fm, _, ok := DecodeSharedMD(existing)
	if !ok || fm.AgentID == "" || fm.AgentID == agentID {
		return nil // 旧格式无 owner / 本人即 owner：允许覆盖。
	}
	return fmt.Errorf("slot %q 为单写多读：已由 Agent %s 写入，你是 %s，只读不可覆盖。请直接复用现有共享记忆，或用不同 key 另写新段",
		slot, fm.AgentID, agentID)
}

// writeWithCAS 走版本 CAS 写入；冲突重读重试一次，仍冲突则返回错误（交 LLM 决策，避免盲覆盖）。
// 存储后端不支持版本接口时退化为普通 Set 覆盖写（行为不变）。
func (t *writeSharedMemoryTool) writeWithCAS(ctx context.Context, key, md string) (int, error) {
	vs, ok := t.store.(versionedSharedMemoryStore)
	if !ok {
		if err := t.store.Set(ctx, key, md); err != nil {
			return 0, err
		}
		return 0, nil
	}
	const maxCASAttempts = 2 // 首次 + 冲突后重试一次
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		expect := sharedMemoryVersion(ctx, t.store, key)
		newVer, err := vs.SetIfVersion(ctx, key, md, expect)
		if err == nil {
			return newVer, nil
		}
		if !errors.Is(err, ErrVersionConflict) {
			return 0, err
		}
		if attempt == maxCASAttempts-1 {
			return 0, fmt.Errorf("%w: 该 slot 并发被其他 Agent 更新，请重读最新内容后再决定写入", ErrVersionConflict)
		}
	}
	return 0, fmt.Errorf("%w", ErrVersionConflict)
}

// readSharedMemoryTool 是 ReadSharedMemory 工具的封装：主 Agent 派发/返工前读回
// 已写入的共享记忆（spec/契约/前置结论），作为自身无 ReadFile 时的信息源。
type readSharedMemoryTool struct {
	store SharedMemoryStore
}

// readSharedMemoryTruncateLimit 是单槽位读回正文的截断上限：
// 共享记忆按设计存"摘要/契约"而非原文全文，超限视为写入方违反写入纪律，读回截断保护上下文。
const readSharedMemoryTruncateLimit = 20000

// Name 返回工具标准名称 ReadSharedMemory。
func (t *readSharedMemoryTool) Name() string { return "ReadSharedMemory" }

// Aliases 返回 ReadSharedMemory 的别名列表。
func (t *readSharedMemoryTool) Aliases() []string {
	return []string{"read_shared_memory", "readSharedMemory"}
}

// Description 返回工具的人类可读描述，供 schema 与 UI 展示。
func (t *readSharedMemoryTool) Description() string {
	return readSharedMemoryToolDescription()
}

// readSharedMemoryToolDescription 是 ReadSharedMemory 的描述文本（blades 桥接层复用）。
func readSharedMemoryToolDescription() string {
	return "读回共享记忆。key 省略时列出当前全部槽位（key 与字数）；指定 key 读回该槽位全文。" +
		"key 可传全键 \"<agentID>:<slot>\" 或仅 slot（自动用你的 agentID 前缀解析，未命中时全库唯一同名槽位回退）。" +
		"用途：返工/修复前重读上次写入的 spec/契约/前置结论；派发前核对已写入内容。" +
		"被派发的子 Agent 会自动注入共享记忆，无需为它们调用本工具。"
}

// readSharedMemoryInput 是 ReadSharedMemory 工具的入参结构。
type readSharedMemoryInput struct {
	// Key 是要读回的槽位；省略时列出全部槽位名与字数。
	Key string `json:"key" description:"要读回的槽位：全键 \"<agentID>:<slot>\" 或仅 slot（自动用你的 agentID 前缀解析）。省略时只列出全部槽位名与字数。可空。"`
}

// Execute 读回共享记忆：无 key 枚举槽位清单，有 key 返回槽位正文。
func (t *readSharedMemoryTool) Execute(ctx context.Context, args map[string]any) *Result {
	if t.store == nil {
		return &Result{Tool: "ReadSharedMemory", Error: "shared memory store not configured"}
	}
	key, _ := args["key"].(string)
	key = strings.TrimSpace(key)

	keys := t.store.Keys(ctx)
	sort.Strings(keys)

	if key == "" {
		return t.listResult(ctx, keys)
	}

	val, hitKey := t.resolve(ctx, key, keys)
	if !hitKey {
		return &Result{Tool: "ReadSharedMemory", Error: fmt.Sprintf(
			"shared memory %q 不存在。可用槽位：\n%s", key, listKeysSummary(ctx, t.store, keys))}
	}
	return &Result{Tool: "ReadSharedMemory", Success: true, Output: decodeSharedBody(val)}
}

// resolve 按"本 Agent 前缀 -> 全键"顺序解析 key，返回命中内容。
func (t *readSharedMemoryTool) resolve(ctx context.Context, key string, keys []string) (string, bool) {
	if !strings.Contains(key, ":") {
		if agentID := AgentIDFromContext(ctx); agentID != "" {
			if val, err := t.store.Get(ctx, agentID+":"+key); err == nil && strings.TrimSpace(val) != "" {
				return val, true
			}
		}
	}
	val, err := t.store.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		// 仅 slot 且带本 Agent 前缀未命中时，回退全库唯一候选（跨 Agent 读，如 Meta 读子 Agent 沉淀）。
		if !strings.Contains(key, ":") {
			var cands []string
			for _, k := range keys {
				if _, slot, ok := strings.Cut(k, ":"); ok && slot == key {
					cands = append(cands, k)
				}
			}
			if len(cands) == 1 {
				if val, err := t.store.Get(ctx, cands[0]); err == nil && strings.TrimSpace(val) != "" {
					return val, true
				}
			}
		}
		return "", false
	}
	return val, true
}

// listResult 输出全部槽位的 key 与字数摘要。
func (t *readSharedMemoryTool) listResult(ctx context.Context, keys []string) *Result {
	if len(keys) == 0 {
		return &Result{Tool: "ReadSharedMemory", Success: true, Output: "共享记忆为空。"}
	}
	return &Result{Tool: "ReadSharedMemory", Success: true,
		Output: "共享记忆槽位（用 ReadSharedMemory(key=...) 读全文）：\n" + listKeysSummary(ctx, t.store, keys)}
}

// listKeysSummary 生成 "key — N 字符" 逐行摘要。
func listKeysSummary(ctx context.Context, store SharedMemoryStore, keys []string) string {
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		n := 0
		if val, err := store.Get(ctx, k); err == nil {
			_, body, ok := DecodeSharedMD(val)
			if ok {
				val = body
			}
			n = len([]rune(val))
		}
		lines = append(lines, fmt.Sprintf("- %s — %d 字符", k, n))
	}
	return strings.Join(lines, "\n")
}

// decodeSharedBody 解出槽位正文；非 MD 格式原样返回，超限截断。
func decodeSharedBody(val string) string {
	if _, body, ok := DecodeSharedMD(val); ok {
		val = body
	}
	runes := []rune(val)
	if len(runes) > readSharedMemoryTruncateLimit {
		return string(runes[:readSharedMemoryTruncateLimit]) +
			fmt.Sprintf("\n\n[截断：全文 %d 字符，仅显示前 %d。写入方应存摘要而非原文全文]", len(runes), readSharedMemoryTruncateLimit)
	}
	return val
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

// growingDirNames 是系统自身持续写入的目录名（路径段级匹配）：
// 其下文件的 mtime 恒变（TUI/Agent 日志、.bma 内部元数据），spec/共享摘要记录它们必然 stale——
// "系统自己改自己"，与"别的 Agent 改了源码"（#12 的失效语义）必须区分。
// 对这类文件豁免 mtime 校验、降级为存在性检查，否则"分析活体日志"类任务机制上永远派发不出去。
var growingDirNames = map[string]bool{"logs": true, ".bma": true}

// IsGrowingPath 判断 path 任一路径段命中持续增长目录（logs/.bma）。
// 路径段级匹配：workdir 下的 logs/tui/xxx.log、项目子目录 tower-defense/logs/ 均豁免；
// 源码目录恰好名为 logs 的罕见场景接受误豁免（该类目录下 mtime 校验本身不可靠）。
// 供 WriteSpec 告警与 subagent.verifyFileMtimes 豁免复用，保证两侧判定一致。
func IsGrowingPath(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if growingDirNames[seg] {
			return true
		}
	}
	return false
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
