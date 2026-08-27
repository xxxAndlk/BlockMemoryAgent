package agent

// idle_roster_context_test.go 验证【空闲领域Agent】清单渲染含职责/写过文件字段。

import (
	"strings"
	"testing"
	"time"
)

func TestRenderIdleRoster_IncludesRespAndFiles(t *testing.T) {
	txt := renderIdleRoster([]IdleDomainInfo{
		{
			AgentID:      "s1/domain-1",
			Domain:       "jiujie",
			Resp:         "负责九劫服务器部署",
			WrittenFiles: []string{"app.conf", "nginx.conf"},
			LastTask:     "部署完成",
			ReuseCount:   2,
			IdleLeft:     90 * time.Minute,
		},
	})
	for _, want := range []string{
		"【空闲领域Agent】",
		"id=s1/domain-1",
		"领域=jiujie",
		"职责=负责九劫服务器部署",
		"写过=app.conf,nginx.conf",
		"最近任务=部署完成",
		"空闲可复用",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("roster text missing %q:\n%s", want, txt)
		}
	}
}

func TestRenderIdleRoster_EmptyNoInject(t *testing.T) {
	if txt := renderIdleRoster(nil); txt != "" {
		t.Fatalf("empty roster should render empty, got %q", txt)
	}
}
