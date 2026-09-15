package subagent

// domain_profile_test.go 领域注册表派发匹配测试（TODO #17 T24）：
// matchDomainProfile 三路命中（同名/别名/路径）+ 歧义并列不命中 + 未命中零改动；
// applyDomainProfileSeed 归一化 domain 并把种子段拼进 task。

import (
	"context"
	"strings"
	"testing"
)

func profileFixtures() []*DomainProfile {
	return []*DomainProfile{
		{
			Domain:      "前端工程师",
			DisplayName: "前端工程师",
			Aliases:     []string{"前端", "UI 开发"},
			Files:       []string{"web/src/views/session/index.vue", "web/src/api/session.ts"},
			Summary:     "负责会话页 UI 实现",
		},
		{
			Domain:      "后端工程师",
			DisplayName: "后端工程师",
			Files:       []string{"backend/internal/agent/service_react.go", "backend/internal/store/knowledge_store.go"},
		},
	}
}

func TestMatchDomainProfile(t *testing.T) {
	profiles := profileFixtures()
	cases := []struct {
		name      string
		domain    string
		task      string
		specFiles []string
		wantHit   bool
		wantVia   string
		wantDom   string
	}{
		{
			name:    "正名精确命中",
			domain:  "前端工程师",
			task:    "实现设置页",
			wantHit: true, wantVia: "name", wantDom: "前端工程师",
		},
		{
			name:    "别名命中归一化",
			domain:  "前端",
			task:    "调整对话框样式",
			wantHit: true, wantVia: "alias", wantDom: "前端工程师",
		},
		{
			name:   "路径重叠 ≥2 命中",
			domain: "界面组",
			task:   "重构 web/src/api/session.ts 并更新 web/src/views/session/index.vue 的交互",
			wantHit: true, wantVia: "paths", wantDom: "前端工程师",
		},
		{
			name:      "spec 文件清单与档案重叠命中",
			domain:    "接口层",
			task:      "按规范落地",
			specFiles: []string{"backend/internal/agent/service_react.go", "backend/internal/store/knowledge_store.go"},
			wantHit:   true, wantVia: "paths", wantDom: "后端工程师",
		},
		{
			name:   "单文件重叠不命中（防 README 类通用名误杀）",
			domain: "文档组",
			task:   "顺手改一下 web/src/api/session.ts",
			wantHit: false,
		},
		{
			name:   "两档案等量重叠歧义不命中",
			domain: "全栈",
			task:   "同时改 backend/internal/agent/service_react.go、backend/internal/store/knowledge_store.go、web/src/api/session.ts 与 web/src/views/session/index.vue",
			wantHit: false,
		},
		{
			name:   "无重叠自由命名不命中",
			domain: "测试组",
			task:   "编写验收用例",
			wantHit: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prof, via := matchDomainProfile(profiles, tc.domain, tc.task, tc.specFiles)
			if !tc.wantHit {
				if prof != nil {
					t.Fatalf("hit = %+v via=%q, want 未命中", prof, via)
				}
				return
			}
			if prof == nil {
				t.Fatalf("want 命中 via=%q dom=%q, got nil", tc.wantVia, tc.wantDom)
			}
			if via != tc.wantVia {
				t.Errorf("via = %q, want %q", via, tc.wantVia)
			}
			if prof.Domain != tc.wantDom {
				t.Errorf("domain = %q, want %q", prof.Domain, tc.wantDom)
			}
		})
	}
}

func TestApplyDomainProfileSeed(t *testing.T) {
	d := &Dispatcher{}
	d.SetDomainProfileHook(func(context.Context) []*DomainProfile { return profileFixtures() })
	var memQueried string
	d.SetDomainMemoryHook(func(_ context.Context, domain string, n int) []string {
		memQueried = domain
		return []string{"上轮已实现暗色主题切换", "更早：完成路由改造"}
	})

	// 别名命中：domain 归一化到正名，task 拼种子段。
	gotTask, gotDomain := d.applyDomainProfileSeed(context.Background(), "s1", "前端", "实现设置页")
	if gotDomain != "前端工程师" {
		t.Fatalf("domain = %q, want 归一化到 档案正名 前端工程师", gotDomain)
	}
	if !strings.Contains(gotTask, "【领域档案】") || !strings.Contains(gotTask, "历史摘要：负责会话页 UI 实现") {
		t.Fatalf("task 缺种子段: %q", gotTask)
	}
	if memQueried != "前端工程师" {
		t.Errorf("memory hook queried %q, want 归一化后的正名", memQueried)
	}
	if !strings.Contains(gotTask, "上轮已实现暗色主题切换") {
		t.Errorf("task 缺既有结论链: %q", gotTask)
	}

	// 未命中：原样返回。
	plainTask, plainDomain := d.applyDomainProfileSeed(context.Background(), "s1", "测试组", "编写验收用例")
	if plainDomain != "测试组" || plainTask != "编写验收用例" {
		t.Fatalf("未命中应原样返回, got domain=%q task=%q", plainDomain, plainTask)
	}

	// 回调未接线：零改动。
	bare := &Dispatcher{}
	t2, d2 := bare.applyDomainProfileSeed(context.Background(), "s1", "前端", "实现设置页")
	if d2 != "前端" || t2 != "实现设置页" {
		t.Fatalf("未接线回调应零改动, got domain=%q task=%q", d2, t2)
	}
}

// TestBumpDomainProfileSink 块记忆收尾旁路（TODO #17 T25）：taskDomain 非空才回调、
// 空串/未接线静默；Files/Summary 原样透传给 sink。
func TestBumpDomainProfileSink(t *testing.T) {
	var got DomainProfileUpdate
	calls := 0
	d := &Dispatcher{}
	d.SetDomainProfileSink(func(_ context.Context, up DomainProfileUpdate) {
		calls++
		got = up
	})

	d.bumpDomainProfile(context.Background(), "前端工程师", []string{"web/src/api/session.ts"}, "完成暗色主题")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if got.Domain != "前端工程师" || got.Summary != "完成暗色主题" || len(got.Files) != 1 {
		t.Fatalf("sink got %+v", got)
	}

	// taskDomain 空：不回调。
	d.bumpDomainProfile(context.Background(), "  ", []string{"a.go"}, "x")
	if calls != 1 {
		t.Fatalf("空 taskDomain 不应回调, calls = %d", calls)
	}

	// 未接线：不 panic。
	bare := &Dispatcher{}
	bare.bumpDomainProfile(context.Background(), "前端工程师", nil, "y")
}
