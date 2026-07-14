package watchdog

import (
	"strings" // 构造长文本
	"testing" // Go 测试框架
)

// TestEstimator 验证包级 Estimator 的基础行为。
func TestEstimator(t *testing.T) {
	// 空字符串应返回 0。
	if Estimator("") != 0 {
		t.Fatalf("empty should be 0")
	}
	// 非空字符串至少应产生大于 0 的估算值。
	if Estimator("hello") < 1 {
		t.Fatalf("non-empty should produce >0 tokens")
	}
}

// TestCheck_Levels 验证 Watchdog 的四级决策阈值划分。
func TestCheck_Levels(t *testing.T) {
	w := New(Config{SoftLimit: 100, HardLimit: 200})

	// 短文本：应判定为 OK。
	if d := w.Check("a", "small"); d.Level != LevelOK {
		t.Fatalf("expected OK, got %s", d.Level)
	}

	// 软阈值：500B / 4 ≈ 125 tokens，应触发 Compress。
	mid := strings.Repeat("a", 500)
	if d := w.Check("a", mid); d.Level != LevelCompress {
		t.Fatalf("expected COMPRESS, got %s (tokens=%d)", d.Level, d.Tokens)
	}

	// 硬阈值：1000B / 4 ≈ 250 tokens，应触发 Evict。
	big := strings.Repeat("b", 1000)
	if d := w.Check("a", big); d.Level != LevelEvict {
		t.Fatalf("expected EVICT, got %s (tokens=%d)", d.Level, d.Tokens)
	}

	// 检查最近非 OK 决策应存在（上面已有 Compress/Evict）。
	if w.LastNonOK() == nil {
		t.Fatalf("LastNonOK should not be nil")
	}
}

// TestConfigForWindow 验证基于上下文窗口的阈值推导。
func TestConfigForWindow(t *testing.T) {
	cfg := ConfigForWindow(64000)
	// 64k 窗口的 50% 应为 32k。
	if cfg.SoftLimit != 32000 {
		t.Fatalf("soft limit for 64k should be 32000, got %d", cfg.SoftLimit)
	}
	// 64k 窗口的 80% 应为 51.2k。
	if cfg.HardLimit != 51200 {
		t.Fatalf("hard limit for 64k should be 51200, got %d", cfg.HardLimit)
	}
	// 硬阈值必须严格大于软阈值。
	if cfg.HardLimit <= cfg.SoftLimit {
		t.Fatal("hard limit must be greater than soft limit")
	}
}

// TestWatchdogSetConfig 验证运行时调整阈值后决策结果随之变化。
func TestWatchdogSetConfig(t *testing.T) {
	w := New(DefaultConfig())
	w.SetConfig(ConfigForWindow(128000))

	// 70k 字节 ≈ 17.5k tokens，在 128k 窗口下应为 OK。
	d := w.Check("a", strings.Repeat("x", 70000))
	if d.Level != LevelOK {
		t.Fatalf("after resize to 128k, 17.5k tokens should be OK, got %s", d.Level)
	}

	// 260k 字节 ≈ 65k tokens，在 128k 窗口下应触发 Compress（软阈值 64k）。
	d = w.Check("a", strings.Repeat("x", 260000))
	if d.Level != LevelCompress {
		t.Fatalf("after resize to 128k, 65k tokens should be COMPRESS, got %s", d.Level)
	}
}
