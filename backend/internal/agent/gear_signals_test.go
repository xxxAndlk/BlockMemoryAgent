// gear_signals_test.go 选档误判隐性信号测试（TODO #14 T18）：表驱动覆盖四象限
// （档位 × 时间窗 × 动词），并钉住"非对应档位/超窗/零值不触发"的负例。
package agent

import (
	"testing"
	"time"
)

func TestGearSignalOnSend(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		gear    string
		endedAt *time.Time
		content string
		want    bool
	}{
		{"快速档+刚结束+动词→命中", "fast", ptrTime(now.Add(-2 * time.Minute)), "帮我修一下登录报错", true},
		{"快速档+刚结束+无动词→不命中", "fast", ptrTime(now.Add(-2 * time.Minute)), "谢谢你刚才的解答", false},
		{"快速档+超窗→不命中", "fast", ptrTime(now.Add(-10 * time.Minute)), "帮我修一下登录报错", false},
		{"集群档+命中条件→不命中（只观测快速档）", "cluster", ptrTime(now.Add(-2 * time.Minute)), "帮我修一下登录报错", false},
		{"未设档位→不命中", "", ptrTime(now.Add(-2 * time.Minute)), "帮我修一下登录报错", false},
		{"快速档+从未终态→不命中", "fast", nil, "帮我修一下登录报错", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gearSignalOnSend(tc.gear, tc.endedAt, tc.content); (got != "") != tc.want {
				t.Fatalf("gearSignalOnSend(%q, ...) detail=%q, want hit=%v", tc.gear, got, tc.want)
			}
		})
	}
}

func TestGearSignalOnStop(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		gear   string
		start  time.Time
		want   bool
	}{
		{"集群档+30s 内→命中", "cluster", now.Add(-10 * time.Second), true},
		{"集群档+超窗→不命中", "cluster", now.Add(-2 * time.Minute), false},
		{"快速档→不命中（只观测集群档）", "fast", now.Add(-10 * time.Second), false},
		{"未设档位→不命中", "", now.Add(-10 * time.Second), false},
		{"run 开始时刻零值→不命中", "cluster", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gearSignalOnStop(tc.gear, tc.start); (got != "") != tc.want {
				t.Fatalf("gearSignalOnStop(%q, ...) detail=%q, want hit=%v", tc.gear, got, tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
