package tui

// live_harness_test.go 是一个手动触发的端到端验证 harness：
// 用真实后端（bootstrap.Build + 本地 HTTP）驱动 TUI Model，模拟用户输入一个问题，
// 按时间顺序把 View() 渲染结果快照到 test/tmp/harness_*.txt，用于排查
// "首条问题不可见 / 等待期无输出 / 无流式渲染" 等真实行为问题。
//
// 默认跳过；设置 BMA_LIVE_HARNESS=1 才会运行：
//
//	BMA_LIVE_HARNESS=1 go test ./internal/tui -run TestLiveHarness -v -timeout 300s
//
// 注意：会真实调用大模型（消耗 token），并在数据库中创建真实会话记录。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/bootstrap"
)

// liveHarnessRoot 是项目根目录的绝对路径（配置/.env 相对它解析）。
const liveHarnessRoot = `E:\MyProject\BlockMemoryAgent`

// liveHarnessSnapDir 返回快照输出目录。
func liveHarnessSnapDir() string { return liveHarnessRoot + `\test\tmp` }

// stripANSI 已在 helpers 中实现，这里直接复用包内函数。

// TestLiveHarness 端到端驱动：WindowSize -> 输入问题 -> Enter -> 周期性 tick，
// 每 2 秒快照一次 View，直到会话完成或超时。
func TestLiveHarness(t *testing.T) {
	if os.Getenv("BMA_LIVE_HARNESS") == "" {
		t.Skip("设置 BMA_LIVE_HARNESS=1 才运行该端到端 harness")
	}
	// 与 cmd/tui 保持一致：东亚字符按窄字符计算宽度，保证 harness 复现真实布局。
	runewidth.DefaultCondition.EastAsianWidth = false

	ctx := context.Background()
	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath: liveHarnessRoot + `\config\config.yaml`,
		RolePath:   liveHarnessRoot + `\config\roles.yaml`,
		EnvPath:    liveHarnessRoot + `\.env`,
		SoulPath:   liveHarnessRoot + `\config\soul.md`,
		SkillPath:  liveHarnessRoot + `\config\skills.yaml`,
	})
	if err != nil {
		t.Fatalf("bootstrap.Build 失败: %v", err)
	}
	defer app.Close()

	// 复刻 cmd/tui 的本地 HTTP 路由（输入栏经 HTTP 访问后端）。
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			app.Server.HandleListSessions(w, r)
		case http.MethodPost:
			app.Server.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sessions/{id}/stream", app.Server.HandleSessionStream)
	mux.HandleFunc("/api/sessions/{id}/message", app.Server.HandleSessionMessage)
	mux.HandleFunc("/api/sessions/{id}/clarify", app.Server.HandleSessionClarify)
	mux.HandleFunc("/api/sessions/{id}/interrupt", app.Server.HandleSessionInterrupt)
	mux.HandleFunc("/api/sessions/{id}/enqueue", app.Server.HandleSessionEnqueue)
	mux.HandleFunc("/api/sessions/{id}/cancel", app.Server.HandleSessionCancel)
	mux.HandleFunc("/api/sessions/{id}/board", app.Server.HandleSessionBoard)
	mux.HandleFunc("/api/sessions/{id}/agents", app.Server.HandleSessionAgents)
	mux.HandleFunc("/api/sessions/{id}/metrics", app.Server.HandleSessionMetrics)
	mux.HandleFunc("/api/sessions/{id}/watchdog", app.Server.HandleSessionWatchdog)
	mux.HandleFunc("/api/sessions/{id}/topic", app.Server.HandleSessionTopic)
	mux.HandleFunc("/api/sessions/{id}", app.Server.HandleGetSession)
	mux.Handle("/api/dag", app.DAGHandler)
	mux.Handle("/api/dag/", app.DAGHandler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 失败: %v", err)
	}
	defer ln.Close()
	go func() { _ = http.Serve(ln, mux) }()
	httpAddr := "http://" + ln.Addr().String()

	modelName := app.RoleConfig.MetaAgent.ModelConfig.Model
	m := NewModel(app.Agent, app.DAGHandler, httpAddr, modelName)

	start := time.Now()
	snapIdx := 0
	snapshot := func(tag string) {
		snapIdx++
		var sb strings.Builder
		fmt.Fprintf(&sb, "=== snapshot %02d [%s] elapsed=%.1fs ===\n", snapIdx, tag, time.Since(start).Seconds())
		if s := m.selectedSession(); s != nil {
			fmt.Fprintf(&sb, "session=%s status=%s events=%d messages=%d\n", s.ID, s.Status, len(s.Events), len(s.Messages))
		} else {
			fmt.Fprintf(&sb, "session=<none> cursor=%d sessions=%d\n", m.sessionsCursor, len(m.sessions))
		}
		sb.WriteString(stripANSI(m.View()))
		path := fmt.Sprintf(`%s\harness_%02d_%s.txt`, liveHarnessSnapDir(), snapIdx, tag)
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			t.Logf("写快照失败: %v", err)
		}
	}

	// 模拟 bubbletea 初始化：窗口尺寸可用 BMA_HARNESS_W/H 覆盖，默认 140x40。
	winW, winH := 140, 40
	if v := os.Getenv("BMA_HARNESS_W"); v != "" {
		fmt.Sscanf(v, "%d", &winW)
	}
	if v := os.Getenv("BMA_HARNESS_H"); v != "" {
		fmt.Sscanf(v, "%d", &winH)
	}
	nm, _ := m.Update(tea.WindowSizeMsg{Width: winW, Height: winH})
	m = modelPtr(nm)
	snapshot("welcome")

	// 输入问题并回车（一次性 KeyRunes + KeyEnter），问题可用 BMA_HARNESS_Q 覆盖。
	question := os.Getenv("BMA_HARNESS_Q")
	if question == "" {
		question = "hello, please introduce yourself briefly"
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(question)})
	m = modelPtr(nm)
	// 模拟真人按键间隔，避免 Enter 被 80ms 粘贴兜底逻辑误判为粘贴换行。
	m.inputBar.lastKeyTime = time.Now().Add(-200 * time.Millisecond)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = modelPtr(nm)
	snapshot("after_enter")

	// 周期性驱动 tick（真实程序 100ms 一次，这里 150ms 一次），每 ~2s 快照。
	deadline := start.Add(150 * time.Second)
	lastSnap := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		nm, _ = m.Update(tickMsg{})
		m = modelPtr(nm)
		if time.Since(lastSnap) >= 1*time.Second {
			lastSnap = time.Now()
			snapshot("running")
			if s := m.selectedSession(); s != nil && s.Status != "running" {
				snapshot("final")
				t.Logf("会话结束 status=%s，共 %d 个快照", s.Status, snapIdx)
				return
			}
		}
	}
	snapshot("timeout")
	t.Logf("到达超时，共 %d 个快照", snapIdx)
}
