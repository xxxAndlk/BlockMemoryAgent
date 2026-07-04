package watchdog

import (
	"strings"
	"testing"
)

func TestEstimator(t *testing.T) {
	if Estimator("") != 0 {
		t.Fatalf("empty should be 0")
	}
	if Estimator("hello") < 1 {
		t.Fatalf("non-empty should produce >0 tokens")
	}
}

func TestCheck_Levels(t *testing.T) {
	w := New(Config{SoftLimit: 100, HardLimit: 200})

	if d := w.Check("a", "small"); d.Level != LevelOK {
		t.Fatalf("expected OK, got %s", d.Level)
	}

	// 软阈值
	mid := strings.Repeat("a", 500) // 500B / 4 ≈ 125 tokens
	if d := w.Check("a", mid); d.Level != LevelCompress {
		t.Fatalf("expected COMPRESS, got %s (tokens=%d)", d.Level, d.Tokens)
	}

	// 硬阈值
	big := strings.Repeat("b", 1000)
	if d := w.Check("a", big); d.Level != LevelEvict {
		t.Fatalf("expected EVICT, got %s (tokens=%d)", d.Level, d.Tokens)
	}

	if w.LastNonOK() == nil {
		t.Fatalf("LastNonOK should not be nil")
	}
}

func TestConfigForWindow(t *testing.T) {
	cfg := ConfigForWindow(64000)
	if cfg.SoftLimit != 32000 {
		t.Fatalf("soft limit for 64k should be 32000, got %d", cfg.SoftLimit)
	}
	if cfg.HardLimit != 51200 {
		t.Fatalf("hard limit for 64k should be 51200, got %d", cfg.HardLimit)
	}
	if cfg.HardLimit <= cfg.SoftLimit {
		t.Fatal("hard limit must be greater than soft limit")
	}
}

func TestWatchdogSetConfig(t *testing.T) {
	w := New(DefaultConfig())
	w.SetConfig(ConfigForWindow(128000))

	d := w.Check("a", strings.Repeat("x", 70000)) // 70000B / 4 ≈ 17500 tokens
	if d.Level != LevelOK {
		t.Fatalf("after resize to 128k, 17.5k tokens should be OK, got %s", d.Level)
	}

	d = w.Check("a", strings.Repeat("x", 260000)) // ~65k tokens
	if d.Level != LevelCompress {
		t.Fatalf("after resize to 128k, 65k tokens should be COMPRESS, got %s", d.Level)
	}
}
