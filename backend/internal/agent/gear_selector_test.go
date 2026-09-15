package agent

// gear_selector_test.go 表驱动验证规则选档器（TODO #14）：
// 核心不变式——误判方向永远安全：闲聊误判成 cluster 只是多等一会；
// 任务误判成 fast 会得浅答案（绝不放过，动词表从宽）。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestSelectGear(t *testing.T) {
	cases := []struct {
		goal string
		want string
	}{
		// 明确闲聊：短、无载体特征、无动词 → fast。
		{"你好", tool.GearFast},
		{"在吗", tool.GearFast},
		{"谢谢", tool.GearFast},
		{"1+1等于几", tool.GearFast},
		{"今天天气怎么样", tool.GearFast},
		{"解释一下什么是闭包", tool.GearFast},
		{"给我讲个笑话", tool.GearFast},

		// 任务型：动词命中 → cluster。
		{"帮我写一个爬虫", tool.GearCluster},
		{"修复登录页的报错", tool.GearCluster},
		{"把配置改成生产环境", tool.GearCluster},
		{"优化这段SQL", tool.GearCluster},
		{"rebuild the index", tool.GearCluster},
		{"fix the login bug", tool.GearCluster},

		// 任务载体特征：路径/URL/扩展名 → cluster。
		{"src/main.go 这个文件是干嘛的", tool.GearCluster},
		{"把 report.docx 总结成三句话", tool.GearCluster},
		{"https://example.com 上有什么", tool.GearCluster},
		{"C:\\data 目录多大", tool.GearCluster},

		// 边界：空/超长 → cluster（宁慢勿浅）；40 rune 整仍是闲聊（上限含）。
		{"", tool.GearCluster},
		{strings.Repeat("字", 41), tool.GearCluster},
		{strings.Repeat("字", 39) + "？", tool.GearFast},
	}
	for _, c := range cases {
		if got := SelectGear(c.goal); got != c.want {
			t.Errorf("SelectGear(%q) = %q, want %q", c.goal, got, c.want)
		}
	}
}
