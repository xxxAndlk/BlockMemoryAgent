package tool

// shared_memory_cas_test.go 验证共享记忆 slot 写入方校验 + version 乐观锁（TODO #17 P1）：
//   - store 级：SetIfVersion CAS 语义（创建/递增/冲突/非 MD 旧值）；
//   - 工具级：file_tree 单写多读 owner 校验，兄弟 Agent 覆盖被拒；
//   - 多写 slot 不校验 owner，仅靠 CAS 防并发覆盖。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFileSharedMemoryStore_SetIfVersion 验证 CAS 写入全语义：
// 无值创建 v1、同版本递增、版本不一致返 ErrVersionConflict、非 MD 旧值视为 v0。
func TestFileSharedMemoryStore_SetIfVersion(t *testing.T) {
	dir := t.TempDir()
	s := NewFileSharedMemoryStore(dir)
	ctx := context.Background()
	key := "meta-1:file_tree"

	// 无既有值：expect=0 创建 v1，frontmatter 落版本号。
	v, err := s.SetIfVersion(ctx, key, EncodeSharedMD("meta-1", "file_tree", nil, "tree v1"), 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v != 1 {
		t.Fatalf("expected version 1 on create, got %d", v)
	}
	val, _ := s.Get(ctx, key)
	fm, body, ok := DecodeSharedMD(val)
	if !ok || fm.Version != 1 || body != "tree v1" {
		t.Fatalf("unexpected stored value: version=%d body=%q ok=%v", fm.Version, body, ok)
	}

	// 同版本更新：expect=1 → v2。
	v, err = s.SetIfVersion(ctx, key, EncodeSharedMD("meta-1", "file_tree", nil, "tree v2"), 1)
	if err != nil || v != 2 {
		t.Fatalf("update: v=%d err=%v", v, err)
	}

	// 版本不一致（期间有并发写入）：返 ErrVersionConflict，内容不被覆盖。
	_, err = s.SetIfVersion(ctx, key, EncodeSharedMD("meta-1", "file_tree", nil, "stale v3"), 0)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected ErrVersionConflict on stale expect, got %v", err)
	}
	val, _ = s.Get(ctx, key)
	if fm, _, _ = DecodeSharedMD(val); fm.Version != 2 {
		t.Fatalf("conflict write must not mutate version, got %d", fm.Version)
	}

	// 不存在的 key + expect>0：版本不一致（视为 0）。
	if _, err = s.SetIfVersion(ctx, "meta-9:nope", EncodeSharedMD("meta-9", "nope", nil, "x"), 3); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected conflict on missing key with expect>0, got %v", err)
	}

	// 非 MD 旧格式值：版本视为 0，expect=0 覆盖并转 MD 带版本。
	if err := s.Set(ctx, "meta-2:old", "plain legacy string"); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	v, err = s.SetIfVersion(ctx, "meta-2:old", EncodeSharedMD("meta-2", "old", nil, "now md"), 0)
	if err != nil || v != 1 {
		t.Fatalf("legacy overwrite: v=%d err=%v", v, err)
	}
	val, _ = s.Get(ctx, "meta-2:old")
	fm, _, _ = DecodeSharedMD(val)
	if fm.Version != 1 {
		t.Fatalf("expected version 1 after legacy overwrite, got %d", fm.Version)
	}
}

// TestWriteSharedMemory_SingleWriterSlotOwnership 验证 file_tree 单写多读写入方校验：
// 当前实现 key = <ownAgentID>:<slot>，兄弟 Agent 键天然不同（结构上互不覆盖），
// 校验针对同键场景（如未来 parentID 命名空间写入 / 历史脏数据）：frontmatter owner
// 与当前写入者不一致时拒绝覆盖，owner 本人可更新（版本递增），非单写 slot 不校验。
func TestWriteSharedMemory_SingleWriterSlotOwnership(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	store := NewFileSharedMemoryStore(dir)
	r.SetSharedMemory(store)
	ctx := context.Background()

	write := func(agentID, key, content string) *Result {
		res, _ := r.Dispatch(WithAgentID(ctx, agentID), "WriteSharedMemory", map[string]any{
			"content": content,
			"key":     key,
		})
		return res
	}

	// 预置同键值但 owner 为他人（模拟共享命名空间写入/脏数据：key 归 domain-2 但 owner 是 domain-1）。
	// 直接落 store，绕过工具自身的 owner 注入，验证校验逻辑本身。
	if err := store.Set(ctx, "session-1/domain-2:file_tree", EncodeSharedMD("session-1/domain-1", "file_tree", nil, "owned by d1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 非 owner 覆盖 file_tree：拒绝（单写多读）。
	res := write("session-1/domain-2", "file_tree", "tree by d2")
	if res == nil || res.Success {
		t.Fatalf("non-owner overwrite should be rejected, got %+v", res)
	}
	if !strings.Contains(res.Error, "单写多读") {
		t.Fatalf("expected 单写多读 rejection hint, got: %s", res.Error)
	}

	// owner 本人更新：成功且版本递增。
	if res := write("session-1/domain-1", "file_tree", "tree by d1 v2"); res == nil || !res.Success {
		t.Fatalf("owner update should succeed, got %+v", res)
	}
	val, _ := store.Get(ctx, "session-1/domain-1:file_tree")
	if fm, _, _ := DecodeSharedMD(val); fm.Version != 1 {
		t.Fatalf("expected version 1 after owner update, got %d", fm.Version)
	}

	// 非单写 slot（shared）：不校验 owner，任意写入者可用。
	if res := write("session-1/domain-2", "shared", "d2 context"); res == nil || !res.Success {
		t.Fatalf("multi-writer slot should accept any writer, got %+v", res)
	}
}

// TestWriteSharedMemory_CASFallbackNonVersionedStore 验证不支持版本接口的存储
// 退化为普通覆盖写（行为不变，版本号不注入）。
func TestWriteSharedMemory_CASFallbackNonVersionedStore(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	res, err := r.Dispatch(WithAgentID(context.Background(), "meta-1"), "WriteSharedMemory", map[string]any{
		"content": "plain",
	})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("expected success with non-versioned store, got res=%+v err=%v", res, err)
	}
}
