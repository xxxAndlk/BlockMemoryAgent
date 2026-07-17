package tool

// 导入测试所需标准库与项目包。
import (
	"context"
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
// schema 中，且描述文本来自工具的 Description()；未注册时 schema 只有 11 个内置工具。
func TestSchemaIncludesCallSubAgent(t *testing.T) {
	// 未安装 call_sub_agent 时，schema 恰为 11 个内置工具。
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	if n := len(r.Schema()); n != 11 {
		t.Fatalf("expected 11 builtin tools without call_sub_agent, got %d", n)
	}
	// 安装后应出现在 schema 中，且描述来自 Description()。
	r.Register(&stubCallSubAgent{})
	schema := r.Schema()
	if len(schema) != 12 {
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
