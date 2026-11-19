package checkpoint

// checkpoint_concurrency_test.go 对应 2026-11-19 验收报告 P1-5「并发」项：
//   - 多个 Action 并发写同一 blob 存储：临时文件名不得互撞（旧实现用固定 .tmp），
//     最终 blob 内容必须正确；
//   - 并发 Capture 同一工作区：快照 ID 稳定（同内容 → 同 ID），不得互相破坏索引。
//
// 并发结果一律经 channel 回收，不在 goroutine 里写共享切片（避免数据竞争的
// 静态怀疑，也让失败定位更直接）。

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// concurrencyResult 一条并发任务的结果（下标 + 值 + 错误）。
type concurrencyResult struct {
	index int
	value string
	err   error
}

// runConcurrently 并发执行 n 个任务并按 index 汇总结果（结果经 channel 回收）。
func runConcurrently(n int, task func(i int) (string, error)) ([]string, []error) {
	results := make(chan concurrencyResult, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			v, err := task(i)
			results <- concurrencyResult{index: i, value: v, err: err}
		}(i)
	}
	values := make([]string, n)
	errs := make([]error, n)
	for k := 0; k < n; k++ {
		r := <-results
		values[r.index] = r.value
		errs[r.index] = r.err
	}
	return values, errs
}

// TestConcurrentCaptureSameWorkspace 并发快照同一工作区。
func TestConcurrentCaptureSameWorkspace(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		writeFile(t, root, fmt.Sprintf("src/f%02d.css", i), fmt.Sprintf(".f%d{width:%dpx}\n", i, i))
	}
	s := newTestStore(t)

	const workers = 8
	ids, errs := runConcurrently(workers, func(int) (string, error) {
		snap, err := s.Capture(root)
		if err != nil {
			return "", err
		}
		return snap.ID, nil
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: capture failed: %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("identical workspace must yield the same snapshot id: %s vs %s", ids[0], ids[i])
		}
	}
	// 索引可回读且文件齐全（并发写索引不能互相截断）。
	loaded, err := s.Load(ids[0])
	if err != nil {
		t.Fatalf("load after concurrent capture: %v", err)
	}
	if len(loaded.Files) != 20 {
		t.Fatalf("snapshot must contain all files, got %d", len(loaded.Files))
	}
}

// TestConcurrentBlobWriteSameContent 并发写不同 hash 的大 blob 不得互相破坏
// （旧实现用固定 `<hash>.tmp`，同目录并发会互相覆盖临时文件）。
func TestConcurrentBlobWriteSameContent(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64KB，放大竞争窗口

	const workers = 8
	_, errs := runConcurrently(workers, func(i int) (string, error) {
		hash := fmt.Sprintf("concurrency-%d", i)
		if err := s.writeBlob(hash, content); err != nil {
			return "", err
		}
		return hash, nil
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	for i := 0; i < workers; i++ {
		hash := fmt.Sprintf("concurrency-%d", i)
		got, err := os.ReadFile(s.blobPath(hash))
		if err != nil {
			t.Fatalf("blob %s: %v", hash, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("blob %s corrupted by concurrent write (%d bytes)", hash, len(got))
		}
	}
}

// TestConcurrentWritesSameBlobHash 多个 goroutine 写同一 hash 的 blob 也要正确，
// 且不得残留临时文件。
func TestConcurrentWritesSameBlobHash(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("x"), 32*1024)
	const workers = 6
	_, errs := runConcurrently(workers, func(int) (string, error) {
		return "", s.writeBlob("same-hash", content)
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(s.blobPath("same-hash"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("same-hash blob corrupted: %d bytes", len(got))
	}
	entries, err := os.ReadDir(filepath.Dir(s.blobPath("same-hash")))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if bytes.Contains([]byte(e.Name()), []byte(".tmp-")) {
			t.Fatalf("temp blob file leaked: %s", e.Name())
		}
	}
}
