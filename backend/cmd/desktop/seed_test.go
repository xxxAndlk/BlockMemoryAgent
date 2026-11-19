//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyInstallSeed 验证安装向导种子合并进 .env 的行为。
//
// 关键契约：
//   - 种子存在 → 值写进 .env，种子被删除（下次不再重复合并）；
//   - .env 里已有的**其它**配置（尤其中文注释、无关项）原样保留；
//   - 空字段不覆盖 .env 已有的值（向导允许只填 Key 不填 URL）；
//   - 没有种子 / 种子是空 Key → 不动 .env，也不报错（正常升级场景）。
func TestApplyInstallSeed(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	seedPath := filepath.Join(dir, "config", "install-seed.json")

	base := "# 中文注释要保住\n" +
		"ANTHROPIC_API_KEY=sk-ant-placeholder\n" +
		"ANTHROPIC_BASE_URL=https://api.anthropic.com\n" +
		"EMBEDDING_BASE_URL=https://old-embed.example/v1\n" +
		"EMBEDDING_API_KEY=\n" +
		"REDIS_PASSWORD=blockmemory_dev\n"
	if err := os.WriteFile(envPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{
  "model_api_key": "sk-ant-real-key",
  "model_base_url": "https://api.deepseek.com/v1",
  "embed_base_url": "",
  "embed_api_key": "sk-embed-real",
  "provider": "1"
}`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if !applyInstallSeed(dir) {
		t.Fatal("applyInstallSeed 应返回 true（有有效种子）")
	}

	got := readEnvFile(t, envPath)
	checks := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-ant-real-key",
		"ANTHROPIC_BASE_URL": "https://api.deepseek.com/v1",
		"EMBEDDING_API_KEY":  "sk-embed-real",
		// 种子里 embed_base_url 为空 → 不覆盖 .env 已有值
		"EMBEDDING_BASE_URL": "https://old-embed.example/v1",
		"REDIS_PASSWORD":     "blockmemory_dev",
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if !strings.Contains(readFile(t, envPath), "中文注释要保住") {
		t.Error("合并过程破坏了 .env 里的中文注释")
	}
	if _, err := os.Stat(seedPath); !os.IsNotExist(err) {
		t.Error("合并成功后应删除种子文件，实际仍存在")
	}

	// 再跑一次：种子已删 → 不重复合并，返回 false
	if applyInstallSeed(dir) {
		t.Error("种子已消费时应返回 false")
	}
}

// TestApplyInstallSeed_EmptyKey 种子里的 Key 是占位值 → 视为无效，不动 .env。
func TestApplyInstallSeed_EmptyKey(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	orig := "ANTHROPIC_API_KEY=keep-me\n"
	if err := os.WriteFile(envPath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(dir, "config", "install-seed.json")
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte(`{"model_api_key":"","embed_api_key":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if applyInstallSeed(dir) {
		t.Error("空 Key 种子不应触发合并")
	}
	if got := readEnvFile(t, envPath)["ANTHROPIC_API_KEY"]; got != "keep-me" {
		t.Errorf("空种子不应改动 .env，实际 ANTHROPIC_API_KEY=%q", got)
	}
}

// TestApplyInstallSeed_NoSeed 没有种子文件是所有正常升级的常态。
func TestApplyInstallSeed_NoSeed(t *testing.T) {
	dir := t.TempDir()
	if applyInstallSeed(dir) {
		t.Error("没有种子文件时应返回 false")
	}
	if readInstallSeed(dir) != nil {
		t.Error("没有种子文件时 readInstallSeed 应返回 nil")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// readEnvFile 用与生产同一个解析口径读 .env（KEY=VALUE，忽略注释与空行）。
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(readFile(t, path), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		out[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
	}
	return out
}
