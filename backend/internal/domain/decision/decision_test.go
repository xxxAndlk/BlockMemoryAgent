package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProvider 按预置 Response/错误应答，记录收到的 Request。
type fakeProvider struct {
	resp Response
	err  error
	got  Request
}

func (f *fakeProvider) Decide(_ context.Context, req Request) (Response, error) {
	f.got = req
	return f.resp, f.err
}

// fakeRecorder 捕获影子行。
type fakeRecorder struct {
	rows []ShadowEvent
}

func (f *fakeRecorder) RecordShadow(_ context.Context, ev ShadowEvent) error {
	f.rows = append(f.rows, ev)
	return nil
}

func TestLLMProviderParsesStrictJSON(t *testing.T) {
	caller := CallerFunc(func(_ context.Context, _ string) (string, error) {
		return "```json\n{\"answers\":[{\"key\":\"k1\",\"value\":\"yes\",\"confidence\":0.9}]}\n```", nil
	})
	p := NewLLMProvider(caller)
	req := Request{State: "s", Questions: []Question{{
		Key: "k1", Point: PointNeedClarify, Primitive: PrimitiveNoul, Prompt: "需澄清?",
	}}}
	resp, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	a, ok := resp.AnswerOf("k1")
	if !ok || a.Value != "yes" || a.Confidence != 0.9 {
		t.Fatalf("unexpected answer: %+v ok=%v", a, ok)
	}
}

func TestLLMProviderDropsOutOfSetChoice(t *testing.T) {
	// 结构性防幻觉：Choice 答案必须命中候选闭集，集外值整条丢弃。
	caller := CallerFunc(func(_ context.Context, _ string) (string, error) {
		return `{"answers":[
			{"key":"k1","value":"hallucinated","confidence":0.99},
			{"key":"k2","value":"single","confidence":0.8}
		]}`, nil
	})
	p := NewLLMProvider(caller)
	req := Request{State: "s", Questions: []Question{
		{Key: "k1", Point: PointIntentKind, Primitive: PrimitiveChoice, Prompt: "q1", Candidates: []string{"quick", "single", "multi"}},
		{Key: "k2", Point: PointIntentKind, Primitive: PrimitiveChoice, Prompt: "q2", Candidates: []string{"quick", "single", "multi"}},
	}}
	resp, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, ok := resp.AnswerOf("k1"); ok {
		t.Fatal("out-of-set choice must be dropped")
	}
	if a, ok := resp.AnswerOf("k2"); !ok || a.Value != "single" {
		t.Fatalf("valid choice lost: %+v", a)
	}
}

func TestLLMProviderCallerErrorPropagates(t *testing.T) {
	caller := CallerFunc(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("boom")
	})
	p := NewLLMProvider(caller)
	req := Request{Questions: []Question{{Key: "k1", Point: PointNeedClarify, Primitive: PrimitiveNoul, Prompt: "q"}}}
	if _, err := p.Decide(context.Background(), req); err == nil {
		t.Fatal("caller error must propagate (fail-open at call site)")
	}
}

func TestAnswerCompare(t *testing.T) {
	cases := []struct {
		name   string
		ans    Answer
		actual string
		want   string
	}{
		{"空 actual=pending", Answer{Primitive: PrimitiveNoul, Value: "yes"}, "", "pending"},
		{"noul match", Answer{Primitive: PrimitiveNoul, Value: "yes"}, "yes", "match"},
		{"noul mismatch", Answer{Primitive: PrimitiveNoul, Value: "yes"}, "no", "mismatch"},
		{"actual 带附注取首段", Answer{Primitive: PrimitiveChoice, Value: "retry"}, "retry rank=1", "match"},
		{"score vs yes", Answer{Primitive: PrimitiveScore, Score: 0.8}, "yes", "match"},
		{"score vs no", Answer{Primitive: PrimitiveScore, Score: 0.2}, "no", "match"},
		{"score 数值带宽内", Answer{Primitive: PrimitiveScore, Score: 0.5}, "score=0.55", "match"},
		{"score 数值带宽外", Answer{Primitive: PrimitiveScore, Score: 0.9}, "score=0.1", "mismatch"},
	}
	for _, c := range cases {
		if got := AnswerCompare(c.ans, c.actual); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestLayerShadowOnlyZeroBehavior(t *testing.T) {
	rec := &fakeRecorder{}
	prov := &fakeProvider{resp: Response{Answers: []Answer{{Key: "disposition", Point: PointFailureDisp, Primitive: PrimitiveChoice, Value: "retry", Confidence: 0.95}}}}
	l := NewLayer(prov, rec, Options{ShadowEnabled: true, Timeout: time.Second})

	if l.ShouldEnforce(PointFailureDisp, 0.95) {
		t.Fatal("shadow mode must never enforce")
	}
	ans := l.Ask(context.Background(), "state", FailureDispositionQuestion("fail"))
	if ans == nil {
		t.Fatal("shadow mode should still ask (for observation)")
	}
	l.Observe(context.Background(), "agent-1", "retry", *ans)
	if len(rec.rows) != 1 {
		t.Fatalf("want 1 shadow row, got %d", len(rec.rows))
	}
	row := rec.rows[0]
	if row.Point != PointFailureDisp || row.Compare != "match" || row.Actual != "retry" {
		t.Fatalf("bad shadow row: %+v", row)
	}
	if !strings.Contains(row.AnswerJSON, `"confidence":0.95`) {
		t.Fatalf("answer json missing confidence: %s", row.AnswerJSON)
	}
}

func TestLayerEnforceGate(t *testing.T) {
	rec := &fakeRecorder{}
	prov := &fakeProvider{resp: Response{Answers: []Answer{{Key: "k1", Point: PointDispatchGap, Primitive: PrimitiveChoice, Value: GapNeedsVision, Confidence: 0.9}}}}
	l := NewLayer(prov, rec, Options{
		ShadowEnabled: true,
		Timeout:       time.Second,
		Points:        map[string]PointPolicy{PointDispatchGap: {Mode: ModeEnforce, MinConfidence: 0.8}},
	})
	if !l.ShouldEnforce(PointDispatchGap, 0.9) {
		t.Fatal("enforce + confident must apply")
	}
	if l.ShouldEnforce(PointDispatchGap, 0.7) {
		t.Fatal("below threshold must fall back to status quo")
	}
	// 未配置点恒影子。
	if l.ShouldEnforce(PointGearHint, 0.99) {
		t.Fatal("unconfigured point must stay shadow")
	}
}

func TestLayerFailOpenOnProviderError(t *testing.T) {
	prov := &fakeProvider{err: errors.New("provider down")}
	l := NewLayer(prov, &fakeRecorder{}, Options{ShadowEnabled: true, Timeout: time.Second})
	if ans := l.Ask(context.Background(), "s", FailureDispositionQuestion("f")); ans != nil {
		t.Fatal("provider failure must yield nil answer (fail-open)")
	}
}

func TestLayerShadowDisabledSkipsInactivePoints(t *testing.T) {
	rec := &fakeRecorder{}
	prov := &fakeProvider{}
	l := NewLayer(prov, rec, Options{ShadowEnabled: false, Timeout: time.Second})
	req := Request{State: "s", Questions: []Question{{
		Key: "k1", Point: PointGearHint, Primitive: PrimitiveChoice, Prompt: "q",
		Candidates: []string{GearFast, GearDaily, GearCluster},
	}}}
	// 影子关 + 非 enforce 点：零 provider 往返、零落行。
	if _, err := l.Decide(context.Background(), req); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if prov.got.Questions != nil {
		t.Fatal("inactive point must not reach provider")
	}
	l.Observe(context.Background(), "a1", GearDaily, Answer{Key: "k1", Point: PointGearHint, Primitive: PrimitiveChoice, Value: GearDaily, Confidence: 1})
	if len(rec.rows) != 0 {
		t.Fatal("shadow disabled must not record shadow-only points")
	}
}

func TestHTTPProviderFallbackToLLM(t *testing.T) {
	fallback := NewLLMProvider(CallerFunc(func(_ context.Context, _ string) (string, error) {
		return `{"answers":[{"key":"k1","value":"no","confidence":0.7}]}`, nil
	}))
	// 空 URL 状态下手动摆一个坏 provider：post 阶段失败 → 回退 llm。
	p := NewHTTPProvider("http://127.0.0.1:1/dead", "", 100*time.Millisecond, fallback)
	req := Request{State: "s", Questions: []Question{{Key: "k1", Point: PointExtractWorth, Primitive: PrimitiveNoul, Prompt: "q"}}}
	resp, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("fallback should recover: %v", err)
	}
	if a, ok := resp.AnswerOf("k1"); !ok || a.Value != "no" {
		t.Fatalf("fallback answer lost: %+v", a)
	}
}

func TestFilterActiveRespectsPolicy(t *testing.T) {
	opts := Options{
		ShadowEnabled: false,
		Points: map[string]PointPolicy{
			PointExtractWorth: {Mode: ModeEnforce, MinConfidence: 0.6},
		},
	}
	req := Request{Questions: []Question{
		{Key: "a", Point: PointExtractWorth, Primitive: PrimitiveNoul, Prompt: "q"},
		{Key: "b", Point: PointGearHint, Primitive: PrimitiveChoice, Prompt: "q", Candidates: []string{GearFast}},
		{Key: "", Point: PointGearHint, Primitive: PrimitiveChoice, Prompt: "bad key"},
	}}
	active := req.filterActive(opts)
	if len(active.Questions) != 1 || active.Questions[0].Key != "a" {
		t.Fatalf("filter wrong: %+v", active.Questions)
	}
}
