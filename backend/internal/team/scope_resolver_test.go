package team

// scope_resolver_test.go 覆盖 2026-11-19 再次验收 P0-4：路径识别必须有可靠边界。
//
// 旧实现的实锤：输入 `修改src/a.css的间距` 得到文件 `修改src/a.css` 与目录 `修改src/**`
// ——既不是精确授权，也让后续路径检查与语义目标归属全部失效（正则把中文动词吞进路径，
// 且文件路径中间的分隔符被当成目录 token）。

import (
	"os"
	"path/filepath"
	"testing"
)

// scopeValues 取 WriteSet 的 "kind:value" 列表（断言用）。
func scopeValues(r ScopeResolution) []string {
	out := make([]string, 0, len(r.WriteSet))
	for _, rule := range r.WriteSet {
		out = append(out, rule.Kind+":"+rule.Value)
	}
	return out
}

func containsValue(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// TestResolveScopeChineseNoSpaceIsExact 中文无空格请求必须只解析出精确文件路径。
func TestResolveScopeChineseNoSpaceIsExact(t *testing.T) {
	res := ResolveScope("修改src/a.css的间距", "", nil)
	got := scopeValues(res)
	if len(got) != 1 || got[0] != "path:src/a.css" {
		t.Fatalf("中文无空格输入必须精确解析目标文件：%v", got)
	}
	if res.Ambiguous {
		t.Fatal("解析成功后不得标记歧义")
	}
}

// TestResolveScopeCarriesPluralChineseNoSpace 中文动词重叠、多文件场景。
func TestResolveScopeCarriesPluralChineseNoSpace(t *testing.T) {
	res := ResolveScope("请把web/src/a.css和web/src/b.vue都改一下", "", nil)
	got := scopeValues(res)
	if !containsValue(got, "path:web/src/a.css") || !containsValue(got, "path:web/src/b.vue") {
		t.Fatalf("两个文件都必须进入写范围：%v", got)
	}
	for _, v := range got {
		if v == "glob:web/src/**" || v == "glob:修改src/**" {
			t.Fatalf("不得凭空授予目录写权限：%v", got)
		}
	}
}

// TestResolveScopeChineseDirNameKept 真实中文目录名必须保留（不被误当动词前缀剥离）。
func TestResolveScopeChineseDirNameKept(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "文档"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "文档", "样式"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "文档", "样式", "a.css"), []byte(".a{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := ResolveScope("改一下文档/样式/a.css", root, nil)
	got := scopeValues(res)
	if len(got) != 1 || got[0] != "path:文档/样式/a.css" {
		t.Fatalf("中文目录名必须保留：%v", got)
	}
}

// TestResolveScopeMixedChineseEnglishInline 中英混写（中文与英文路径直接相邻）。
func TestResolveScopeMixedChineseEnglishInline(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"把Order.vue的标题改掉", "path:Order.vue"},
		{"修改src/styles/a.scss的间距", "path:src/styles/a.scss"},
		{"请检查web/src/view/disOrder.vue", "path:web/src/view/disOrder.vue"},
	}
	for _, c := range cases {
		res := ResolveScope(c.input, "", nil)
		got := scopeValues(res)
		if !containsValue(got, c.want) {
			t.Fatalf("%q 必须解析出 %s，实际 %v", c.input, c.want, got)
		}
	}
}

// TestResolveScopeSeparatedBySpaces 空格分隔/标点包裹路径。
func TestResolveScopeSeparatedBySpaces(t *testing.T) {
	cases := []string{
		"修改 src/a.css 的间距",
		"修改（src/a.css）的间距",
		"改一下 `src/a.css`，谢谢",
	}
	for _, input := range cases {
		res := ResolveScope(input, "", nil)
		got := scopeValues(res)
		if !containsValue(got, "path:src/a.css") {
			t.Fatalf("%q 必须解析出 path:src/a.css，实际 %v", input, got)
		}
	}
}

// TestResolveScopeDirectoryTokenIsolated 独立目录 token（后接分隔符/标点）才授目录权。
func TestResolveScopeDirectoryTokenIsolated(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := ResolveScope("web/src/ 下面所有样式都加 2px", root, nil)
	got := scopeValues(res)
	if !containsValue(got, "glob:web/src/**") {
		t.Fatalf("独立目录 token 必须授目录范围：%v", got)
	}
}

// TestResolveScopeNonexistentDirectoryNotGranted 不存在的目录不授予（宁少不多）。
func TestResolveScopeNonexistentDirectoryNotGranted(t *testing.T) {
	root := t.TempDir()
	res := ResolveScope("web/nosuch/ 下面都改", root, nil)
	got := scopeValues(res)
	for _, v := range got {
		if v == "glob:web/nosuch/**" {
			t.Fatalf("不存在的目录不得授权：%v", got)
		}
	}
	if !res.Ambiguous {
		t.Fatalf("无任何可写资源时必须标歧义：%+v", res)
	}
}

// TestResolveScopeRelativeAndAbsolute 绝对路径与上跳路径一律拒绝（只收工作区相对路径）。
func TestResolveScopeRelativeAndAbsolute(t *testing.T) {
	for _, input := range []string{"改 /etc/passwd.css", "改 ../../outside/a.css", `改 C:\Windows\a.css`} {
		res := ResolveScope(input, "", nil)
		for _, rule := range res.WriteSet {
			if !isCleanRelPath(rule.Value) {
				t.Fatalf("%q 解析出越界路径：%+v", input, rule)
			}
		}
	}
}

// TestResolveScopeNegationGoesToExcluded "不要动 X" 进 ExcludedSet 且恒 deny。
func TestResolveScopeNegationGoesToExcluded(t *testing.T) {
	res := ResolveScope("改 src/a.css，但不要动 src/b.css", "", nil)
	if !containsValue(scopeValues(res), "path:src/a.css") {
		t.Fatalf("允许写入应含 src/a.css：%v", scopeValues(res))
	}
	found := false
	for _, rule := range res.ExcludedSet {
		if rule.Value == "src/b.css" && rule.Effect == RuleEffectDeny {
			found = true
		}
	}
	if !found {
		t.Fatalf("否定句目标必须进 ExcludedSet（deny）：%+v", res.ExcludedSet)
	}
}

// TestResolveScopeNewFileAllowed 拟新建（不存在）的文件仍记 allow，并留 Note。
func TestResolveScopeNewFileAllowed(t *testing.T) {
	root := t.TempDir()
	res := ResolveScope("新建 src/NewStyle.scss 写样式", root, nil)
	if !containsValue(scopeValues(res), "path:src/NewStyle.scss") {
		t.Fatalf("拟新建文件必须进写范围：%v", scopeValues(res))
	}
	noted := false
	for _, n := range res.Notes {
		if len(n) > 0 {
			noted = true
			break
		}
	}
	if !noted {
		t.Fatalf("不存在的路径必须留 Note（可审计）：%+v", res)
	}
}

// TestResolveScopeNoPathIsAmbiguous 完全没给路径 → Ambiguous（合同保持空 WriteSet）。
func TestResolveScopeNoPathIsAmbiguous(t *testing.T) {
	res := ResolveScope("把那个标题改好看一点", "", nil)
	if !res.Ambiguous || len(res.WriteSet) != 0 {
		t.Fatalf("无路径输入必须标歧义且不授权：%+v", res)
	}
}

// TestResolveScopeCandidatesOutsideRangeDropped 语义目标超出写范围时不得当授权。
func TestResolveScopeCandidatesOutsideRangeDropped(t *testing.T) {
	cands := []SemanticTarget{
		{ID: "t-in", File: "src/a.css", Kind: "css_declaration"},
		{ID: "t-out", File: "src/z.css", Kind: "css_declaration"},
	}
	res := ResolveScope("改 src/a.css", "", cands)
	if len(res.Targets) != 1 || res.Targets[0].ID != "t-in" {
		t.Fatalf("只有范围内的候选可保留：%+v", res.Targets)
	}
}

// TestStripHanVerbPrefixBoundaries 边界修复的单元规则（防回归）。
func TestStripHanVerbPrefixBoundaries(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		stripped bool
	}{
		{"修改src/a.css", "src/a.css", true},
		{"文档/a.css", "文档/a.css", false}, // 中文目录名
		{"src/a.css", "src/a.css", false},
		{"请把订单页改一下", "请把订单页改一下", false}, // 全中文：不是路径
	}
	for _, c := range cases {
		got, stripped := stripHanVerbPrefix(c.in)
		if got != c.want || stripped != c.stripped {
			t.Fatalf("%q → (%q,%v)，want (%q,%v)", c.in, got, stripped, c.want, c.stripped)
		}
	}
}
