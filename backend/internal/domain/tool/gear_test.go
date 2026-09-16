package tool

// gear_test.go 验证档位枚举校验与历史值归一化（TODO #14 三档全手动）。

import "testing"

// TestValidGear 验证枚举校验。
func TestValidGear(t *testing.T) {
	for _, g := range []string{GearFast, GearDaily, GearCluster} {
		if !ValidGear(g) {
			t.Errorf("ValidGear(%q) = false, want true", g)
		}
	}
	for _, g := range []string{"", "auto", "explore", "warp", "Daily"} {
		if ValidGear(g) {
			t.Errorf("ValidGear(%q) = true, want false", g)
		}
	}
}

// TestNormalizeGear 验证历史值 "auto"（规则自动选档，已退役）读侧映射 daily；其余原样。
func TestNormalizeGear(t *testing.T) {
	if got := NormalizeGear(LegacyGearAuto); got != GearDaily {
		t.Errorf("NormalizeGear(auto) = %q, want daily", got)
	}
	for _, g := range []string{GearFast, GearDaily, GearCluster, "warp", ""} {
		if got := NormalizeGear(g); got != g {
			t.Errorf("NormalizeGear(%q) = %q, want unchanged", g, got)
		}
	}
}
