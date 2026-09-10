package prompts

import (
	"strings"
	"testing"
)

// TestGet_AllRegisteredIDs 全部 10 个 roleID 可取且非空。
func TestGet_AllRegisteredIDs(t *testing.T) {
	ids := []string{
		"meta", "domain",
		"scout", "light", "code_assistant", "ui_assistant",
		"prompt_reviewer", "code_reviewer", "test_assistant", "doc_assistant",
	}
	for _, id := range ids {
		p, err := Get(id)
		if err != nil {
			t.Errorf("Get(%q): %v", id, err)
			continue
		}
		if strings.TrimSpace(p) == "" {
			t.Errorf("Get(%q) 返回空提示词", id)
		}
	}
}

// TestGet_UnknownID 未知 roleID 报错（config loader 转启动失败）。
func TestGet_UnknownID(t *testing.T) {
	if _, err := Get("no_such_role"); err == nil {
		t.Fatal("未知 roleID 应报错")
	}
}

// TestGet_ExpandsLeafCommonToken 叶子角色占位符已展开：
// 无占位符残留，公共段【终止纪律】/【共享记忆】注入且与单一来源常量逐行一致。
func TestGet_ExpandsLeafCommonToken(t *testing.T) {
	sp, err := Get("code_assistant")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sp, LeafCommonToken) {
		t.Fatalf("占位符未展开:\n%s", sp)
	}
	if !strings.Contains(sp, "【终止纪律】") || !strings.Contains(sp, "【共享记忆】") {
		t.Fatalf("公共段未注入完整:\n%s", sp)
	}
	if !strings.Contains(sp, "\n"+LeafCommonBlock+"\n") {
		t.Fatalf("展开内容与单一来源常量不一致:\n%s", sp)
	}
}

// TestExpandLeaf_IndentPreserved 带缩进的占位符行按原缩进逐行对齐展开。
func TestExpandLeaf_IndentPreserved(t *testing.T) {
	in := "你是叶子执行者。\n\n      【执行纪律】\n      " + LeafCommonToken + "\n\n      【职责】\n      1. 干活\n"
	got := expandLeaf(in)

	if strings.Contains(got, LeafCommonToken) {
		t.Fatalf("占位符未展开:\n%s", got)
	}
	for _, want := range strings.Split(LeafCommonBlock, "\n") {
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
	if got := expandLeaf(plain); got != plain {
		t.Fatalf("无占位符时不应改动原文: %q", got)
	}
}
