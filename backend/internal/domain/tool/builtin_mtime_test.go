package tool

// builtin_mtime_test.go 验证 expected_mtime 乐观锁（TODO #16 T16 并发写护栏）：
//   - 匹配放行：mtime 传回（含秒级容差内漂移）→ 写入成功；
//   - 冲突拒绝：文件在读取后被改 → 拒收（工具结果级错误，不碰文件），错误列当前 mtime；
//   - 缺省现状：不传 expected_mtime → 不校验（advisory 行为不变）；
//   - ReadFile 分页头暴露 mtime=...（expected_mtime 的取值来源，格式回环可解析）。
//
// 并发修改用 os.Chtimes 回拨模拟：2s 容差内真睡等 mtime 漂移会拖慢测试，回拨一次到位。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckExpectedMtime_MatchPasses 匹配放行：EditFile/WriteFile 传回当前 mtime 成功写入。
func TestCheckExpectedMtime_MatchPasses(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)

	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	mt := mtimeOf(t, filepath.Join(dir, "a.js")).Format(time.RFC3339)

	// EditFile：expected_mtime 与当前一致 → 放行。
	res := e.editFile(context.Background(), map[string]any{
		"path": "a.js", "old_string": "hello", "new_string": "world", "expected_mtime": mt,
	})
	if !res.Success {
		t.Fatalf("edit with matching mtime should pass: %v", res.Error)
	}
	if got := readAll(t, filepath.Join(dir, "a.js")); got != "world" {
		t.Fatalf("edit not applied: %q", got)
	}

	// WriteFile：同样放行。
	mt2 := mtimeOf(t, filepath.Join(dir, "a.js")).Format(time.RFC3339)
	res = e.writeFile(context.Background(), map[string]any{
		"path": "a.js", "content": "next", "expected_mtime": mt2,
	})
	if !res.Success {
		t.Fatalf("write with matching mtime should pass: %v", res.Error)
	}

	// 容差内漂移（RFC3339 秒级截断，实际偏 1s）也算匹配。
	cur := mtimeOf(t, filepath.Join(dir, "a.js"))
	res = e.writeFile(context.Background(), map[string]any{
		"path": "a.js", "content": "next2", "expected_mtime": cur.Add(-1 * time.Second).Format(time.RFC3339),
	})
	if !res.Success {
		t.Fatalf("within-tolerance mtime should pass: %v", res.Error)
	}
}

// TestCheckExpectedMtime_ConflictRejected 冲突拒绝：读取后文件被改 → 拒收且不碰文件。
func TestCheckExpectedMtime_ConflictRejected(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)

	target := filepath.Join(dir, "a.js")
	if err := os.WriteFile(target, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	// 把 v1 的 mtime 回拨 1 分钟，再模拟并发覆盖为 v2（mtime=now）——
	// stale 与当前差 ~1min > 2s 容差，构成现实冲突。
	past := time.Now().Add(-time.Minute)
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatal(err)
	}
	stale := mtimeOf(t, target).Format(time.RFC3339)
	if err := os.WriteFile(target, []byte("v2 (modified by sibling)"), 0644); err != nil {
		t.Fatal(err)
	}

	// EditFile 拒收：old_string 基于过期内容。
	res := e.editFile(context.Background(), map[string]any{
		"path": "a.js", "old_string": "v1", "new_string": "v1-edit", "expected_mtime": stale,
	})
	if res.Success {
		t.Fatal("edit with stale mtime must be rejected")
	}
	if !strings.Contains(res.Error, "mtime 冲突") || !strings.Contains(res.Error, "ReadFile") {
		t.Fatalf("conflict error should list current mtime and guide re-read: %q", res.Error)
	}
	// 文件未被本次写入触碰。
	if got := readAll(t, target); got != "v2 (modified by sibling)" {
		t.Fatalf("rejected edit must not touch file: %q", got)
	}

	// WriteFile 整写同样拒收。
	res = e.writeFile(context.Background(), map[string]any{
		"path": "a.js", "content": "clobber", "expected_mtime": stale,
	})
	if res.Success {
		t.Fatal("write with stale mtime must be rejected")
	}
	if got := readAll(t, target); got != "v2 (modified by sibling)" {
		t.Fatalf("rejected write must not touch file: %q", got)
	}

	// 已被并发删除的目标：同样拒收，不误建文件。
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	res = e.writeFile(context.Background(), map[string]any{
		"path": "a.js", "content": "resurrect", "expected_mtime": stale,
	})
	if res.Success {
		t.Fatal("write with expected_mtime on deleted file must be rejected")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("rejected write must not recreate file: %v", err)
	}
}

// TestCheckExpectedMtime_AbsentKeepsStatusQuo 缺省现状：不传 expected_mtime 不校验——
// 即使 mtime 已变（旧 advisory 警告语义），写入照常成功。
func TestCheckExpectedMtime_AbsentKeepsStatusQuo(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)

	target := filepath.Join(dir, "a.js")
	if err := os.WriteFile(target, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("v2"), 0644); err != nil {
		t.Fatal(err)
	}

	res := e.writeFile(context.Background(), map[string]any{"path": "a.js", "content": "v3"})
	if !res.Success {
		t.Fatalf("absent expected_mtime must keep current behavior: %v", res.Error)
	}
	// 无法解析的取值按"要求校验"拒收（显式传了就不得静默放行）。
	res = e.writeFile(context.Background(), map[string]any{"path": "a.js", "content": "v4", "expected_mtime": "not-a-time"})
	if res.Success || !strings.Contains(res.Error, "无法解析") {
		t.Fatalf("unparseable expected_mtime must be rejected, got success=%v err=%q", res.Success, res.Error)
	}
}

// TestReadFile_HeaderExposesMtime ReadFile 分页头带 mtime=...，且格式可被
// parseFlexibleTime 回环解析（expected_mtime 依赖 copy-back 通道闭合）。
func TestReadFile_HeaderExposesMtime(t *testing.T) {
	dir := t.TempDir()
	e := NewExecutor(dir)

	target := filepath.Join(dir, "a.js")
	if err := os.WriteFile(target, []byte("line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res := e.readFile(context.Background(), map[string]any{"path": "a.js"})
	if !res.Success {
		t.Fatalf("read failed: %v", res.Error)
	}
	idx := strings.Index(res.Output, "mtime=")
	if idx < 0 {
		t.Fatalf("ReadFile header missing mtime segment: %q", res.Output)
	}
	seg := res.Output[idx+len("mtime="):]
	if end := strings.IndexAny(seg, " |]"); end >= 0 {
		seg = seg[:end]
	}
	got, ok := parseFlexibleTime(seg)
	if !ok {
		t.Fatalf("header mtime %q not round-trippable", seg)
	}
	if d := mtimeOf(t, target).Sub(got); d > expectedMtimeTolerance || d < -expectedMtimeTolerance {
		t.Fatalf("header mtime %v off from file mtime: %v", got, d)
	}
}

// TestParseFlexibleTime 解析器覆盖：RFC3339/Go 默认布局/无时区布局/失败。
func TestParseFlexibleTime(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{now.Format(time.RFC3339), now, true},
		{now.Format(time.RFC3339Nano), now, true},
		{now.Format("2006-01-02 15:04:05"), now, true},
		{now.Format("2006-01-02 15:04:05.999999999 -0700 MST"), now, true},
		{"garbage", time.Time{}, false},
	}
	for i, c := range cases {
		got, ok := parseFlexibleTime(c.in)
		if ok != c.ok {
			t.Fatalf("case %d (%q): ok=%v want %v", i, c.in, ok, c.ok)
		}
		if ok && got.Sub(c.want).Abs() > time.Second {
			t.Fatalf("case %d (%q): got %v want %v", i, c.in, got, c.want)
		}
	}
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime()
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
