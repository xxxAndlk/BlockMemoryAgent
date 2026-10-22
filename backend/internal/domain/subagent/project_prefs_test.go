package subagent

import (
	"context"
	"strings"
	"testing"
)

// TestProjectPrefsPrefixStripsArchive 验证子 Agent 派发前缀不注入归档小节、
// 并按小节预算保留最新经验（2026-10-22 上下文污染修复）。
func TestProjectPrefsPrefixStripsArchive(t *testing.T) {
	content := `# 项目偏好

## 项目经验
- 现行方向：16 帧图集序列帧+动作表驱动（2026-10-22 21:50）
- 数字指标校验腿距（2026-10-22 16:54）

## 经验归档
- 已废止：骨骼拼接/枢轴拆分方案（2026-10-22 21:50）
`
	d := &Dispatcher{projectPrefs: func(ctx context.Context) string { return content }}
	out := d.projectPrefsPrefix(context.Background())
	if strings.Contains(out, "已废止") || strings.Contains(out, "骨骼拼接") {
		t.Fatalf("归档小节不应出现在子 Agent 前缀:\n%s", out)
	}
	if !strings.Contains(out, "现行方向") {
		t.Fatalf("现行项目经验应保留:\n%s", out)
	}
	if !strings.Contains(out, "【项目偏好】") {
		t.Fatalf("应带项目偏好段头:\n%s", out)
	}
}
