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
