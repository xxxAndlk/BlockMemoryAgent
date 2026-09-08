package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandLeafCommonDiscipline 锚定叶子公共段展开结果：
// 占位符行按原缩进逐行展开，展开后无占位符残留，内容与 leafCommonBlock 一致。
func TestExpandLeafCommonDiscipline(t *testing.T) {
	in := "你是叶子执行者。\n\n      【执行纪律】\n      " + leafCommonToken + "\n\n      【职责】\n      1. 干活\n"
	got := expandLeafCommonDiscipline(in)

	if strings.Contains(got, leafCommonToken) {
		t.Fatalf("占位符未展开:\n%s", got)
	}
	for _, want := range strings.Split(leafCommonBlock, "\n") {
		if want == "" {
			continue
		}
		if !strings.Contains(got, "      "+want) {
			t.Fatalf("展开结果缺少公共段行（6 空格缩进）:\n%q", want)
		}
	}
	if !strings.Contains(got, "\n\n      【职责】") {
		t.Fatalf("占位符行后的原有内容丢失:\n%s", got)
	}

	// 无占位符的提示词必须原样返回。
	plain := "没有占位符的提示词"
	if got := expandLeafCommonDiscipline(plain); got != plain {
		t.Fatalf("无占位符时不应改动原文: %q", got)
	}
}

// TestLoadRoleConfig_ExpandsLeafCommonToken 走完整加载链路验证占位符替换生效。
func TestLoadRoleConfig_ExpandsLeafCommonToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.yaml")
	yaml := `fixed_roles:
  - id: code_assistant
    name: 代码助手
    type: fixed
    system_prompt: |
      你是叶子执行者。

      【执行纪律】
      {{LEAF_COMMON_DISCIPLINE}}

      【职责】
      1. 写代码
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRoleConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.GetFixedRole("code_assistant")
	if role == nil {
		t.Fatal("code_assistant 未加载")
	}
	sp := role.SystemPrompt
	if strings.Contains(sp, leafCommonToken) {
		t.Fatalf("加载后仍含占位符:\n%s", sp)
	}
	if !strings.Contains(sp, "【终止纪律】") || !strings.Contains(sp, "【共享记忆】") {
		t.Fatalf("公共段未注入完整:\n%s", sp)
	}
	// 块标量解析后缩进已剥离，展开内容应与常量逐行一致（0 列对齐）。
	if !strings.Contains(sp, "\n"+leafCommonBlock+"\n") {
		t.Fatalf("展开内容与单一来源常量不一致:\n%s", sp)
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
