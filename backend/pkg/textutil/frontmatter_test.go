package textutil

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseFrontmatter_Basic 验证标准围栏解析与标量/数组折叠。
func TestParseFrontmatter_Basic(t *testing.T) {
	data := []byte("---\nname: demo\ndescription: 演示技能\ntags: [a, b]\n---\n正文第一行\n正文第二行")
	fm, body := ParseFrontmatter(data)
	if fm["name"] != "demo" || fm["description"] != "演示技能" {
		t.Fatalf("frontmatter mismatch: %#v", fm)
	}
	if fm["tags"] != "a,b" {
		t.Fatalf("expected tags folded to a,b, got %q", fm["tags"])
	}
	if body != "正文第一行\n正文第二行" {
		t.Fatalf("unexpected body: %q", body)
	}
}

// TestParseFrontmatter_CRLF 验证 Windows 换行（\r\n）下围栏解析正常。
func TestParseFrontmatter_CRLF(t *testing.T) {
	data := []byte("---\r\nname: demo\r\ndescription: d\r\n---\r\nbody\r\n")
	fm, body := ParseFrontmatter(data)
	if fm["name"] != "demo" {
		t.Fatalf("expected name=demo, got %#v", fm)
	}
	if strings.Contains(body, "---") {
		t.Fatalf("body should not contain fence: %q", body)
	}
}

// TestParseFrontmatter_NoFrontmatter 验证无围栏时返回 nil map 与全文。
func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	text := "直接正文，没有围栏"
	fm, body := ParseFrontmatter([]byte(text))
	if fm != nil || body != text {
		t.Fatalf("expected nil map + full text, got %#v %q", fm, body)
	}
}

// TestParseFrontmatter_UnclosedFence 验证围栏未闭合时按无 frontmatter 处理。
func TestParseFrontmatter_UnclosedFence(t *testing.T) {
	text := "---\nname: demo\n没有闭合围栏"
	fm, body := ParseFrontmatter([]byte(text))
	if fm != nil || body != text {
		t.Fatalf("expected nil map + full text, got %#v %q", fm, body)
	}
}

// TestSanitizeID 验证 ID 规整规则（大写折叠、非法字符转下划线）。
func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"PDF-Extract.v2": "pdf_extract_v2",
		"中文 skill":      "___skill",
		"already_ok":     "already_ok",
	}
	for in, want := range cases {
		if got := SanitizeID(in); got != want {
			t.Errorf("SanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseFrontmatter_UnknownFieldsTolerated 验证未知字段进入 map（由调用方忽略）。
func TestParseFrontmatter_UnknownFieldsTolerated(t *testing.T) {
	fm, _ := ParseFrontmatter([]byte("---\nname: x\ndescription: y\nallowed-tools: A, B\n---\nbody"))
	if !reflect.DeepEqual(fm["allowed-tools"], "A, B") {
		t.Fatalf("expected unknown field preserved, got %#v", fm)
	}
}
