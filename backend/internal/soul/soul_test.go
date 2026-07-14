package soul

import (
	"os"            // 文件读写
	"path/filepath" // 临时路径拼接
	"strings"       // 子串检查
	"testing"       // Go 测试框架
)

// TestLoaderInjectAndReload 验证 Load/Reload 与 Inject 的完整流程。
func TestLoaderInjectAndReload(t *testing.T) {
	dir := t.TempDir()                    // 创建临时目录，测试结束后自动清理
	path := filepath.Join(dir, "soul.md") // 构造 soul.md 完整路径
	// 写入第一版人格文件。
	if err := os.WriteFile(path, []byte("你是助手 v1"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	l := NewLoader(path) // 创建加载器
	// 首次加载应成功。
	if err := l.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Inject 后应同时包含人格内容与原始 prompt。
	out := l.Inject("base prompt")
	if !strings.Contains(out, "你是助手 v1") || !strings.Contains(out, "base prompt") {
		t.Fatalf("inject content lost: %q", out)
	}

	// 修改文件后 Reload 生效
	if err := os.WriteFile(path, []byte("你是助手 v2"), 0644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if err := l.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	out2 := l.Inject("base")
	// Reload 后应读到第二版内容。
	if !strings.Contains(out2, "你是助手 v2") {
		t.Fatalf("reload not applied: %q", out2)
	}
}

// TestTemperature 验证包级 Temperature 函数对各类别的推荐值。
func TestTemperature(t *testing.T) {
	// 路由任务要求确定性最强，应为 0。
	if Temperature(KindRouting, 0.5) != 0 {
		t.Fatalf("routing should be 0")
	}
	// 创意任务应使用较高温度。
	if Temperature(KindCreative, 0.5) < 0.7 {
		t.Fatalf("creative should be high")
	}
	// 通用类别应回退到 base。
	if Temperature(KindGeneric, 0.42) != 0.42 {
		t.Fatalf("generic should fall back to base")
	}
}

// TestInferKind 验证 InferKind 对典型中文描述的分类。
func TestInferKind(t *testing.T) {
	cases := []struct {
		in   string   // 输入描述
		want TaskKind // 期望类别
	}{
		{"请帮我写代码修复 bug", KindCode},
		{"总结今天的进展", KindSummarize},
		{"分析为什么订单失败", KindAnalysis},
		{"帮我做一次创意营销文案", KindCreative},
		{"判断该路由到哪个 Agent", KindRouting},
		{"你好", KindGeneric},
	}
	// 逐条校验分类结果。
	for _, c := range cases {
		if got := InferKind(c.in); got != c.want {
			t.Errorf("InferKind(%q)=%s want %s", c.in, got, c.want)
		}
	}
}
