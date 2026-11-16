package plugins

// dockerlaunch_test.go Docker 引擎探测与拉起的纯函数部分。
//
// 不测"真的把 Docker Desktop 拉起来"（有副作用、依赖机器状态），
// 只测错误文案处理与探测在引擎不可用时的行为。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDockerErrTail(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{"多行取首行", "failed to connect to the docker API\nat npipe:////./pipe", nil, "failed to connect to the docker API"},
		{"空输出用 err", "", errors.New("exec: not found"), "exec: not found"},
		{"全空", "", nil, "未知错误"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dockerErrTail([]byte(c.out), c.err); got != c.want {
				t.Fatalf("dockerErrTail = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDockerErrText(t *testing.T) {
	if got := dockerErrText(nil); got != "未知错误" {
		t.Fatalf("nil 错误文案 = %q", got)
	}
	err := errors.New(DockerUnavailableReason + ": failed to connect to the docker API")
	got := dockerErrText(err)
	if strings.Contains(got, "Docker 引擎不可用") {
		t.Fatalf("应剥掉前缀，只留底层原因，实际 = %q", got)
	}
	if got != "failed to connect to the docker API" {
		t.Fatalf("dockerErrText = %q", got)
	}
}

// TestDockerEngineReadyReportsReason 引擎不可用时必须给出可读原因（含行动指引）。
func TestDockerEngineReadyReportsReason(t *testing.T) {
	err := DockerEngineReady(context.Background())
	if err == nil {
		t.Skip("本机 Docker 引擎可用，跳过")
	}
	if !strings.Contains(err.Error(), "Docker 引擎不可用") {
		t.Fatalf("错误文案应说明引擎不可用，实际 = %v", err)
	}
	if !strings.Contains(err.Error(), "启动 Docker") {
		t.Fatalf("错误文案应给出行动指引（启动 Docker），实际 = %v", err)
	}
}
