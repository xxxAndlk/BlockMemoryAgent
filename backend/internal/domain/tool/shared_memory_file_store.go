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
// root 为 .bma/shared 绝对路径；构造时 MkdirAll。
type FileSharedMemoryStore struct {
	root string
}

// NewFileSharedMemoryStore 创建文件后端共享记忆存储。
// workDir 为工作目录（通常 os.Getwd）；实际 root 为 <workDir>/.bma/shared。
func NewFileSharedMemoryStore(workDir string) *FileSharedMemoryStore {
	root := filepath.Join(workDir, ".bma", "shared")
	_ = os.MkdirAll(root, 0o755)
	return &FileSharedMemoryStore{root: root}
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
	path := s.filePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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
	path := s.filePath(key)
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
	err := os.Remove(s.filePath(key))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// Keys 返回 root 下所有 MD 文件对应的 KV key（desanitize 还原）。
// 供 injectKVMemory 枚举 parentID: 前缀的所有槽位，与 invalidateSharedMemoryForPath 遍历。
func (s *FileSharedMemoryStore) Keys(ctx context.Context) []string {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.root)
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

// Clear 删除 root 下所有 MD 文件，幂等。供新 session 启动清理旧 session 残留
//（spec/file_tree 不跨 session 复用，丢历史无损失）。非 file-store 后端不实现此方法。
func (s *FileSharedMemoryStore) Clear(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("file shared memory store not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.root)
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
		if err := os.Remove(filepath.Join(s.root, e.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

// filePath 返回 key 对应的绝对文件路径。
func (s *FileSharedMemoryStore) filePath(key string) string {
	return filepath.Join(s.root, sanitizeKey(key)+".md")
}

// sanitizeKey 把 KV key "<agentID>:<slot>" 转为安全文件名。
// agentID 段 hex 编码（可逆，兼容 "/" 与 "_"），slot 段原样保留。
// 无 ":" 时整段 hex 编码（防御性，正常路径不触发）。
func sanitizeKey(key string) string {
	idx := strings.Index(key, ":")
	if idx < 0 {
		return hex.EncodeToString([]byte(key))
	}
	agentID := key[:idx]
	slot := key[idx+1:]
	return hex.EncodeToString([]byte(agentID)) + "__" + slot
}

// desanitizeKey 反向 sanitizeKey。
// 还原 "<hex>__<slot>" -> "<agentID>:<slot>"。
// 解析失败（hex 非法 / 无 "__" 分隔）返回 ok=false。
func desanitizeKey(stem string) (string, bool) {
	idx := strings.Index(stem, "__")
	if idx < 0 {
		// 无分隔符：整体 hex 解码
		agentBytes, err := hex.DecodeString(stem)
		if err != nil {
			return "", false
		}
		return string(agentBytes), true
	}
	hexAgent := stem[:idx]
	slot := stem[idx+2:]
	agentBytes, err := hex.DecodeString(hexAgent)
	if err != nil {
		return "", false
	}
	return string(agentBytes) + ":" + slot, true
}
