package tool

// gear_test.go 验证档位枚举校验（TODO #14 会话三档控制）。

import "testing"

// TestValidGear 验证枚举校验。
func TestValidGear(t *testing.T) {
	for _, g := range []string{GearAuto, GearFast, GearCluster} {
		if !ValidGear(g) {
			t.Errorf("ValidGear(%q) = false, want true", g)
		}
	}
	for _, g := range []string{"", "explore", "warp", "Auto"} {
		if ValidGear(g) {
			t.Errorf("ValidGear(%q) = true, want false", g)
		}
	}
}
