package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kratos/blades"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// recordingModelProvider 捕获首轮 LLM 请求的系统提示词(Instruction),随即返回 done 结束会话。
// 供终审修复 I-1 测试断言 MetaAgent 系统提示词的工作目录来源。
type recordingModelProvider struct {
	mu  sync.Mutex
	sys string
}

// Generate 实现 blades.ModelProvider 接口:记录 Instruction 文本,返回 done 让会话立即完成。
func (m *recordingModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	if m.sys == "" && req != nil && req.Instruction != nil {
		m.sys = systemText(req)
	}
	m.mu.Unlock()
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

// Name 返回 provider 名称,用于日志与调试。
func (m *recordingModelProvider) Name() string { return "recording" }

// systemPromptOf 创建会话并等其完成,返回 MetaAgent 首轮系统提示词。
func systemPromptOf(t *testing.T, rec *recordingModelProvider, s *ReactService, workDir string) string {
	t.Helper()
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g", WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.Get(context.Background(), sess.ID)
		if err != nil {
			t.Fatalf("Get error: %v", err)
		}
		if got.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.sys == "" {
		t.Fatal("provider 未被调用或系统提示词为空")
	}
	return rec.sys
}

// TestCreateSession_PerSessionWorkDir 验证每会话工作目录（S2）：
// CreateRequest.WorkDir 非空时会话级 TempDir 基于该目录；空时回落服务默认 workDir。
func TestCreateSession_PerSessionWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	// provider 不能传 nil：空角色配置仍会合成 meta 角色，runSession 会在
	// nil modelFactory.GetBladesProvider 上 panic（goroutine 内崩溃打爆测试二进制）。
	// mockReactModelProvider 无预设响应时首轮即返回 "done"，会话快速完成。
	s := newReactServiceForTest(&mockReactModelProvider{}, dirA)
	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g", WorkDir: dirB})
	if err != nil {
		t.Fatal(err)
	}
	if sess.WorkDir != dirB {
		t.Fatalf("WorkDir got %q want %q", sess.WorkDir, dirB)
	}
	wantTemp := filepath.Join(dirB, ".bma", "tmp", sess.ID)
	if sess.TempDir != wantTemp {
		t.Fatalf("TempDir got %q want %q", sess.TempDir, wantTemp)
	}
	// 空 WorkDir 回落默认
	sess2, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g2"})
	if err != nil {
		t.Fatal(err)
	}
	if sess2.WorkDir != "" {
		t.Fatalf("empty WorkDir should stay empty, got %q", sess2.WorkDir)
	}
	wantTemp2 := filepath.Join(dirA, ".bma", "tmp", sess2.ID)
	if sess2.TempDir != wantTemp2 {
		t.Fatalf("fallback TempDir got %q want %q", sess2.TempDir, wantTemp2)
	}
	s.Shutdown(context.Background())
}

// TestRunSession_SystemPromptPerSessionWorkDir 验证终审修复 I-1:自定义 workDir 会话的
// MetaAgent 系统提示词——工作目录行与【项目概览】(PROJECT.md)均取自会话目录,
// 与 createSession 在该目录 EnsureProjectDoc 生成的 PROJECT.md 对上;
// 未设 workDir 的会话回落进程默认目录。
func TestRunSession_SystemPromptPerSessionWorkDir(t *testing.T) {
	dirA := t.TempDir() // 进程默认目录
	dirB := t.TempDir() // 会话目录
	// 会话目录预写带哨兵的 PROJECT.md:createSession 的 EnsureProjectDoc 见已存在不覆盖。
	if err := os.MkdirAll(filepath.Join(dirB, ".bma"), 0o755); err != nil {
		t.Fatal(err)
	}
	const sentinel = "SENTINEL-SESSION-PROJECT-DOC"
	doc := project.ManagedBegin + "\n" + sentinel + "\n" + project.ManagedEnd + "\n"
	if err := os.WriteFile(filepath.Join(dirB, ".bma", "PROJECT.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	// 自定义 workDir 会话:工作目录行与 PROJECT.md 均取自 dirB。
	rec := &recordingModelProvider{}
	s := newReactServiceForTest(rec, dirA)
	sys := systemPromptOf(t, rec, s, dirB)
	if !strings.Contains(sys, "- 工作目录: "+dirB) {
		t.Fatalf("系统提示词工作目录行未取会话目录 %q:\n%s", dirB, sys)
	}
	if !strings.Contains(sys, sentinel) {
		t.Fatalf("系统提示词项目概览未取会话目录 PROJECT.md:\n%s", sys)
	}

	// 空 workDir 会话:回落进程默认目录 dirA。
	rec2 := &recordingModelProvider{}
	s2 := newReactServiceForTest(rec2, dirA)
	sys2 := systemPromptOf(t, rec2, s2, "")
	if !strings.Contains(sys2, "- 工作目录: "+dirA) {
		t.Fatalf("空 workDir 会话应回落进程目录 %q:\n%s", dirA, sys2)
	}
	s.Shutdown(context.Background())
	s2.Shutdown(context.Background())
}

// TestSetSessionWorkDir 验证"每会话目录可后续修改"（会话页本会话目录入口）：
// 驻留会话改内存 + 落库、空串回落进程默认、未知会话返回 ErrSessionNotFound（HTTP 404），
// 以及每次改动留一条系统事件（事后可回溯产物为何换位置）。
func TestSetSessionWorkDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	// 会话不跑循环：直接往 store 塞会话即可（改动只动内存与事件流）。
	s := newReactServiceForTest(nil, dirA)
	sess := s.store.createSession("g", "")
	defer s.Shutdown(context.Background())

	if got := s.store.sessions[sess.ID].currentWorkDir(); got != "" {
		t.Fatalf("初始 workDir 应为空, got %q", got)
	}

	// 1) 修改：内存即时生效（同会话后续 Get 返回新值）。
	if err := s.SetSessionWorkDir(context.Background(), sess.ID, dirB); err != nil {
		t.Fatalf("SetSessionWorkDir: %v", err)
	}
	if got := s.store.sessions[sess.ID].currentWorkDir(); got != dirB {
		t.Fatalf("改后 workDir got %q want %q", got, dirB)
	}
	pub, err := s.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pub.WorkDir != dirB {
		t.Fatalf("对外 DTO WorkDir got %q want %q", pub.WorkDir, dirB)
	}

	// 2) 审计留痕：改动写一条系统事件。
	events := s.store.sessions[sess.ID].Events
	found := false
	for _, ev := range events {
		if strings.Contains(ev.Message, "工作目录已改为") && strings.Contains(ev.Message, dirB) {
			found = true
		}
	}
	if !found {
		t.Fatalf("改动应留系统事件, events=%+v", events)
	}

	// 3) 空串 = 清除为进程默认目录。
	if err := s.SetSessionWorkDir(context.Background(), sess.ID, ""); err != nil {
		t.Fatalf("清除 workDir: %v", err)
	}
	if got := s.store.sessions[sess.ID].currentWorkDir(); got != "" {
		t.Fatalf("清除后 workDir 应为空, got %q", got)
	}

	// 4) 未知会话 → ErrSessionNotFound（HTTP 404，而不是 trust-mode 那样的 500）。
	if err := s.SetSessionWorkDir(context.Background(), "session-does-not-exist", dirB); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("未知会话应 ErrSessionNotFound, got %v", err)
	}
}

// multiInstructionProvider 记录每一次 LLM 请求的系统提示词（与 recordingModelProvider
// 只留首条不同）：用于验证"改目录后下一回合生效"。
type multiInstructionProvider struct {
	mu   sync.Mutex
	sys  []string
	text string
}

func (m *multiInstructionProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	if req != nil && req.Instruction != nil {
		m.sys = append(m.sys, systemText(req))
	}
	text := m.text
	m.mu.Unlock()
	if text == "" {
		text = "done"
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage(text)}, nil
}
func (m *multiInstructionProvider) Name() string { return "multi-instruction" }
func (m *multiInstructionProvider) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sys) == 0 {
		return ""
	}
	return m.sys[len(m.sys)-1]
}

// TestSetSessionWorkDir_TakesEffectNextTurn 验证"下一回合生效"这条承诺：
// 会话在 dirA 完成首轮后改到 dirB，下一条消息的回合系统提示词必须已用 dirB
// （目录在每回合开始时读取——这是 UI 提示"下回合生效"的依据）。
func TestSetSessionWorkDir_TakesEffectNextTurn(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	prov := &multiInstructionProvider{}
	s := newReactServiceForTest(prov, dirA)
	defer s.Shutdown(context.Background())

	sess, err := s.CreateSession(context.Background(), CreateRequest{Goal: "g1", WorkDir: dirA})
	if err != nil {
		t.Fatal(err)
	}
	waitCompleted(t, s, sess.ID)
	if first := prov.last(); !strings.Contains(first, "- 工作目录: "+dirA) {
		t.Fatalf("首轮应使用会话创建时的目录 %q:\n%s", dirA, first)
	}

	if err := s.SetSessionWorkDir(context.Background(), sess.ID, dirB); err != nil {
		t.Fatalf("SetSessionWorkDir: %v", err)
	}
	if err := s.Send(context.Background(), sess.ID, Message{Content: "继续"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitCompleted(t, s, sess.ID)
	if last := prov.last(); !strings.Contains(last, "- 工作目录: "+dirB) {
		t.Fatalf("改目录后下一回合应使用 %q:\n%s", dirB, last)
	}
}

// waitCompleted 轮询等待会话进入 completed（跑完一个回合）。
func waitCompleted(t *testing.T, s *ReactService, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := s.Get(context.Background(), id); err == nil && got.Status == string(enums.SessionStatusCompleted) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("会话 %s 未在期限内完成回合", id)
}

// TestControlWorkDirFailClosed 验证 Control 的 work_dir 参数 fail-closed：
// 参数缺失/类型不符必须报错，**绝不能**降级成空串——空串是合法的"清除为默认目录"
// 载荷，一旦静默降级，一次序列化误差就会悄悄清掉用户设的目录。
func TestControlWorkDirFailClosed(t *testing.T) {
	dirB := t.TempDir()
	s := newReactServiceForTest(nil, "")
	defer s.Shutdown(context.Background())
	sess := s.store.createSession("g", dirB)
	ctx := context.Background()

	for name, args := range map[string]map[string]any{
		"缺参数":  {},
		"类型不符": {"work_dir": 123},
	} {
		if err := s.Control(ctx, sess.ID, ControlCommand{Op: ControlOpWorkDir, Args: args}); err == nil {
			t.Fatalf("%s：应报错而非静默清除", name)
		}
		if got := s.store.sessions[sess.ID].currentWorkDir(); got != dirB {
			t.Fatalf("%s：目录被误改 got=%q want=%q", name, got, dirB)
		}
	}

	// 合法空串仍走"清除"语义（不是被误伤的那类）。
	if err := s.Control(ctx, sess.ID, ControlCommand{Op: ControlOpWorkDir, Args: map[string]any{"work_dir": ""}}); err != nil {
		t.Fatalf("显式空串应放行: %v", err)
	}
	if got := s.store.sessions[sess.ID].currentWorkDir(); got != "" {
		t.Fatalf("显式空串应清除目录, got %q", got)
	}
}

// TestHandleToolEvent_ArtifactsIntoDetailJSON 验证可视成果随工具事件落 detail_json：
// ShowArtifact 的 Result.Artifacts 经 registry.emitResult 进 ProgressEvent.Detail，
// 这里必须并入事件的 detail_json（前端对话栏读它渲染媒体卡片）——漏了的话
// "生成图/HTML 后展示"整条链路在浏览器侧就是空的。
func TestHandleToolEvent_ArtifactsIntoDetailJSON(t *testing.T) {
	s := newReactServiceForTest(nil, "")
	defer s.Shutdown(context.Background())
	sess := s.store.createSession("g", "")

	ctx := WithAgentID(context.Background(), sess.ID)
	detail := `{"output":"已把 主界面效果图 展示给用户","path":"/w/assets/img/hero.png",` +
		`"artifacts":[{"kind":"image","path":"assets/img/hero.png","title":"主界面效果图","mime":"image/png"}]}`
	s.handleToolEvent(ctx, tool.ProgressEvent{
		SessionID: sess.ID, Kind: "tool_result", Tool: "ShowArtifact",
		Message: "工具结果 ShowArtifact", Detail: detail,
	})

	events := s.store.sessions[sess.ID].Events
	if len(events) == 0 {
		t.Fatal("应记录一条工具结果事件")
	}
	ev := events[len(events)-1]
	if !strings.Contains(ev.DetailJSON, `"artifacts"`) || !strings.Contains(ev.DetailJSON, "assets/img/hero.png") {
		t.Fatalf("detail_json 应含 artifacts: %s", ev.DetailJSON)
	}
	if !strings.Contains(ev.DetailJSON, "agent_id") {
		t.Fatalf("原有 agent_id 归属字段不应丢失: %s", ev.DetailJSON)
	}
	if ev.ToolOutput != "已把 主界面效果图 展示给用户" {
		t.Fatalf("tool_output 应照旧落库: %q", ev.ToolOutput)
	}
}
