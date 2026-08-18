package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ${WORKDIR} 占位符展开：插件卷映射随启动目录解析（多目录多开互不串产物）。
func TestExpandWorkDir(t *testing.T) {
	t.Run("占位符展开为正斜杠绝对路径", func(t *testing.T) {
		// Windows 上 os.Getwd 返回反斜杠路径，docker -v 需统一为正斜杠。
		got := ExpandWorkDir(`${WORKDIR}/.bma/od-artifacts:/out`, `D:\data\proj`)
		if want := "D:/data/proj/.bma/od-artifacts:/out"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("整卷挂载点", func(t *testing.T) {
		got := ExpandWorkDir(`${WORKDIR}:/workspace`, "/home/u/proj")
		if want := "/home/u/proj:/workspace"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("无占位符原样返回", func(t *testing.T) {
		for _, v := range []string{"bma-open-design-data:/app/.od", "D:/abs/path:/out", "D:/data/x:/workspace"} {
			if got := ExpandWorkDir(v, "/anywhere"); got != v {
				t.Fatalf("got %q, want %q", got, v)
			}
		}
	})
	t.Run("空 workDir 回退 os.Getwd", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Skip("os.Getwd 不可用")
		}
		got := ExpandWorkDir(`${WORKDIR}:/workspace`, "")
		if want := filepath.ToSlash(wd) + ":/workspace"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("不吞环境变量形态", func(t *testing.T) {
		// ${VAR:default} 形态在配置加载期已插值；此处不含 ${WORKDIR} 时必须原样透传。
		v := "${OD_API_TOKEN:}"
		if got := ExpandWorkDir(v, "/x"); got != v {
			t.Fatalf("got %q, want %q", got, v)
		}
	})
}

// service 插件 Init 展开 volumes 中的 ${WORKDIR}。
func TestServiceInitExpandsWorkDir(t *testing.T) {
	p := newServicePlugin("svc", map[string]any{
		"image":   "img:tag",
		"volumes": []any{`${WORKDIR}/data:/data`, "named-vol:/cache"},
	}, nil).(*servicePlugin)
	if err := p.Init(context.Background(), Deps{WorkDir: `D:\data\proj`}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if want := "D:/data/proj/data:/data"; p.settings.Volumes[0] != want {
		t.Fatalf("Volumes[0] = %q, want %q", p.settings.Volumes[0], want)
	}
	if p.settings.Volumes[1] != "named-vol:/cache" {
		t.Fatalf("命名卷被误改: %q", p.settings.Volumes[1])
	}
	if !strings.HasPrefix(p.settings.Volumes[0], "D:/") {
		t.Fatalf("应展开为绝对路径: %q", p.settings.Volumes[0])
	}
}
