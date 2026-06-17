package soul

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoaderInjectAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(path, []byte("你是助手 v1"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	l := NewLoader(path)
	if err := l.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
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
	if !strings.Contains(out2, "你是助手 v2") {
		t.Fatalf("reload not applied: %q", out2)
	}
}

func TestTemperature(t *testing.T) {
	if Temperature(KindRouting, 0.5) != 0 {
		t.Fatalf("routing should be 0")
	}
	if Temperature(KindCreative, 0.5) < 0.7 {
		t.Fatalf("creative should be high")
	}
	if Temperature(KindGeneric, 0.42) != 0.42 {
		t.Fatalf("generic should fall back to base")
	}
}

func TestInferKind(t *testing.T) {
	cases := []struct {
		in   string
		want TaskKind
	}{
		{"请帮我写代码修复 bug", KindCode},
		{"总结今天的进展", KindSummarize},
		{"分析为什么订单失败", KindAnalysis},
		{"帮我做一次创意营销文案", KindCreative},
		{"判断该路由到哪个 Agent", KindRouting},
		{"你好", KindGeneric},
	}
	for _, c := range cases {
		if got := InferKind(c.in); got != c.want {
			t.Errorf("InferKind(%q)=%s want %s", c.in, got, c.want)
		}
	}
}
