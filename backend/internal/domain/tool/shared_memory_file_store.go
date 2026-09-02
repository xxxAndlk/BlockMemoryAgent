package tool

// shared_memory_file_store.go 提供基于文件系统的 SharedMemoryStore 实现。
//
// 替代 memory.InMemoryKV：所有共享记忆/spec 落盘到 <workDir>/.bma/shared/ 下 MD 文件，
// 进程重启后文件仍存在（当前会话不主动恢复，避免旧 spec 复用；启动时目录空由调用方保证）。
//
// 键名映射：KV key "<agentID>:<slot>" 经 sanitizeKey 转为文件名 "<safe>.md"。
// agentID 可能含 "/"（子 Agent ID 如 "session-1/domain-1"）与 "_"（roleID 如 code_assistant），
// 为保证 desanitize 可逆，agentID 段 hex 编码，slot 段原样保留（slot 为简单标识符如 spec/shared）。
// 文件名形如 "<hex(agentID)>__<slot>.md"。

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileSharedMemoryStore 是基于文件的 SharedMemoryStore 实现。
// root 为 .bma/shared 绝对路径；构造时 MkdirAll 默认目录。
// S2 起按 ctx 会话工作目录解析根目录（root(ctx)），ctx 未注入时回落构造目录。
type FileSharedMemoryStore struct {
	workDir string
}

// NewFileSharedMemoryStore 创建文件后端共享记忆存储。
// workDir 为工作目录（通常 os.Getwd）；实际 root 为 <workDir>/.bma/shared。
func NewFileSharedMemoryStore(workDir string) *FileSharedMemoryStore {
	s := &FileSharedMemoryStore{workDir: workDir}
	_ = os.MkdirAll(s.root(context.Background()), 0o755)
	return s
}

// root 按会话解析共享记忆根目录：ctx 携带的会话目录优先，空回退构造目录。
func (s *FileSharedMemoryStore) root(ctx context.Context) string {
	if wd := WorkDirFromContext(ctx); wd != "" {
		return filepath.Join(wd, ".bma", "shared")
	}
	return filepath.Join(s.workDir, ".bma", "shared")
}

// WorkDirOf 返回 ctx 生效的工作目录（供 WriteSpec baseline 落盘）。
func (s *FileSharedMemoryStore) WorkDirOf(ctx context.Context) string {
	if s == nil {
		return ""
	}
	if wd := WorkDirFromContext(ctx); wd != "" {
		return wd
	}
	return s.workDir
}

// Set 把 value 写入 <root>/<sanitized_key>.md。
// value 应为 encodeSharedMD/encodeSpecMD 产生的 MD 字符串。
// 文件已存在则覆盖（与 KV 语义一致）。
func (s *FileSharedMemoryStore) Set(ctx context.Context, key, value string) error {
	if s == nil {
		return fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.filePath(ctx, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// SetIfVersion 实现 versionedSharedMemoryStore 可选接口：CAS 乐观锁写入。
// 读当前文件 frontmatter 版本（无值/非 MD 视为 0），与 expectVersion 不一致返回
// ErrVersionConflict（期间有并发写入）；一致则把 value 的 frontmatter Version 置为
// expectVersion+1 后落盘并返回新版本。
func (s *FileSharedMemoryStore) SetIfVersion(ctx context.Context, key, value string, expectVersion int) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path := s.filePath(ctx, key)
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	curVersion := 0
	if len(cur) > 0 {
		if fm, _, ok := DecodeSharedMD(string(cur)); ok {
			curVersion = fm.Version
		}
		// 非 MD 旧格式：无版本概念，视为 0（可被期望 0 的写入覆盖）。
	}
	if curVersion != expectVersion {
		return 0, fmt.Errorf("%w: key=%s expect=%d got=%d", ErrVersionConflict, key, expectVersion, curVersion)
	}
	newVersion := expectVersion + 1
	value = stampVersion(value, newVersion)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	return newVersion, nil
}

// stampVersion 把 MD 值 frontmatter 的 Version 置为 v 后重编码。
// 值非 MD 格式（无法解码）时原样返回（无版本信息可注入）。
func stampVersion(value string, v int) string {
	fm, body, ok := DecodeSharedMD(value)
	if !ok {
		return value
	}
	fm.Version = v
	return encodeMD(fm, body)
}

// Get 读取 key 对应的 MD 文件内容。
// 文件不存在返回 ("", nil)（与 KV 语义一致：缺失不报错）。
func (s *FileSharedMemoryStore) Get(ctx context.Context, key string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := s.filePath(ctx, key)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// Delete 删除 key 对应文件，文件不存在幂等返回 nil。
func (s *FileSharedMemoryStore) Delete(ctx context.Context, key string) error {
	if s == nil {
		return fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(s.filePath(ctx, key))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// WorkDir 返回 store 的构造工作目录（不含 ctx 会话覆盖；按会话解析见 WorkDirOf）。
// 供 WriteSpec 的 baseline_content 内联落盘定位默认 <workDir>/.bma/baseline/；非文件后端返回空串。
func (s *FileSharedMemoryStore) WorkDir() string {
	if s == nil {
		return ""
	}
	return s.workDir
}

// Keys 按 ctx 会话目录解析 root，返回其下所有 MD 文件对应的 KV key（desanitize 还原）。
// 供 injectKVMemory 枚举 parentID: 前缀的所有槽位，与 invalidateSharedMemoryForPath 遍历。
func (s *FileSharedMemoryStore) Keys(ctx context.Context) []string {
	if s == nil {
		return nil
	}
	root := s.root(ctx)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		stem := strings.TrimSuffix(name, ".md")
		if key, ok := desanitizeKey(stem); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// Clear 按 ctx 会话目录解析 root，删除其下所有 MD 文件，幂等。供新 session 启动清理旧
// session 残留（spec/file_tree 不跨 session 复用，丢历史无损失）。非 file-store 后端不实现此方法。
func (s *FileSharedMemoryStore) Clear(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root := s.root(ctx)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read shared dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(root, e.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

// filePath 按 ctx 会话目录解析 root，返回 key 对应的绝对文件路径。
func (s *FileSharedMemoryStore) filePath(ctx context.Context, key string) string {
	return filepath.Join(s.root(ctx), sanitizeKey(key)+".md")
}

// sanitizeKey 把 KV key "<agentID>:<slot>" 转为安全文件名。
// agentID 段 hex 编码（可逆，兼容 "/" 与 "_"），slot 段原样保留（slot 为简单标识符如 spec/shared）。
// slot 含 Windows 文件名非法字符（":\/<>|?*" 或控制字符）时整键 hex 编码——
// 多 key spec 的 slot 为 "spec:<key>" 含 ":"，Windows 上 ":xx" 会被当 NTFS 备用数据流（ADS）：
// 写入落在 ADS、主文件 0 字节且无 .md 后缀，Keys() 枚举不到（实证 2026-08-25 水果忍者
// key=spec:fruit-ninja 派发连续 spec missing）。
// 无 ":" 时整段 hex 编码（防御性，正常路径不触发）。
func sanitizeKey(key string) string {
	agentID, slot, found := strings.Cut(key, ":")
	if !found {
		return hex.EncodeToString([]byte(key))
	}
	if safeFilenameSlot(slot) {
		return hex.EncodeToString([]byte(agentID)) + "__" + slot
	}
	return hex.EncodeToString([]byte(key))
}

// safeFilenameSlot 判断 slot 是否可直接用作文件名段（Windows 合法且不歧义）。
// 非法字符含 Windows 保留字符 "<>:\"/\\|?*" 与控制字符（<0x20）。
// 中文等 Unicode 字符在 NTFS 合法，保持原样可读。
func safeFilenameSlot(slot string) bool {
	for _, r := range slot {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return false
		}
	}
	return true
}

// desanitizeKey 反向 sanitizeKey。
// 先试整 stem hex 解码（slot 非法字符的整键编码，含原 ":" 结构）；
// 失败回退旧格式 "<hex>__<slot>"（旧文件兼容：含 "__" 必非 hex，两分支无歧义）。
// 旧格式解析失败（hex 非法 / 无 "__" 分隔）返回 ok=false。
func desanitizeKey(stem string) (string, bool) {
	if agentBytes, err := hex.DecodeString(stem); err == nil {
		return string(agentBytes), true
	}
	hexAgent, slot, found := strings.Cut(stem, "__")
	if !found {
		return "", false
	}
	agentBytes, err := hex.DecodeString(hexAgent)
	if err != nil {
		return "", false
	}
	return string(agentBytes) + ":" + slot, true
}
