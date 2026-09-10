package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/prompts"
)

// TestLoadRoleConfig_FillsPromptsFromCode 提示词由 pkg/prompts 内置填充：
// YAML 不写 system_prompt/prompt_template，加载后字段从代码注册表填充，
// 叶子公共段占位符已展开、无残留。
func TestLoadRoleConfig_FillsPromptsFromCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")
	yaml := `fixed_roles:
  - id: code_assistant
    name: 代码助手
    type: fixed
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRoleConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.MetaAgent.SystemPrompt == "" {
		t.Fatal("meta 提示词未从代码填充")
	}
	if cfg.DomainAgent.SystemPrompt == "" {
		t.Fatal("domain 提示词未从代码填充")
	}

	role := cfg.GetFixedRole("code_assistant")
	if role == nil {
		t.Fatal("code_assistant 未加载")
	}
	sp := role.SystemPrompt
	if strings.Contains(sp, prompts.LeafCommonToken) {
		t.Fatalf("加载后仍含占位符:\n%s", sp)
	}
	if !strings.Contains(sp, "【终止纪律】") || !strings.Contains(sp, "【共享记忆】") {
		t.Fatalf("公共段未注入完整:\n%s", sp)
	}
	// 展开内容应与单一来源常量逐行一致（0 列对齐）。
	if !strings.Contains(sp, "\n"+prompts.LeafCommonBlock+"\n") {
		t.Fatalf("展开内容与单一来源常量不一致:\n%s", sp)
	}
}

// TestLoadRoleConfig_UnknownFixedRoleID 未在 pkg/prompts 登记的固定角色 ID
// 报 strict 错误（自定义固定角色需改代码，YAML 纯配置添加不再支持）。
func TestLoadRoleConfig_UnknownFixedRoleID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")
	yaml := `fixed_roles:
  - id: no_such_role
    name: 幽灵角色
    type: fixed
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRoleConfig(path)
	if err == nil {
		t.Fatal("未知固定角色 ID 应报错")
	}
	if !strings.Contains(err.Error(), "no_such_role") {
		t.Fatalf("错误未指明角色 ID: %v", err)
	}
}

// TestLoadRoleConfig_ResolvesModelEnvRef 模型名字段同样支持 ${VAR} 引用
// （ui_assistant model: ${GEMINI_CHAT_MODEL} 场景）；未设置时为空而非字面量残留。
func TestLoadRoleConfig_ResolvesModelEnvRef(t *testing.T) {
	t.Setenv("BMA_TEST_CHAT_MODEL", "gemini-flash")
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")
	yaml := `fixed_roles:
  - id: ui_assistant
    name: UI助手
    type: fixed
    model_config:
      provider: openai-chat
      model: ${BMA_TEST_CHAT_MODEL}
      api_key: xxx
      base_url: ${BMA_TEST_CHAT_URL}
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRoleConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.GetFixedRole("ui_assistant")
	if role == nil {
		t.Fatal("ui_assistant 未加载")
	}
	if got := role.ModelConfig.Model; got != "gemini-flash" {
		t.Fatalf("model 环境变量未解析: %q", got)
	}
}
