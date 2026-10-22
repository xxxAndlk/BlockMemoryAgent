package userprofile

// store_test.go 验证 TODO #28 用户画像存储：
// 加载/追加（新小节/已有小节）/全量覆盖/缺失文件容忍。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_MissingFileTolerated(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nope.md"))
	if err := s.Load(); err != nil {
		t.Fatalf("missing profile file should load as empty: %v", err)
	}
	if s.Current().Content != "" {
		t.Fatalf("expected empty content, got %q", s.Current().Content)
	}
}

func TestStore_AppendCreatesSectionAndLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.md")
	s := NewStore(path)
	_ = s.Load()

	if err := s.Append("偏好", "直接给结论不要铺垫"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append("偏好", "用 Go 写后端"); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if err := s.Append("沟通风格", "中文回答"); err != nil {
		t.Fatalf("Append other section: %v", err)
	}

	content := s.Current().Content
	if !strings.Contains(content, "## 偏好") || !strings.Contains(content, "直接给结论不要铺垫") {
		t.Fatalf("偏好 section missing: %s", content)
	}
	if !strings.Contains(content, "用 Go 写后端") {
		t.Fatalf("second preference missing: %s", content)
	}
	if !strings.Contains(content, "## 沟通风格") || !strings.Contains(content, "中文回答") {
		t.Fatalf("沟通风格 section missing: %s", content)
	}
	// 磁盘可读（人可直接编辑、git 可追踪）。
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "直接给结论不要铺垫") {
		t.Fatalf("disk content missing append: %v", err)
	}
}

func TestStore_SaveOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.md")
	s := NewStore(path)
	_ = s.Load()
	_ = s.Append("偏好", "旧内容")

	if err := s.Save("# 用户画像\n\n## 偏好\n- 新内容（2026-08-10）\n"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	content := s.Current().Content
	if strings.Contains(content, "旧内容") || !strings.Contains(content, "新内容") {
		t.Fatalf("Save should fully replace, got: %s", content)
	}
}

// TestStore_AppendBeforeNextSectionKeepsLineBoundary 验证向"非末尾小节"追加时
// 新行不会拼到上一行末尾（2026-10-22 线上用户画像出现 "- 旧行（时间）- 新行（时间）"）。
func TestStore_AppendBeforeNextSectionKeepsLineBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.md")
	seed := "# 用户画像\n\n## 反馈记录\n- 旧归档行（2026-10-20 10:00）\n\n## 技术栈\n- 技术行（2026-10-20 10:00）\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := s.Append("反馈记录", "新归档行"); err != nil {
		t.Fatalf("append: %v", err)
	}
	content := s.Current().Content
	if strings.Contains(content, "）- 新归档行") || strings.Contains(content, "）- 新归档行") {
		t.Fatalf("新行被拼到上一行末尾:\n%s", content)
	}
	if !strings.Contains(content, "- 新归档行（") {
		t.Fatalf("新行缺失:\n%s", content)
	}
	// 技术栈小节必须还在。
	if !strings.Contains(content, "## 技术栈") || !strings.Contains(content, "- 技术行（") {
		t.Fatalf("下一小节被破坏:\n%s", content)
	}
}
