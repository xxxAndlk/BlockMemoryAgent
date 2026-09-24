package agent

// memory_index_test.go 记忆索引槽（TODO #20③+#22③）：双配额、超限重写指令（不静默截断）、
// untrusted 围栏识别（provenance 门 + 召回循环防护）。

import (
	"strings"
	"testing"
)

func entries(n int) []MemoryIndexEntry {
	out := make([]MemoryIndexEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, MemoryIndexEntry{Type: "项目经验", Summary: "条目"})
	}
	return out
}

func TestRenderMemoryIndex_QuotasAndRewrite(t *testing.T) {
	// 行数超限：只注入 maxLines 行 + 重写指令（不静默截断）。
	block, over := RenderMemoryIndex(entries(5), 3, 100000)
	if !over {
		t.Fatal("5 entries vs 3 lines: over must be true")
	}
	if got := strings.Count(block, "- [项目经验]"); got != 3 {
		t.Fatalf("rendered lines = %d, want 3", got)
	}
	if !strings.Contains(block, "重写索引") {
		t.Fatalf("over-limit block must carry rewrite instruction, got:\n%s", block)
	}

	// runes 超限：按 runes 截停 + 重写指令。
	block2, over2 := RenderMemoryIndex(entries(5), 100, 50)
	if !over2 {
		t.Fatal("runes overflow must set over=true")
	}
	if !strings.Contains(block2, "重写索引") {
		t.Fatalf("runes overflow block must carry rewrite instruction, got:\n%s", block2)
	}

	// 不超限：无指令、全量注入。
	block3, over3 := RenderMemoryIndex(entries(2), 3, 100000)
	if over3 || strings.Contains(block3, "重写索引") {
		t.Fatalf("within quota must be clean, over=%v block=%s", over3, block3)
	}

	// 空条目：空块零行为。
	if block4, over4 := RenderMemoryIndex(nil, 3, 100); block4 != "" || over4 {
		t.Fatalf("empty entries must render nothing, got %q over=%v", block4, over4)
	}
}

func TestContainsUntrustedFence(t *testing.T) {
	if ContainsUntrustedFence("普通沉淀条目") {
		t.Error("clean text must pass provenance gate")
	}
	if !ContainsUntrustedFence(`x <untrusted_data source="web"> 内容 </untrusted_data> y`) {
		t.Error("fenced content must be rejected (recall loop protection)")
	}
}
