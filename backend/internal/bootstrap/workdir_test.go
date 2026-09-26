package bootstrap

import (
	"os"            // 断言目录已创建
	"path/filepath" // 路径拼接
	"testing"       // 测试框架
)

// fakeHome 建一个形如安装目录的临时根（含 config/config.yaml，满足 looksLikeHome），
// 并把 BMA_HOME 指向它，返回该根路径。
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "config.yaml"), []byte("agent: {}\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	t.Setenv("BMA_HOME", home)
	return home
}

// 未配置（空/空白）→ 返回空串交给调用方回落进程 cwd，且不去碰安装目录解析。
func TestResolveDefaultWorkDir_EmptyFallsBack(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		got, err := resolveDefaultWorkDir(in)
		if err != nil {
			t.Fatalf("value %q: unexpected error: %v", in, err)
		}
		if got != "" {
			t.Fatalf("value %q: got %q, want empty (fallback to cwd)", in, got)
		}
	}
}

// 相对路径按安装目录（BMA_HOME）解析，且目录被创建出来——新会话落盘根不能等首次写文件才建。
func TestResolveDefaultWorkDir_RelativeUnderHome(t *testing.T) {
	home := fakeHome(t)
	got, err := resolveDefaultWorkDir("workspace")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := filepath.Join(home, "workspace")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("default workdir should exist as dir: %v", err)
	}
}

// 绝对路径原样使用（清洗后），同样保证已创建。
func TestResolveDefaultWorkDir_AbsolutePassthrough(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absolute-wd")
	got, err := resolveDefaultWorkDir(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != filepath.Clean(dir) {
		t.Fatalf("got %q, want %q", got, filepath.Clean(dir))
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("absolute default workdir should exist as dir: %v", err)
	}
}
