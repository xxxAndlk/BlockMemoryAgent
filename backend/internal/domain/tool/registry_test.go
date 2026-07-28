package tool

// 导入测试所需标准库与项目包。
import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRegistrySchema 验证内置工具注册表的 Schema 至少包含 11 个工具定义。
func TestRegistrySchema(t *testing.T) {
	// 创建内置工具注册表，使用临时目录作为工作目录。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	// 获取所有工具定义。
	schema := r.Schema()
	// 校验工具数量是否满足预期下限。
	if len(schema) < 11 {
		t.Fatalf("expected at least 11 tools, got %d", len(schema))
	}
}

// TestReadFile 验证 ReadFile 工具可以正确读取工作目录下的文件内容。
func TestReadFile(t *testing.T) {
	// 创建临时目录并在其中写入测试文件 hello.txt。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("world"), 0644); err != nil {
		// 文件写入失败则终止测试。
		t.Fatalf("write file: %v", err)
	}
	// 使用临时目录创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 构造携带 SessionID 的上下文。
	ctx := WithSessionID(context.Background(), "s1")
	// 调度 ReadFile 工具读取 hello.txt。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "hello.txt"})
	// 校验调度未返回错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验工具调用成功。
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	// 校验输出内容包含 "world"。
	if !strings.Contains(res.Output, "world") {
		t.Fatalf("expected output to contain 'world', got: %s", res.Output)
	}
}

// TestReadFile_DedupAndReset 验证已读守卫拦截重读，且 ResetReadHistory 后可重读。
// 对应日志事故：test_assistant-23 反复读 kv.go 被拒 14 分钟；现有修复应让其在新任务重读。
func TestReadFile_DedupAndReset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 首次读取应成功。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}

	// 同 session 内第二次读取应被守卫拦截。
	res2, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if res2.Success {
		t.Fatal("second read should be blocked by dedup guard")
	}
	if !strings.Contains(res2.Error, "已读过") {
		t.Fatalf("expected dedup error, got: %s", res2.Error)
	}

	// 模拟用户新消息：ResetReadHistory 清空记录。
	r.ResetReadHistory("s1")

	// 清空后应可再次读取。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res3.Success {
		t.Fatalf("read after reset should succeed: err=%v success=%v", err, res3.Success)
	}
}

// TestWriteFile_ClearsReadHistory 验证 WriteFile 成功后清掉同 path 的已读记录，
// 允许后续 ReadFile 重读改后内容（防 Agent history 脏数据：文件被改但 LLM 只看旧 tool_result）。
func TestWriteFile_ClearsReadHistory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 首次读取成功，记录进已读列表。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "v1") {
		t.Fatalf("expected v1 content, got: %s", res.Output)
	}

	// 第二次读取应被拦截。
	res2, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if res2.Success {
		t.Fatal("second read should be blocked by dedup guard")
	}

	// WriteFile 改写 a.txt 内容为 v2，触发清同 path 已读记录。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "a.txt",
		"content": "v2",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile should succeed: err=%v success=%v", err, wres.Success)
	}

	// 改写后 ReadFile 应能再次读取，返回最新内容 v2。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res3.Success {
		t.Fatalf("read after WriteFile should succeed: err=%v success=%v", err, res3.Success)
	}
	if !strings.Contains(res3.Output, "v2") {
		t.Fatalf("expected v2 content after rewrite, got: %s", res3.Output)
	}
}

// TestWriteFile_ClearsReadHistory_PreservesOthers 验证 WriteFile 只清同 path 的已读记录，
// 不误清其他文件的已读记录（保持其他文件的防重读语义）。
func TestWriteFile_ClearsReadHistory_PreservesOthers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 读 a.txt 与 b.txt，两者都进已读列表。
	if _, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"}); err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if _, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "b.txt"}); err != nil {
		t.Fatalf("read b.txt: %v", err)
	}

	// WriteFile 改 a.txt：只清 a.txt 的已读记录，b.txt 仍被拦截。
	if _, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "a.txt",
		"content": "a2",
	}); err != nil {
		t.Fatalf("WriteFile a.txt: %v", err)
	}

	// a.txt 应能重读（已清）。
	resA, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !resA.Success {
		t.Fatalf("read a.txt after WriteFile should succeed: err=%v success=%v", err, resA.Success)
	}

	// b.txt 仍应被拦截（未清）。
	resB, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "b.txt"})
	if resB.Success {
		t.Fatal("b.txt should still be blocked by dedup guard (WriteFile touched a.txt only)")
	}
}

// TestWriteFileProtectedPath 验证 WriteFile 工具不能写入受保护的 backend/ 目录。
func TestWriteFileProtectedPath(t *testing.T) {
	// 创建临时目录作为工作目录。
	dir := t.TempDir()
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 构造携带 SessionID 的上下文。
	ctx := WithSessionID(context.Background(), "s1")
	// 尝试写入 backend/foo.go，该路径命中受保护目录规则。
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "backend/foo.go",
		"content": "package foo",
	})
	// 校验调度未返回底层错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验写入被拦截，Success 为 false。
	if res.Success {
		t.Fatal("expected write to backend/ to fail")
	}
	// 校验错误信息包含中文或英文的受保护路径提示。
	if !strings.Contains(res.Error, "受保护") && !strings.Contains(res.Error, "protected") {
		t.Fatalf("expected protected path error, got: %s", res.Error)
	}
}

// TestRunCommandEcho 验证 RunCommand 工具可以正确执行 echo 命令。
func TestRunCommandEcho(t *testing.T) {
	// 创建临时目录作为工作目录。
	dir := t.TempDir()
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 根据操作系统选择命令字符串，echo 在 Windows 与 Unix 下语法一致。
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "echo hello"
	} else {
		cmd = "echo hello"
	}
	// 调度 RunCommand 工具执行 echo 命令。
	res, err := r.Dispatch(context.Background(), "RunCommand", map[string]any{"command": cmd})
	// 校验调度未返回错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验命令执行成功。
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	// 校验输出包含 "hello"。
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("expected output to contain 'hello', got: %s", res.Output)
	}
}

// TestUnknownTool 验证调度未知工具时会返回包含 "unknown tool" 的错误。
func TestUnknownTool(t *testing.T) {
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	// 尝试调度一个不存在的工具。
	_, err := r.Dispatch(context.Background(), "NotATool", map[string]any{})
	// 校验返回了错误。
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	// 校验错误信息包含 "unknown tool"。
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got: %v", err)
	}
}

// stubCallSubAgent 是 call_sub_agent 的测试桩，模拟 subagent.Dispatcher
// 安装的工具（带 Description 可选接口）。
type stubCallSubAgent struct{}

func (s *stubCallSubAgent) Name() string      { return "call_sub_agent" }
func (s *stubCallSubAgent) Aliases() []string { return nil }
func (s *stubCallSubAgent) Description() string {
	return "stub: 派发自包含子任务给子 Agent。"
}
func (s *stubCallSubAgent) Execute(ctx context.Context, args map[string]any) *Result {
	return &Result{Tool: "call_sub_agent", Success: true, Output: "sub-1"}
}

// TestSchemaIncludesCallSubAgent 验证：call_sub_agent 注册后出现在 LLM 工具
// schema 中，且描述文本来自工具的 Description()；未注册时 schema 只有 12 个内置工具
// （ReadFile/WriteFile/ListDir/RunCommand/SearchInFiles/HTTPGet/HTTPPost/GitDiff/GitStatus/GitLog/GitBlame/WriteSharedMemory）。
func TestSchemaIncludesCallSubAgent(t *testing.T) {
	// 未安装 call_sub_agent 时，schema 恰为 12 个内置工具。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	if n := len(r.Schema()); n != 12 {
		t.Fatalf("expected 12 builtin tools without call_sub_agent, got %d", n)
	}
	// 安装后应出现在 schema 中，且描述来自 Description()。
	r.Register(&stubCallSubAgent{})
	schema := r.Schema()
	if len(schema) != 13 {
		t.Fatalf("expected 12 tools with call_sub_agent, got %d", len(schema))
	}
	// 遍历查找 call_sub_agent 并校验描述文本。
	found := false
	for _, tl := range schema {
		if tl.Name() == "call_sub_agent" {
			found = true
			if !strings.Contains(tl.Description(), "stub:") {
				t.Fatalf("expected description from Description(), got %q", tl.Description())
			}
		}
	}
	if !found {
		t.Fatal("call_sub_agent not found in schema after Register")
	}
}

// fakeSharedMemoryStore 是测试用 SharedMemoryStore 实现，内存版，无持久化。
type fakeSharedMemoryStore struct {
	items map[string]string
}

func newFakeSharedMemoryStore() *fakeSharedMemoryStore {
	return &fakeSharedMemoryStore{items: make(map[string]string)}
}

func (s *fakeSharedMemoryStore) Set(ctx context.Context, key, value string) error {
	s.items[key] = value
	return nil
}

func (s *fakeSharedMemoryStore) Get(ctx context.Context, key string) (string, error) {
	return s.items[key], nil
}

func (s *fakeSharedMemoryStore) Delete(ctx context.Context, key string) error {
	delete(s.items, key)
	return nil
}

func (s *fakeSharedMemoryStore) Keys(ctx context.Context) []string {
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	return keys
}

// TestWriteSharedMemory_StructuredAndFileTracking 验证 Layer 1：WriteSharedMemory
// 入参 files 被结构化存为 SharedEntry JSON，含 mtime 戳。files 缺省时退化为旧格式兼容。
func TestWriteSharedMemory_StructuredAndFileTracking(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(target, []byte("package foo"), 0644); err != nil {
		t.Fatalf("write foo.go: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 带 files 调用：KV value 应为 JSON，含 foo.go 的 mtime。
	res, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "foo.go defines package foo",
		"files":   []any{target},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}

	val, _ := store.Get(ctx, "meta-1:shared")
	if val == "" {
		t.Fatal("expected KV entry after WriteSharedMemory")
	}
	var entry SharedEntry
	if err := json.Unmarshal([]byte(val), &entry); err != nil {
		t.Fatalf("KV value not JSON: %v (val=%q)", err, val)
	}
	if entry.Content != "foo.go defines package foo" {
		t.Fatalf("unexpected content: %q", entry.Content)
	}
	if len(entry.Files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(entry.Files))
	}
	if _, ok := entry.Files[target]; !ok {
		t.Fatalf("expected %s in Files map, got %v", target, entry.Files)
	}

	// files 缺省调用：仍写 KV，Files 为空 map。
	res2, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "no files summary",
	})
	if err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	if !res2.Success {
		t.Fatalf("expected success2, got: %s", res2.Error)
	}
	val2, _ := store.Get(ctx, "meta-1:shared")
	var entry2 SharedEntry
	if err := json.Unmarshal([]byte(val2), &entry2); err != nil {
		t.Fatalf("KV value2 not JSON: %v", err)
	}
	if entry2.Content != "no files summary" {
		t.Fatalf("unexpected content2: %q", entry2.Content)
	}
	if len(entry2.Files) != 0 {
		t.Fatalf("expected 0 tracked files, got %d", len(entry2.Files))
	}
}

// TestWriteFile_InvalidatesSharedMemory 验证 Layer 2：WriteFile 成功后，
// 引用同 path 的 KV entry 被删除，防止子 Agent 读到旧摘要。
func TestWriteFile_InvalidatesSharedMemory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(target, []byte("package bar"), 0644); err != nil {
		t.Fatalf("write bar.go: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆，引用 bar.go。
	_, err := r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "bar.go defines package bar",
		"files":   []any{target},
	})
	if err != nil {
		t.Fatalf("WriteSharedMemory: %v", err)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("expected KV entry before WriteFile")
	}

	// WriteFile 修改 bar.go，触发失效 hook。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    target,
		"content": "package bar // modified",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !wres.Success {
		t.Fatalf("WriteFile failed: %s", wres.Error)
	}

	// KV entry 应被删除。
	if _, ok := store.items["meta-1:shared"]; ok {
		t.Fatal("expected KV entry deleted after WriteFile invalidated it")
	}
}

// TestWriteFile_DoesNotInvalidateUnrelatedEntry 验证 Layer 2 精确性：
// WriteFile 只删引用同 path 的 entry，不误删引用其他 path 的 entry。
func TestWriteFile_DoesNotInvalidateUnrelatedEntry(t *testing.T) {
	dir := t.TempDir()
	foo := filepath.Join(dir, "foo.go")
	bar := filepath.Join(dir, "bar.go")
	if err := os.WriteFile(foo, []byte("foo"), 0644); err != nil {
		t.Fatalf("write foo: %v", err)
	}
	if err := os.WriteFile(bar, []byte("bar"), 0644); err != nil {
		t.Fatalf("write bar: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写共享记忆只引用 foo.go。
	_, _ = r.Dispatch(ctx, "WriteSharedMemory", map[string]any{
		"content": "foo summary",
		"files":   []any{foo},
	})

	// WriteFile 修改 bar.go：foo 引用的 entry 不应被删。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    bar,
		"content": "bar modified",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile bar: err=%v res=%+v", err, wres)
	}
	if _, ok := store.items["meta-1:shared"]; !ok {
		t.Fatal("KV entry should NOT be deleted: WriteFile touched bar.go, entry references foo.go only")
	}
}
