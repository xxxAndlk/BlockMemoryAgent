package bootstrap

// skill_gate_test.go 覆盖 TODO 25 阶段 B1/B2/B3 的写入质量治理：
// B1 prompt 收紧后结构不变；B2 评分拒绝/放行/失败放行三路径；
// B3 近重复 judge 合并 / judge 失败保守合并 / 判不重复与低相似度新建。
// 存储层用 fake（learnedSkillStoreOps 接口），judge/score 注入 fake 回调。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/store"
)

// ---- fake 存储 ----

type fakeEvolutionLog struct {
	kind, target, summary, session string
}

// fakeSkillStore 内存版 learnedSkillStoreOps：向量检索按真实 cosine 距离过滤。
type fakeSkillStore struct {
	items    map[string]*store.LearnedSkill
	logs     []fakeEvolutionLog
	embedVec []float32
}

func newFakeSkillStore() *fakeSkillStore {
	return &fakeSkillStore{items: map[string]*store.LearnedSkill{}, embedVec: []float32{1, 0, 0}}
}

func (f *fakeSkillStore) Get(_ context.Context, name string) (*store.LearnedSkill, error) {
	if r := f.items[name]; r != nil {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (f *fakeSkillStore) List(_ context.Context, enabledOnly bool) ([]*store.LearnedSkill, error) {
	var out []*store.LearnedSkill
	for _, r := range f.items {
		if !enabledOnly || r.Enabled {
			c := *r
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeSkillStore) SearchSkills(_ context.Context, emb []float32, limit int, maxDistance float64) ([]*store.LearnedSkill, error) {
	var hits []*store.LearnedSkill
	for _, r := range f.items {
		if !r.Enabled || len(r.Embedding) == 0 {
			continue
		}
		d := cosineDist(emb, r.Embedding)
		if d <= maxDistance {
			c := *r
			c.Score = 1 - d
			hits = append(hits, &c)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (f *fakeSkillStore) CountEnabled(_ context.Context) (int, error) {
	n := 0
	for _, r := range f.items {
		if r.Enabled {
			n++
		}
	}
	return n, nil
}

func (f *fakeSkillStore) SetEnabled(_ context.Context, name string, enabled bool) error {
	if r := f.items[name]; r != nil {
		r.Enabled = enabled
	}
	return nil
}

func (f *fakeSkillStore) Upsert(_ context.Context, rec *store.LearnedSkill) error {
	c := *rec
	f.items[rec.Name] = &c
	return nil
}

func (f *fakeSkillStore) AppendEvolutionLog(_ context.Context, kind, target, summary, session string) error {
	f.logs = append(f.logs, fakeEvolutionLog{kind, target, summary, session})
	return nil
}

func (f *fakeSkillStore) Embed(_ context.Context, _ string) ([]float32, error) {
	return f.embedVec, nil
}

// ListColdEnabled 内存版冷淘汰候选：与 PG（store.ListColdEnabled）同规则——
// tier1 use_count==0 且创建早于 now()-coldDays；tier2 use_count<=idleMaxUse 且最近使用
//（从未使用按创建时间）早于 now()-idleDays；按 use_count、最近使用、创建升序取前 limit 个。
func (f *fakeSkillStore) ListColdEnabled(_ context.Context, limit, coldDays, idleDays, idleMaxUse int) ([]*store.LearnedSkill, error) {
	coldBefore := time.Now().AddDate(0, 0, -coldDays)
	idleBefore := time.Now().AddDate(0, 0, -idleDays)
	var out []*store.LearnedSkill
	for _, r := range f.items {
		if !r.Enabled {
			continue
		}
		lastUsed := r.CreatedAt
		if r.LastUsedAt != nil {
			lastUsed = *r.LastUsedAt
		}
		cold := r.UseCount == 0 && r.CreatedAt.Before(coldBefore)
		idle := r.UseCount <= idleMaxUse && lastUsed.Before(idleBefore)
		if !cold && !idle {
			continue
		}
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].CreatedAt, out[j].CreatedAt
		if out[i].LastUsedAt != nil {
			li = *out[i].LastUsedAt
		}
		if out[j].LastUsedAt != nil {
			lj = *out[j].LastUsedAt
		}
		if out[i].UseCount != out[j].UseCount {
			return out[i].UseCount < out[j].UseCount
		}
		if !li.Equal(lj) {
			return li.Before(lj)
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func cosineDist(a, b []float32) float64 {
	var dot, na, nb float64
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/math.Sqrt(na*nb)
}

// ---- 测试辅助 ----

// seedExistingSkill 造一个已有技能：文件（frontmatter + 步骤/坑点正文）+ 存储记录。
func seedExistingSkill(t *testing.T, st *fakeSkillStore, dir, name, title, when string, useCount int, emb []float32) *store.LearnedSkill {
	t.Helper()
	content := fmt.Sprintf("---\nname: %s\ntitle: %s\nwhen_to_use: %s\noutcome: success\n---\n\n## 步骤\n1. 写测试\n\n## 坑点\n- 别硬编码路径\n", name, title, when)
	path := filepath.Join(dir, name+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed skill file: %v", err)
	}
	rec := &store.LearnedSkill{
		Name: name, Title: title, WhenToUse: when,
		ContentPath: path, Embedding: emb, Enabled: true, UseCount: useCount,
	}
	st.items[name] = rec
	return rec
}

// nearDupSkill 与新造既有技能同域的新技能（异名）。
// 夹具步骤需过确定性结构门（≥3 条带锚点 + verify 非空）——门是薄技能治理新增的硬兜底。
func nearDupSkill() agent.EvolvedSkill {
	return agent.EvolvedSkill{
		Name: "cli-tool-acceptance-verification", Title: "CLI 工具验收验证",
		WhenToUse: "验收命令行工具时",
		Steps: []string{
			"运行 pytest tests/ -q --tb=short 执行测试套件，全部通过即功能正常",
			"运行 flake8 src/ --max-line-length=120 做静态检查，0 告警即风格合规",
			"运行 python -m build 打包，dist/ 目录生成 .whl 即产物完整",
		},
		Pitfalls: []string{"- 别跳过 lint"},
		Verify:   "echo $? 退出码为 0",
	}
}

// newTestSink 组一个注入式 sink（score=nil 跳过 B2 门禁，专注被测路径）。
func newTestSink(st *fakeSkillStore, dir string) *learnedSkillSink {
	return &learnedSkillSink{skills: st, dir: dir, maxCount: 0}
}

func lastLog(st *fakeSkillStore) fakeEvolutionLog {
	return st.logs[len(st.logs)-1]
}

// ---- B1：prompt 收紧后结构不变 ----

// TestBuildEvolvePromptTightened B1 门槛收紧（最多 1 个/三条件/动词开头/不带行首序号），
// 且 JSON 字段结构与 parseEvolveOutput 保持兼容。
func TestBuildEvolvePromptTightened(t *testing.T) {
	p := buildEvolvePrompt(agent.EvolveInput{
		Goal: "goal", Summary: "sum", Outcome: "success", EventsDigest: "ev", UserMessages: "um",
	})
	for _, want := range []string{"0 或 1 个", "跨项目通用", "非显而易见", "动词开头", "不要自带行首序号", "检查/注意/确保/考虑"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// JSON 结构兼容：六个字段键名原样保留。
	for _, key := range []string{`"user_prefs"`, `"project_lessons"`, `"skills"`, `"name"`, `"title"`, `"when_to_use"`, `"steps"`, `"pitfalls"`, `"verify"`} {
		if !strings.Contains(p, key) {
			t.Errorf("prompt lost JSON key %s", key)
		}
	}
}

// ---- B2：质量门禁三路径 ----

func TestPersistQualityGateRejected(t *testing.T) {
	st := newFakeSkillStore()
	s := newTestSink(st, t.TempDir())
	s.score = func(context.Context, agent.EvolvedSkill) (skillScoreResult, error) {
		return skillScoreResult{Score: 2, Reason: "空泛不可执行"}, nil
	}
	sk := agent.EvolvedSkill{Name: "vague-skill", Title: "空泛技能", WhenToUse: "任何时候",
		Steps: []string{"运行 pytest -q 验证行为", "运行 mypy src/ --strict 检查类型", "运行 ruff check . 扫一遍 lint"},
		Verify: "三个命令退出码均为 0"}
	if err := s.persist(context.Background(), "sess-1", []agent.EvolvedSkill{sk}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(st.items) != 0 {
		t.Fatalf("rejected skill must not persist, items=%v", st.items)
	}
	if len(st.logs) != 1 || st.logs[0].kind != "skill_rejected" {
		t.Fatalf("expect one skill_rejected log, got %+v", st.logs)
	}
	if st.logs[0].target != "vague-skill" || !strings.Contains(st.logs[0].summary, "空泛技能") {
		t.Fatalf("rejected log content = %+v", st.logs[0])
	}
}

func TestPersistQualityGatePass(t *testing.T) {
	st := newFakeSkillStore()
	s := newTestSink(st, t.TempDir())
	s.score = func(context.Context, agent.EvolvedSkill) (skillScoreResult, error) {
		return skillScoreResult{Score: 4, Reason: "具体可复用"}, nil
	}
	sk := agent.EvolvedSkill{Name: "solid-skill", Title: "具体技能", WhenToUse: "批量重命名时",
		Steps: []string{"运行 ls *.txt 确认待处理文件清单", "运行 rename 's/foo/bar/' *.txt 批量替换前缀", "运行 ls *.txt 复核新文件名符合预期"},
		Verify: "ls 输出全部以 bar- 开头"}
	if err := s.persist(context.Background(), "sess-1", []agent.EvolvedSkill{sk}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(st.items) != 1 || st.items["solid-skill"] == nil {
		t.Fatalf("score>=3 skill must persist, items=%v", st.items)
	}
	if lastLog(st).kind != "skill_create" {
		t.Fatalf("expect skill_create log, got %+v", lastLog(st))
	}
}

func TestPersistQualityGateScoreFailOpen(t *testing.T) {
	st := newFakeSkillStore()
	s := newTestSink(st, t.TempDir())
	s.score = func(context.Context, agent.EvolvedSkill) (skillScoreResult, error) {
		return skillScoreResult{}, errors.New("llm unreachable")
	}
	sk := agent.EvolvedSkill{Name: "degrade-skill", Title: "降级技能", WhenToUse: "x 时",
		Steps: []string{"运行 curl -s localhost:8080/health 探测服务", "运行 docker ps 确认容器在运行", "运行 tail -50 app.log 查最新报错"},
		Verify: "health 返回 ok"}
	if err := s.persist(context.Background(), "sess-1", []agent.EvolvedSkill{sk}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if st.items["degrade-skill"] == nil {
		t.Fatal("score failure must degrade to pass")
	}
	for _, l := range st.logs {
		if l.kind == "skill_rejected" {
			t.Fatalf("fail-open path must not write skill_rejected, got %+v", st.logs)
		}
	}
}

// ---- B3：近重复预检三路径 ----

func TestPersistNearDupJudgeMerge(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	seedExistingSkill(t, st, dir, "python-cli-tool", "Python CLI 工具开发", "开发命令行工具时", 7, []float32{1, 0, 0})
	s := newTestSink(st, dir)
	judgeCalls := 0
	s.judge = func(_ context.Context, _, _ string, cand *store.LearnedSkill) (duplicateJudgeResult, error) {
		judgeCalls++
		if cand.Name != "python-cli-tool" {
			t.Errorf("judge candidate = %s", cand.Name)
		}
		return duplicateJudgeResult{Duplicate: true, MergeInto: "python-cli-tool", Reason: "同一工艺"}, nil
	}
	sk := nearDupSkill()
	if err := s.persist(context.Background(), "sess-2", []agent.EvolvedSkill{sk}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge called %d times", judgeCalls)
	}
	// 合并路径：不新建异名技能，保留者与 use_count 不动。
	if len(st.items) != 1 || st.items["cli-tool-acceptance-verification"] != nil {
		t.Fatalf("near-dup merge must not create new skill, items=%v", st.items)
	}
	if got := st.items["python-cli-tool"].UseCount; got != 7 {
		t.Fatalf("use_count = %d, want 7", got)
	}
	if lastLog(st).kind != "skill_update" || lastLog(st).target != "python-cli-tool" {
		t.Fatalf("expect skill_update log, got %+v", lastLog(st))
	}
	// 正文 union 重渲染：新步骤带序号剥一层后追加，既有条目不重复。
	data, err := os.ReadFile(filepath.Join(dir, "python-cli-tool.md"))
	if err != nil {
		t.Fatalf("read merged file: %v", err)
	}
	body := string(data)
	for _, want := range []string{"1. 写测试\n2. 运行 pytest tests/ -q --tb=short", "- 别硬编码路径\n- 别跳过 lint", "echo $? 退出码为 0",
		"开发命令行工具时；验收命令行工具时"} {
		if !strings.Contains(body, want) {
			t.Errorf("merged body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "1. 1.") || strings.Contains(body, "- - ") {
		t.Errorf("merged body has double marker:\n%s", body)
	}
}

func TestPersistNearDupJudgeFailConservativeMerge(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	seedExistingSkill(t, st, dir, "python-cli-tool", "Python CLI 工具开发", "开发命令行工具时", 3, []float32{1, 0, 0})
	s := newTestSink(st, dir)
	s.judge = func(context.Context, string, string, *store.LearnedSkill) (duplicateJudgeResult, error) {
		return duplicateJudgeResult{}, errors.New("judge timeout")
	}
	if err := s.persist(context.Background(), "sess-3", []agent.EvolvedSkill{nearDupSkill()}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	// 保守合并进相似度最高的候选：不新建，use_count 保留。
	if len(st.items) != 1 || st.items["cli-tool-acceptance-verification"] != nil {
		t.Fatalf("conservative merge must not create new skill, items=%v", st.items)
	}
	if got := st.items["python-cli-tool"].UseCount; got != 3 {
		t.Fatalf("use_count = %d, want 3", got)
	}
	if lastLog(st).kind != "skill_update" {
		t.Fatalf("expect skill_update log, got %+v", lastLog(st))
	}
}

func TestPersistNearDupJudgeNotDuplicateCreates(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	seedExistingSkill(t, st, dir, "python-cli-tool", "Python CLI 工具开发", "开发命令行工具时", 0, []float32{1, 0, 0})
	s := newTestSink(st, dir)
	judgeCalls := 0
	s.judge = func(context.Context, string, string, *store.LearnedSkill) (duplicateJudgeResult, error) {
		judgeCalls++
		return duplicateJudgeResult{Duplicate: false, Reason: "工艺不同"}, nil
	}
	if err := s.persist(context.Background(), "sess-4", []agent.EvolvedSkill{nearDupSkill()}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge called %d times", judgeCalls)
	}
	// 判"不重复"→ 新建（且跳过 ≥0.75 覆盖更新，尊重 judge 结论）。
	if st.items["cli-tool-acceptance-verification"] == nil {
		t.Fatal("judge not-duplicate must create new skill")
	}
	if got := st.items["python-cli-tool"].Title; got != "Python CLI 工具开发" {
		t.Fatalf("existing skill title overwritten: %q", got)
	}
	if lastLog(st).kind != "skill_create" || lastLog(st).target != "cli-tool-acceptance-verification" {
		t.Fatalf("expect skill_create log, got %+v", lastLog(st))
	}
}

func TestPersistLowSimilarityCreatesWithoutJudge(t *testing.T) {
	st := newFakeSkillStore()
	dir := t.TempDir()
	// 既有技能相似度 0.5（距离 0.5）：落在近重复阈值（0.1）与同名更新阈值（0.25）之外。
	seedExistingSkill(t, st, dir, "unrelated-skill", "无关技能", "画画时", 0, []float32{0.5, 0.8660254, 0})
	s := newTestSink(st, dir)
	judgeCalls := 0
	s.judge = func(context.Context, string, string, *store.LearnedSkill) (duplicateJudgeResult, error) {
		judgeCalls++
		return duplicateJudgeResult{}, nil
	}
	if err := s.persist(context.Background(), "sess-5", []agent.EvolvedSkill{nearDupSkill()}, "success"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if judgeCalls != 0 {
		t.Fatalf("judge must not be called for low similarity, called %d", judgeCalls)
	}
	if st.items["cli-tool-acceptance-verification"] == nil {
		t.Fatal("low similarity must create new skill")
	}
	if lastLog(st).kind != "skill_create" {
		t.Fatalf("expect skill_create log, got %+v", lastLog(st))
	}
}

// ---- 薄技能确定性结构门 ----

func TestThinSkillReason(t *testing.T) {
	// 合格技能：≥3 条带锚点步骤 + verify 非空 → 通过。
	solid := agent.EvolvedSkill{
		Steps: []string{
			"运行 curl -s localhost:7456/api/health 探测服务，返回 ok 即正常",
			"运行 docker logs bma-open-design 2>&1 | tail -50 查看报错",
			"编辑 config/config.yaml 把 video_provider 改为 volcengine 后重启",
		},
		Verify: "curl 返回 ok",
	}
	if reason := thinSkillReason(solid); reason != "" {
		t.Fatalf("solid skill must pass, got %q", reason)
	}
	// 步骤不足 3 条。
	thin := solid
	thin.Steps = thin.Steps[:2]
	if reason := thinSkillReason(thin); reason == "" {
		t.Fatal("expect rejection for <3 steps")
	}
	// 空泛步骤（无锚点）：全是"注意/确保"式空话。
	vague := agent.EvolvedSkill{
		Steps:  []string{"注意检查配置", "确保服务正常", "记得验证结果"},
		Verify: "观察现象",
	}
	if reason := thinSkillReason(vague); reason == "" || !strings.Contains(reason, "锚点") {
		t.Fatalf("expect anchor rejection, got %q", reason)
	}
	// 缺 verify。
	noVerify := solid
	noVerify.Verify = "  "
	if reason := thinSkillReason(noVerify); reason == "" || !strings.Contains(reason, "验证") {
		t.Fatalf("expect verify rejection, got %q", reason)
	}
	// 模型自带行首序号/符号要剥掉后再数（"1. xxx" 不应当成空步骤）。
	numbered := agent.EvolvedSkill{
		Steps:  []string{"1. 运行 pytest -q 跑测试", "- 运行 mypy src/ 查类型", "• 运行 ruff check . 扫 lint"},
		Verify: "退出码 0",
	}
	if reason := thinSkillReason(numbered); reason != "" {
		t.Fatalf("leading markers must be stripped before gate, got %q", reason)
	}
}

// ---- 解析辅助单测 ----

func TestParseSkillScore(t *testing.T) {
	r, err := parseSkillScore("```json\n{\"score\": 2, \"reason\": \"空泛\"}\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Score != 2 || r.Reason != "空泛" {
		t.Fatalf("score = %+v", r)
	}
	// 越界钳制。
	if r, _ := parseSkillScore(`{"score": 9, "reason": "x"}`); r.Score != 5 {
		t.Fatalf("upper clamp = %d", r.Score)
	}
	if r, _ := parseSkillScore(`{"score": 0, "reason": "x"}`); r.Score != 1 {
		t.Fatalf("lower clamp = %d", r.Score)
	}
	if _, err := parseSkillScore("没有对象"); err == nil {
		t.Fatal("expect error for non-JSON")
	}
}

func TestParseDuplicateJudge(t *testing.T) {
	r, err := parseDuplicateJudge(`{"duplicate": true, "merge_into": "python-cli-tool", "reason": "同工艺"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !r.Duplicate || r.MergeInto != "python-cli-tool" || r.Reason != "同工艺" {
		t.Fatalf("judge = %+v", r)
	}
	if _, err := parseDuplicateJudge("noise"); err == nil {
		t.Fatal("expect error for non-JSON")
	}
}

func TestParseSkillSections(t *testing.T) {
	body := "## 步骤\n1. 写测试\n2. 跑 pytest\n\n## 坑点\n- 别硬编码\n- • 符号坑\n\n## 验证\n退出码应为 0\n"
	steps, pitfalls, verify := parseSkillSections(body)
	if len(steps) != 2 || steps[0] != "写测试" || steps[1] != "跑 pytest" {
		t.Fatalf("steps = %v", steps)
	}
	if len(pitfalls) != 2 || pitfalls[0] != "别硬编码" || pitfalls[1] != "• 符号坑" {
		t.Fatalf("pitfalls = %v", pitfalls)
	}
	if verify != "退出码应为 0" {
		t.Fatalf("verify = %q", verify)
	}
}

func TestAppendUnique(t *testing.T) {
	got := appendUnique([]string{"a"}, "a", " b ", "", "c")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("appendUnique = %v", got)
	}
}

func TestScoreAndJudgeNotWired(t *testing.T) {
	if _, err := scoreEvolvedSkill(context.Background(), nil, agent.EvolvedSkill{}); err == nil {
		t.Fatal("expect error when scorer not wired")
	}
	if _, err := judgeDuplicateMerge(context.Background(), nil, "t", "w", &store.LearnedSkill{Name: "x"}); err == nil {
		t.Fatal("expect error when judge not wired")
	}
}
