package mcpbridge

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/plugins"
)

func TestDockerContainerName(t *testing.T) {
	pidSuffix := fmt.Sprintf("-%d", os.Getpid())
	cases := map[string]string{
		"web_search":        "bma-plugin-web_search",
		"bundle/dir/server": "bma-plugin-bundle-dir-server",
		"deep_research@v2":  "bma-plugin-deep_research-v2",
		"UPPER.lower_1-2":   "bma-plugin-UPPER.lower_1-2",
	}
	for id, wantPrefix := range cases {
		got := dockerContainerName(id)
		if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, pidSuffix) {
			t.Errorf("dockerContainerName(%q) = %q, want 前缀 %q + pid 后缀 %q", id, got, wantPrefix, pidSuffix)
		}
	}
}

func TestDockerRunArgs(t *testing.T) {
	s := Settings{
		Transport: "docker",
		Image:     "bma/firecrawl-mcp:local",
		Args:      []string{"--verbose"},
		Ports:     []string{"6081:6081"},
		Env: map[string]string{
			"FIRECRAWL_API_URL": "http://host.docker.internal:3002",
			"FIRECRAWL_API_KEY": "self-hosted",
		},
	}
	name, args := dockerRunArgs("web_search", s)
	if !strings.HasPrefix(name, "bma-plugin-web_search-") {
		t.Fatalf("name = %q", name)
	}
	want := []string{
		"run", "-i", "--rm", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", "6081:6081",
		"-e", "FIRECRAWL_API_KEY=self-hosted",
		"-e", "FIRECRAWL_API_URL=http://host.docker.internal:3002",
		"bma/firecrawl-mcp:local", "--verbose",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}

func TestFromSettingsDocker(t *testing.T) {
	s := FromSettings(map[string]any{
		"transport": "docker",
		"image":     "img:tag",
		"env":       map[string]any{"K": "V"},
	})
	if s.Transport != "docker" || s.Image != "img:tag" || s.Env["K"] != "V" {
		t.Fatalf("FromSettings = %+v", s)
	}

	b := New("p", s, nil)
	if err := b.Init(context.Background(), plugins.Deps{}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	b2 := New("p2", FromSettings(map[string]any{"transport": "docker"}), nil)
	if err := b2.Init(context.Background(), plugins.Deps{}); err == nil {
		t.Fatal("docker transport 缺 image 应报错")
	}
}
