package graph

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-kratos/blades"
)

func TestLoopDetectorDetectsRepeat(t *testing.T) {
	d := newLoopDetector(40)
	// maxRepeat=1：同一 (name,request) 出现 2 次即判定循环。
	for i := 0; i < 2; i++ {
		m := &blades.Message{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.NewToolPart("id", "ReadFile", `{"path":"/same"}`),
			},
		}
		stop, reason := d.observe(m)
		if i == 0 && stop {
			t.Fatalf("第%d次不应触发循环，got %s", i+1, reason)
		}
		if i == 1 && !stop {
			t.Fatal("第二次重复调用应触发循环检测")
		}
		if i == 1 && !strings.Contains(reason, "ReadFile") {
			t.Fatalf("原因应包含工具名，got %s", reason)
		}
	}
}

func TestLoopDetectorDetectsEmptyStreak(t *testing.T) {
	d := newLoopDetector(40)
	for i := 0; i < 4; i++ {
		req := fmt.Sprintf(`{"command":"cmd%d"}`, i)
		m := &blades.Message{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.NewToolPart("id", "RunCommand", req),
			},
		}
		stop, reason := d.observe(m)
		if i < 3 && stop {
			t.Fatalf("第%d次不应触发空转，got %s", i+1, reason)
		}
		if i == 3 && !stop {
			t.Fatal("连续4轮无文本工具调用应触发空转检测")
		}
		if i == 3 && !strings.Contains(reason, "空转") {
			t.Fatalf("原因应提示空转，got %s", reason)
		}
	}
}

func TestLoopDetectorTextResetsEmptyStreak(t *testing.T) {
	d := newLoopDetector(40)
	for i := 0; i < 4; i++ {
		req := fmt.Sprintf(`{"command":"cmd%d"}`, i)
		m := &blades.Message{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.TextPart{Text: "progress"},
				blades.NewToolPart("id", "RunCommand", req),
			},
		}
		if stop, reason := d.observe(m); stop {
			t.Fatalf("有文本输出不应触发空转，got %s", reason)
		}
	}
}
