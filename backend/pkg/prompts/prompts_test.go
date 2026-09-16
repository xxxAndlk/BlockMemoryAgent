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

// TestDocAssistant_EscalateGuidance 快速档升档纪律契约（2026-09-16 fast 档改用
// doc_assistant 顶层）：提示词必须写明 escalate_gear 升档规程，否则快速档识别出
// 超范围任务时无路可走。
func TestDocAssistant_EscalateGuidance(t *testing.T) {
	if !strings.Contains(DocAssistant, "escalate_gear") {
		t.Fatal("DocAssistant 缺快速档升档纪律（escalate_gear）")
	}
	if !strings.Contains(DocAssistant, "【快速档直达模式】") {
		t.Fatal("DocAssistant 缺【快速档直达模式】段")
	}
}

// TestDomainAgent_DailyGearGuidance 日常档顶层升档规程（2026-09-16）：domain 提示词
// 必须写明「先探索、能力/范围不足才 escalate_gear」以及子 Agent 不适用，否则日常档
// 遇能力墙（如模型不支持读图）时无路可走或误在子 Agent 里调用被拒工具。
func TestDomainAgent_DailyGearGuidance(t *testing.T) {
	if !strings.Contains(DomainAgent, "【日常档直达模式】") {
		t.Fatal("DomainAgent 缺【日常档直达模式】段")
	}
	for _, want := range []string{"escalate_gear", "子 Agent 被派发时本段不适用"} {
		if !strings.Contains(DomainAgent, want) {
			t.Fatalf("DomainAgent 日常档段落缺 %q", want)
		}
	}
}

// TestDomainAgent_PeerQA 跨 Agent 协作问答纪律（2026-09-16）：domain 提示词必须写明
// 可以直问兄弟域（send_message request）且收到询问当轮必回（reply）——否则该通道
// 形同虚设（此前 send_message 只出现在续建/归因语境，实测零使用）。
func TestDomainAgent_PeerQA(t *testing.T) {
	for _, want := range []string{"【跨 Agent 协作问答】", "message_type=reply", "当轮必须回复"} {
		if !strings.Contains(DomainAgent, want) {
			t.Fatalf("DomainAgent 缺协作问答纪律 %q", want)
		}
	}
}

// TestMetaAgent_PeerQARelay 跨域问答不必经 Meta 中转（2026-09-16）。
func TestMetaAgent_PeerQARelay(t *testing.T) {
	for _, want := range []string{"跨域问答直连", "message_type=reply"} {
		if !strings.Contains(MetaAgent, want) {
			t.Fatalf("MetaAgent 缺跨域问答直连纪律 %q", want)
		}
	}
}

// TestMetaAgent_EnvironmentalFailureDiscipline 环境性失败不重派纪律（2026-09-16）：
// 子 Agent 报能力缺失（模型不支持读图等）时 Meta 不得原样重派同类域。
func TestMetaAgent_EnvironmentalFailureDiscipline(t *testing.T) {
	for _, want := range []string{"环境性/能力性失败不重派", "set_agent_model", "tool_mount"} {
		if !strings.Contains(MetaAgent, want) {
			t.Fatalf("MetaAgent 缺环境性失败重派纪律 %q", want)
		}
	}
}

// TestVersionPinned 版本钉契约（TODO #15 T15）：非空且符合 yyyymmdd-N 形态——
// bootstrap 启动日志拿它对照线上提示词版本，形态散了归因就没锚点。
func TestVersionPinned(t *testing.T) {
	if strings.TrimSpace(Version) == "" {
		t.Fatal("prompts.Version 不得为空")
	}
	if len(Version) != 10 || Version[8] != '-' || Version[9] < '1' || Version[9] > '9' {
		t.Fatalf("prompts.Version 应为 yyyymmdd-N 形态, got %q", Version)
	}
	for i, c := range Version[:8] {
		if c < '0' || c > '9' {
			t.Fatalf("Version 前缀应为 8 位日期数字, got %q at %d", Version, i)
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

// TestDirectMessageGuidance 钉死两段新规程的存在性（编排页用户直连的提示词契约）。
func TestDirectMessageGuidance(t *testing.T) {
	if !strings.Contains(DomainAgent, "【用户直连消息】") {
		t.Fatal("DomainAgent 缺用户直连消息处置规程")
	}
	for _, kw := range []string{"cancel_agent", "call_sub_agent", "进度"} {
		if !strings.Contains(DomainAgent, kw) {
			t.Fatalf("DomainAgent 用户直连规程缺关键词 %q", kw)
		}
	}
	if !strings.Contains(MetaAgent, "复活返工") {
		t.Fatal("MetaAgent 缺子 Agent 复活返工感知规程")
	}
}

// TestChineseDomainNameGuidance 钉死"子 Agent 展示名用中文领域名"的提示词契约
//（2026-09-12 用户实证：整棵树显示 doc-rev-a…doc-rev-i，用户读不懂谁在干什么）。
func TestChineseDomainNameGuidance(t *testing.T) {
	if !strings.Contains(MetaAgent, "中文领域名") {
		t.Fatal("MetaAgent 缺 domain 中文领域名硬约束")
	}
	// 答复里引用子 Agent 也要用中文领域名，不能甩 inst_id。
	if !strings.Contains(MetaAgent, "inst_id") {
		t.Fatal("MetaAgent 缺「引用子 Agent 用中文领域名而非 inst_id」约束")
	}
}

// TestVisualArtifactGuidance 钉死"可视成果要展示给用户"的提示词契约：
// 叶子公共段与 domain 提示词都必须写明 ShowArtifact——少一处，那条链路的角色就
// 只会贴路径、用户在对话栏什么也看不到。
func TestVisualArtifactGuidance(t *testing.T) {
	if !strings.Contains(LeafCommonBlock, "ShowArtifact") {
		t.Fatal("叶子公共段缺 ShowArtifact 展示指引")
	}
	if !strings.Contains(DomainAgent, "ShowArtifact") {
		t.Fatal("DomainAgent 缺 ShowArtifact 展示指引")
	}
	// 叶子展开后仍带该指引（占位符替换不能把它吃掉）。
	sp, err := Get("ui_assistant")
	if err != nil {
		t.Fatalf("Get(ui_assistant): %v", err)
	}
	if !strings.Contains(sp, "ShowArtifact") {
		t.Fatal("ui_assistant 展开后的提示词缺 ShowArtifact 指引")
	}
}
