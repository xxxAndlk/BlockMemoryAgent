package tool

// 导入测试所需标准库与项目包。
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/blockmemory/agent/backend/internal/config"
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

// TestIsVerificationCommand 验证验证类命令判定覆盖领域提示词规定的三种语法检查写法
// （node -c / go build / py_compile）与常见测试运行器；非验证命令不误判。
// 08-13 塔防实证：node -c 是塔防验收唯一证据来源却全不命中，致 verify_missing 误报。
func TestIsVerificationCommand(t *testing.T) {
	for _, cmd := range []string{
		"node -c js/monster.js",
		"cd D:\\x; node -c js/monster.js; node -c js/config.js",
		"node --check js/a.js",
		"go build ./pkg",
		"go vet ./...",
		"go test ./...",
		"go test",
		"python -m py_compile x.py",
		"pytest tests/",
		"npm test",
		"npm run test",
		"cargo test",
		"eslint src/",
		"npm run lint",
	} {
		if !IsVerificationCommand(cmd) {
			t.Fatalf("IsVerificationCommand(%q) 应为 true", cmd)
		}
	}
	for _, cmd := range []string{
		"echo hello",
		"node server.js",
		"python app.py",
		"cat README.md",
		"git status",
	} {
		if IsVerificationCommand(cmd) {
			t.Fatalf("IsVerificationCommand(%q) 应为 false", cmd)
		}
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

// TestReadFile_ConsecutiveSameCallLoopGuard 验证"参数完全相同的连续 ReadFile"循环检测：
// 重读不再拦截（每次直返磁盘最新内容），第 2 次连续相同调用附翻页提醒，
// 第 3 次判定死循环拦截并请求 LoopExit；ResetReadHistory 后计数清零。
// 对应日志事故：代码助手对 config.js 反复 ReadFile 10+ 次烧 token。
func TestReadFile_ConsecutiveSameCallLoopGuard(t *testing.T) {
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

	// 第 2 次相同参数调用：仍直返内容（不拦截），但结果末尾附翻页提醒。
	res2, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res2.Success {
		t.Fatalf("second identical read should still serve content: err=%v success=%v", err, res2.Success)
	}
	if !strings.Contains(res2.Output, "v1") {
		t.Fatalf("second read should return content, got: %s", res2.Output)
	}
	if !strings.Contains(res2.Output, "勿重复读取") {
		t.Fatalf("second identical read should carry reminder, got: %s", res2.Output)
	}

	// 参数有变化（翻页）即归零重计，不触发循环检测。
	paged := map[string]any{"path": "a.txt", "offset": float64(1), "limit": float64(1)}
	if res, err := r.Dispatch(ctx, "ReadFile", paged); err != nil || !res.Success {
		t.Fatalf("read with different params should succeed: err=%v success=%v", err, res.Success)
	}
	// 同参数第 2 次：仍成功（附提醒）。
	if res, err := r.Dispatch(ctx, "ReadFile", paged); err != nil || !res.Success {
		t.Fatalf("2nd consecutive identical read should succeed: err=%v success=%v", err, res.Success)
	}
	// 同参数第 3 次：判定死循环，拦截。
	res3, _ := r.Dispatch(ctx, "ReadFile", paged)
	if res3.Success {
		t.Fatal("3rd consecutive identical read should be blocked as loop")
	}
	if !strings.Contains(res3.Error, "死循环") {
		t.Fatalf("expected loop error, got: %s", res3.Error)
	}

	// 模拟用户新消息：ResetReadHistory 清空连读计数后可重读。
	r.ResetReadHistory("s1")
	res4, err := r.Dispatch(ctx, "ReadFile", paged)
	if err != nil || !res4.Success {
		t.Fatalf("read after reset should succeed: err=%v success=%v", err, res4.Success)
	}
}

// TestReadFile_AfterWriteFile_ReturnsFreshContent 脏数据回归测试：
// ReadFile 每次直返磁盘最新内容，WriteFile 改写后按相同参数重读必须看到新内容，
// 不依赖任何"已读记录清理"机制（WriteFile/sed/外部进程改写均被天然覆盖）。
func TestReadFile_AfterWriteFile_ReturnsFreshContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 首次读取成功，看到 v1。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res.Success {
		t.Fatalf("first read should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "v1") {
		t.Fatalf("expected v1 content, got: %s", res.Output)
	}

	// WriteFile 改写 a.txt 内容为 v2。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    "a.txt",
		"content": "v2",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile should succeed: err=%v success=%v", err, wres.Success)
	}

	// 相同参数重读：直返磁盘最新内容 v2，绝无脏数据。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if err != nil || !res3.Success {
		t.Fatalf("read after WriteFile should succeed: err=%v success=%v", err, res3.Success)
	}
	if !strings.Contains(res3.Output, "v2") {
		t.Fatalf("expected v2 content after rewrite, got: %s", res3.Output)
	}
}

// TestReadFile_Pagination 验证 offset/limit 行区间分页：
// 分页头（输出首行）给出总行数/本页区间/下一页起点，翻页返回后续区间，
// 末页标注"已到文件末尾"，offset 越界报错并告知总行数。
func TestReadFile_Pagination(t *testing.T) {
	dir := t.TempDir()
	// 构造 10 行文件。
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "p.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 第一页：offset=1, limit=4，返回 1-4 行，分页头指向 offset=5。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(1), "limit": float64(4)})
	if err != nil || !res.Success {
		t.Fatalf("first page should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "共 10 行") || !strings.Contains(res.Output, "本页 1-4 行") || !strings.Contains(res.Output, "offset=5") {
		t.Fatalf("first page header wrong: %s", res.Output)
	}
	if !strings.Contains(res.Output, "line-1") || !strings.Contains(res.Output, "line-4") || strings.Contains(res.Output, "line-5") {
		t.Fatalf("first page content wrong: %s", res.Output)
	}

	// 第二页：offset=5, limit=4，返回 5-8 行（不含上一页内容）。
	res2, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(5), "limit": float64(4)})
	if err != nil || !res2.Success {
		t.Fatalf("second page should succeed: err=%v success=%v", err, res2.Success)
	}
	if strings.Contains(res2.Output, "line-4") || !strings.Contains(res2.Output, "line-5") || !strings.Contains(res2.Output, "line-8") {
		t.Fatalf("second page content wrong: %s", res2.Output)
	}

	// 末页：limit 超出末尾时截到文件末尾并标注。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(9), "limit": float64(4)})
	if err != nil || !res3.Success {
		t.Fatalf("last page should succeed: err=%v success=%v", err, res3.Success)
	}
	if !strings.Contains(res3.Output, "本页 9-10 行") || !strings.Contains(res3.Output, "已到文件末尾") {
		t.Fatalf("last page header wrong: %s", res3.Output)
	}

	// offset 越界：报错并告知总行数。
	res4, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "p.txt", "offset": float64(11), "limit": float64(4)})
	if res4.Success {
		t.Fatal("out-of-range offset should fail")
	}
	if !strings.Contains(res4.Error, "总行数 10") {
		t.Fatalf("expected total-lines error, got: %s", res4.Error)
	}
}

// TestReadFile_LimitHardClamp 验证单次 ReadFile 行数硬上限：
// limit > maxReadFileLimit 时钳到上限返回，分页头标注截断原因与下一页起点。
// 对应日志事故：2026-08-14 塔防 9 叶子并行重绘，单次 ReadFile 401/410/450 行违反 300 行纪律。
func TestReadFile_LimitHardClamp(t *testing.T) {
	dir := t.TempDir()
	// 构造 400 行文件。
	var lines []string
	for i := 1; i <= 400; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// limit=400 超上限：应只返回 1-300 行，分页头标注截断并指引 offset=301。
	res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "big.txt", "offset": float64(1), "limit": float64(400)})
	if err != nil || !res.Success {
		t.Fatalf("clamped read should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "本页 1-300 行") {
		t.Fatalf("expected clamped page range 1-300, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "已截断") {
		t.Fatalf("expected clamp note in header, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "offset=301") {
		t.Fatalf("expected next-page hint offset=301, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "line-300") || strings.Contains(res.Output, "line-301") {
		t.Fatalf("expected content up to line-300 only, got: %s", res.Output)
	}
}

// TestWriteFileProtectedPath 验证 WriteFile 工具不能写入受保护的 .git/ 目录。
// 源码目录名（backend/ 等）已不再受保护：workDir 是用户项目，Agent 需直接编辑用户代码；
// 仅 VCS/IDE/构建产物根目录被挡。此处用 .git/ 代表受保护目录。
func TestWriteFileProtectedPath(t *testing.T) {
	// 创建临时目录作为工作目录。
	dir := t.TempDir()
	// 创建内置工具注册表。
	r := NewBuiltinRegistry(dir, nil, nil)
	// 构造携带 SessionID 的上下文。
	ctx := WithSessionID(context.Background(), "s1")
	// 尝试写入 .git/foo，该路径命中受保护目录规则（VCS 元数据）。
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    ".git/foo",
		"content": "package foo",
	})
	// 校验调度未返回底层错误。
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	// 校验写入被拦截，Success 为 false。
	if res.Success {
		t.Fatal("expected write to .git/ to fail")
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
// schema 中，且描述文本来自工具的 Description()；未注册时 schema 有 14 个内置工具
// （ReadFile/WriteFile/ListDir/RunCommand/SearchInFiles/HTTPGet/HTTPPost/GitDiff/GitStatus/GitLog/GitBlame/RefreshProjectDoc/WriteSharedMemory/WriteSpec）。
func TestSchemaIncludesCallSubAgent(t *testing.T) {
	// 未安装 call_sub_agent 时，schema 恰为 14 个内置工具（含 RefreshProjectDoc/WriteSpec）。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	if n := len(r.Schema()); n != 14 {
		t.Fatalf("expected 14 builtin tools without call_sub_agent, got %d", n)
	}
	// 安装后应出现在 schema 中，且描述来自 Description()。
	r.Register(&stubCallSubAgent{})
	schema := r.Schema()
	if len(schema) != 15 {
		t.Fatalf("expected 15 tools with call_sub_agent, got %d", len(schema))
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

// TestSchemaCallSubAgent_ModeField 验证 call_sub_agent 的 schema 含 mode 参数（TODO #29 三引擎）：
// 派发执行模式枚举暴露给 LLM，缺失则 LLM 无法感知该字段。
func TestSchemaCallSubAgent_ModeField(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&stubCallSubAgent{})
	for _, tl := range r.Schema() {
		if tl.Name() != "call_sub_agent" {
			continue
		}
		props := tl.InputSchema().Properties
		for _, want := range []string{"role_id", "task", "domain", "responsibility", "mode"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("call_sub_agent schema missing parameter %q (have %v)", want, keysOf(props))
			}
		}
		return
	}
	t.Fatal("call_sub_agent not found in schema")
}

// keysOf 提取 map 键切片，用于断言信息展示。
func keysOf(m map[string]*jsonschema.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
// 入参 files 被结构化存为 MD frontmatter（files mtime map），body 为 content 原文。
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

	// 带 files 调用：MD frontmatter 含 foo.go 的 mtime，body 为 content。
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
	fm, body, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if body != "foo.go defines package foo" {
		t.Fatalf("unexpected body: %q", body)
	}
	if len(fm.Files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(fm.Files))
	}
	if _, ok := fm.Files[target]; !ok {
		t.Fatalf("expected %s in Files map, got %v", target, fm.Files)
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
	fm2, body2, ok := DecodeSharedMD(val2)
	if !ok {
		t.Fatalf("KV value2 not MD: %q", val2)
	}
	if body2 != "no files summary" {
		t.Fatalf("unexpected body2: %q", body2)
	}
	if len(fm2.Files) != 0 {
		t.Fatalf("expected 0 tracked files, got %d", len(fm2.Files))
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

// TestWriteSpec_Structured 验证 WriteSpec 入参结构化存为 MD：frontmatter 含
// goal/acceptance/constraints/files mtime，body 为人读结构化 MD。
// 存储键为 "<agentID>:spec"。
func TestWriteSpec_Structured(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "game.js")
	if err := os.WriteFile(target, []byte("var x = 1"), 0644); err != nil {
		t.Fatalf("write game.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":        "把 canvas 宽度从 960 改为 1280",
		"acceptance":  []any{"node -c game.js 通过", "CONFIG.canvas.width=1280"},
		"constraints": []any{"不改动其他配置项"},
		"files":       []any{target},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}

	val, _ := store.Get(ctx, "meta-1:spec")
	if val == "" {
		t.Fatal("expected KV entry at meta-1:spec")
	}
	fm, _, ok := DecodeSharedMD(val)
	if !ok {
		t.Fatalf("KV value not MD: %q", val)
	}
	if fm.Goal != "把 canvas 宽度从 960 改为 1280" {
		t.Fatalf("unexpected goal: %q", fm.Goal)
	}
	if len(fm.Acceptance) != 2 {
		t.Fatalf("expected 2 acceptance, got %d", len(fm.Acceptance))
	}
	if len(fm.Constraints) != 1 {
		t.Fatalf("expected 1 constraint, got %d", len(fm.Constraints))
	}
	if len(fm.Files) != 1 {
		t.Fatalf("expected 1 tracked file, got %d", len(fm.Files))
	}
	if _, ok := fm.Files[target]; !ok {
		t.Fatalf("expected %s in Files map", target)
	}
}

// TestWriteSpec_GrowingFilesWarning files 含持续增长目录（logs/）文件时输出告警提示。
func TestWriteSpec_GrowingFilesWarning(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "tui")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logFile := filepath.Join(logDir, "2026-08-10.log")
	if err := os.WriteFile(logFile, []byte("line\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	srcFile := filepath.Join(dir, "main.js")
	if err := os.WriteFile(srcFile, []byte("x"), 0644); err != nil {
		t.Fatalf("write main.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "分析日志找失败原因",
		"acceptance": []any{"列出失败点"},
		"files":      []any{logFile, srcFile},
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, "豁免") || !strings.Contains(res.Output, logFile) {
		t.Fatalf("expected growing-file warning with path, got: %q", res.Output)
	}
}

// TestWriteSpec_RequiresGoalAndAcceptance 验证 goal/acceptance 必填校验。
func TestWriteSpec_RequiresGoalAndAcceptance(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetSharedMemory(newFakeSharedMemoryStore())
	ctx := WithAgentID(context.Background(), "meta-1")

	// 缺 goal：应失败。
	res, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"acceptance": []any{"a"},
	})
	if res.Success {
		t.Fatal("expected fail when goal missing")
	}

	// 缺 acceptance：应失败。
	res2, _ := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal": "g",
	})
	if res2.Success {
		t.Fatal("expected fail when acceptance missing")
	}
}

// TestWriteSpec_InvalidatesOnWriteFile 验证 Layer 2：WriteFile 成功后引用同 path 的 spec entry 被删除。
func TestWriteSpec_InvalidatesOnWriteFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "foo.js")
	if err := os.WriteFile(target, []byte("var x = 1"), 0644); err != nil {
		t.Fatalf("write foo.js: %v", err)
	}

	r := NewBuiltinRegistry(dir, nil, nil)
	store := newFakeSharedMemoryStore()
	r.SetSharedMemory(store)

	ctx := WithAgentID(WithSessionID(context.Background(), "s1"), "meta-1")

	// 写 spec，引用 foo.js。
	_, err := r.Dispatch(ctx, "WriteSpec", map[string]any{
		"goal":       "g",
		"acceptance": []any{"a"},
		"files":      []any{target},
	})
	if err != nil {
		t.Fatalf("WriteSpec: %v", err)
	}
	if _, ok := store.items["meta-1:spec"]; !ok {
		t.Fatal("expected spec entry before WriteFile")
	}

	// WriteFile 修改 foo.js，触发失效。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":    target,
		"content": "var x = 2",
	})
	if err != nil || !wres.Success {
		t.Fatalf("WriteFile: err=%v res=%+v", err, wres)
	}
	if _, ok := store.items["meta-1:spec"]; ok {
		t.Fatal("expected spec entry deleted after WriteFile invalidated it")
	}
}

// TestExploreBudget_WriteGateRaisesLimit 探索预算两档制回归测试：
// 写入未开始：8 次探索后封锁（反空转）；首次 WriteFile 成功后升档到 40，
// 修复期"读报错位置→改→复验"循环允许精读（v13 实证：验收领域 57 秒耗尽预算后
// 被禁读，只能整文件盲重写，4 次巨型调用耗 29 分钟）。
func TestExploreBudget_WriteGateRaisesLimit(t *testing.T) {
	dir := t.TempDir()
	// 准备足够多行，供不同 offset 的读取（避免触发连读循环守卫）。
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s-budget")

	// 写前烧掉 8 次探索预算（每次 offset 不同，属合法翻页）。
	for i := 1; i <= exploreBudget; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(i), "limit": float64(1)})
		if err != nil || !res.Success {
			t.Fatalf("read %d within budget should succeed: err=%v success=%v", i, err, res.Success)
		}
	}
	// 第 9 次：写前预算耗尽，封锁。
	res, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(30), "limit": float64(1)})
	if res.Success {
		t.Fatal("read beyond pre-write budget should be blocked")
	}
	if !strings.Contains(res.Error, "探索预算耗尽") {
		t.Fatalf("expected budget error, got: %s", res.Error)
	}

	// 首次 WriteFile 成功：预算升档，恢复精读能力。
	wres, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "b.txt", "content": "fix"})
	if err != nil || !wres.Success {
		t.Fatalf("write should succeed: err=%v success=%v", err, wres.Success)
	}
	res, err = r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(30), "limit": float64(1)})
	if err != nil || !res.Success {
		t.Fatalf("read after first write should succeed (post-write budget): err=%v success=%v", err, res.Success)
	}

	// 升档后仍有上限：烧到 40 次后再次封锁（防逐文件通读式发散）。
	for i := exploreBudget + 2; i <= exploreBudgetPostWrite; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(i), "limit": float64(1)})
		if err != nil || !res.Success {
			t.Fatalf("read %d within post-write budget should succeed: err=%v success=%v", i, err, res.Success)
		}
	}
	res, _ = r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(59), "limit": float64(1)})
	if res.Success {
		t.Fatal("read beyond post-write budget should be blocked")
	}
	if !strings.Contains(res.Error, "修复期探索预算耗尽") {
		t.Fatalf("expected post-write budget error, got: %s", res.Error)
	}
}

// TestExploreBudget_VerifyTaskRaisesLimit 验证型任务（整品验收/边验边修）探索预算出生即按
// postWrite 档（40 次），无需等首次 WriteFile：2026-08-12 整品验收事故实证——domain 16 次
// 在"读 8 文件跨文件核对契约"阶段即耗尽，逼出凭记忆整文件盲改 + 超长 LLM 调用。
// 验收任务读在写前，不该等首次 WriteFile 才升档。
func TestExploreBudget_VerifyTaskRaisesLimit(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	// domain 角色 + 验证型任务标记：正常 domain 档 8 次应封，验证型直接升到 postWrite 档 40 次。
	ctx := WithSessionID(context.Background(), "s-verify")
	ctx = WithRoleID(ctx, "domain")
	ctx = WithVerifyTask(ctx)

	// 烧完正常 domain 档 8 次：全部应成功（验证型不受 domain 8 次限制）。
	for i := 1; i <= exploreBudgetDomain; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(i), "limit": float64(1)})
		if err != nil || !res.Success {
			t.Fatalf("verify-task read %d within domain-tier should succeed: err=%v success=%v", i, err, res.Success)
		}
	}
	// 第 9..40 次：postWrite 档内仍应成功。
	for i := exploreBudgetDomain + 1; i <= exploreBudgetPostWrite; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(i), "limit": float64(1)})
		if err != nil || !res.Success {
			t.Fatalf("verify-task read %d within post-write tier should succeed: err=%v success=%v", i, err, res.Success)
		}
	}
	// 第 41 次：postWrite 档耗尽，封锁（文案=修复期，非 domain 协调者文案）。
	res, _ := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(75), "limit": float64(1)})
	if res.Success {
		t.Fatal("verify-task read beyond post-write budget should be blocked")
	}
	if !strings.Contains(res.Error, "修复期探索预算耗尽") {
		t.Fatalf("expected post-write budget error, got: %s", res.Error)
	}
}

// TestExploreBudget_ConfigOverride 验证 config.AgentConfig 覆盖探索预算三档默认值。
// 配置 >0 生效；nil cfg 回落包级常量。
func TestExploreBudget_ConfigOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.AgentConfig{
		ExploreBudget:           5,
		ExploreBudgetDomain:     3,
		ExploreBudgetPostWrite:  7,
	}
	r := NewBuiltinRegistry(dir, cfg, nil)
	if r.exploreBudget != 5 {
		t.Fatalf("exploreBudget = %d, want 5", r.exploreBudget)
	}
	if r.exploreBudgetDomain != 3 {
		t.Fatalf("exploreBudgetDomain = %d, want 3", r.exploreBudgetDomain)
	}
	if r.exploreBudgetPostWrite != 7 {
		t.Fatalf("exploreBudgetPostWrite = %d, want 7", r.exploreBudgetPostWrite)
	}

	// nil cfg 回落常量默认 20/8/40。
	r2 := NewBuiltinRegistry(dir, nil, nil)
	if r2.exploreBudget != 20 {
		t.Fatalf("nil cfg: exploreBudget = %d, want 20", r2.exploreBudget)
	}
	if r2.exploreBudgetDomain != 8 {
		t.Fatalf("nil cfg: exploreBudgetDomain = %d, want 8", r2.exploreBudgetDomain)
	}
	if r2.exploreBudgetPostWrite != 40 {
		t.Fatalf("nil cfg: exploreBudgetPostWrite = %d, want 40", r2.exploreBudgetPostWrite)
	}

	// cfg 字段为 0（未配置）回落常量。
	r3 := NewBuiltinRegistry(dir, &config.AgentConfig{}, nil)
	if r3.exploreBudget != 20 {
		t.Fatalf("zero cfg: exploreBudget = %d, want 20", r3.exploreBudget)
	}
	if r3.exploreBudgetDomain != 8 {
		t.Fatalf("zero cfg: exploreBudgetDomain = %d, want 8", r3.exploreBudgetDomain)
	}
}

// TestWriteFile_TemporaryOutputHasAbsPath 临时文件写入结果带落盘绝对路径（TODO #38-4 根因 D）：
// 临时文件落在会话级临时目录（.bma/tmp/<sid>/，路径不可预测），Output 必须携带绝对路径与
// $env:BMA_SESSION_TEMP_DIR 运行提示——Agent 首次运行不再盲猜工作目录路径
//（事故实证：ca-5 用 <工作目录>\verify-frost.js 找不到模块，白耗 1 条连杀额度）。
func TestWriteFile_TemporaryOutputHasAbsPath(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s-temp-path")

	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{
		"path":      "verify-frost.js",
		"content":   "const x = 1;",
		"temporary": true,
	})
	if err != nil || !res.Success {
		t.Fatalf("temporary write should succeed: err=%v success=%v", err, res.Success)
	}
	if !strings.Contains(res.Output, "bytes to ") || !strings.Contains(res.Output, "verify-frost.js") {
		t.Fatalf("output should carry abs path + size, got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "$env:BMA_SESSION_TEMP_DIR") {
		t.Fatalf("output should tell how to reference the temp file, got: %q", res.Output)
	}
	if res.Path == "" || res.TempDir == "" {
		t.Fatalf("result should carry Path/TempDir for cleanup, got path=%q tempdir=%q", res.Path, res.TempDir)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("temp file should exist at %s: %v", res.Path, err)
	}
}
