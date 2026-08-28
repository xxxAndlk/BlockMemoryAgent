package tui

// live_multiturn_test.go 是手动触发的多轮 TUI 端到端驱动：
// 用真实后端（bootstrap.Build + 本地 HTTP）驱动真实 TUI Model，按剧本逐轮
// "打字 -> 回车 -> 等会话跑完 -> 抓回复"，复刻真人在终端里一轮一轮提问的行为。
// 用于多轮记忆一致性 / 高相似度块记忆混淆等需要连续对话的评测场景。
//
// 默认跳过；设置 BMA_LIVE_MULTITURN=1 才会运行：
//
//	BMA_LIVE_MULTITURN=1 BMA_TURNS_FILE=<剧本.json> go test ./internal/tui -run TestLiveMultiturn -v -timeout 3600s
//
// 环境变量：
//
//	BMA_LIVE_MULTITURN=1   必须，未设置则 skip
//	BMA_TURNS_FILE         剧本文件路径（JSON 字符串数组，每个元素一轮用户输入），必填
//	BMA_SNAP_DIR           快照/结果输出目录，默认 <root>/test/tmp/multiturn_<时间戳>
//	BMA_TURN_TIMEOUT_SEC   单轮等待上限秒数，默认 420
//	BMA_PG_DSN             覆盖 postgres DSN（追加到 env 文件尾部，优先级最高）
//	BMA_REDIS_ADDR         覆盖 redis 地址，同上
//	BMA_REDIS_PASSWORD     覆盖 redis 密码，同上
//
// 注意：会真实调用大模型（消耗 token），并在数据库中创建真实会话记录。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gin-gonic/gin"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/bootstrap"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// liveMultiturnRoot 返回项目根目录（本测试运行于 backend/internal/tui 下）。
func liveMultiturnRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("解析项目根目录失败: %v", err)
	}
	return root
}

// buildMergedEnvFile 读取根 .env 并在尾部追加测试覆盖项（LoadEnvFile 顺序覆盖，
// 尾部行优先级最高），写入 snapDir/merged.env，返回路径。
func buildMergedEnvFile(t *testing.T, root, snapDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		t.Fatalf("读取 .env 失败: %v", err)
	}
	var sb strings.Builder
	sb.Write(raw)
	if !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteString("\n")
	}
	// 默认指向 docker-compose.test.yml 的测试实例，避免污染开发库。
	pgDSN := os.Getenv("BMA_PG_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://blockmemory:blockmemory_dev@localhost:55432/blockmemory?sslmode=disable"
	}
	redisAddr := os.Getenv("BMA_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:56380"
	}
	redisPass := os.Getenv("BMA_REDIS_PASSWORD")
	if redisPass == "" {
		redisPass = "blockmemory_dev"
	}
	fmt.Fprintf(&sb, "POSTGRES_DSN=%s\nREDIS_ADDR=%s\nREDIS_PASSWORD=%s\n", pgDSN, redisAddr, redisPass)
	merged := filepath.Join(snapDir, "merged.env")
	if err := os.WriteFile(merged, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("写 merged.env 失败: %v", err)
	}
	return merged
}

// turnResult 记录单轮结果，落盘 results.json 供评分脚本消费。
type turnResult struct {
	Turn       int     `json:"turn"`
	Question   string  `json:"question"`
	Reply      string  `json:"reply"`
	Status     string  `json:"status"`
	ElapsedSec float64 `json:"elapsed_sec"`
	SessionID  string  `json:"session_id"`
	TimedOut   bool    `json:"timed_out"`
}

// TestLiveMultiturn 多轮驱动主流程。
func TestLiveMultiturn(t *testing.T) {
	if os.Getenv("BMA_LIVE_MULTITURN") == "" {
		t.Skip("设置 BMA_LIVE_MULTITURN=1 才运行该多轮端到端驱动")
	}
	turnsFile := os.Getenv("BMA_TURNS_FILE")
	if turnsFile == "" {
		t.Fatal("BMA_TURNS_FILE 必填（JSON 字符串数组剧本）")
	}
	raw, err := os.ReadFile(turnsFile)
	if err != nil {
		t.Fatalf("读取剧本失败: %v", err)
	}
	var turns []string
	if err := json.Unmarshal(raw, &turns); err != nil {
		t.Fatalf("解析剧本失败: %v", err)
	}
	if len(turns) == 0 {
		t.Fatal("剧本为空")
	}

	turnTimeout := 420 * time.Second
	if v := os.Getenv("BMA_TURN_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			turnTimeout = time.Duration(n) * time.Second
		}
	}

	runewidth.DefaultCondition.EastAsianWidth = false

	root := liveMultiturnRoot(t)
	snapDir := os.Getenv("BMA_SNAP_DIR")
	if snapDir == "" {
		snapDir = filepath.Join(root, "test", "tmp", "multiturn_"+time.Now().Format("20060102-150405"))
	}
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatalf("创建快照目录失败: %v", err)
	}

	envPath := buildMergedEnvFile(t, root, snapDir)

	ctx := t.Context()
	app, err := bootstrap.Build(ctx, bootstrap.ConfigPaths{
		ConfigPath: filepath.Join(root, "config", "config.yaml"),
		RolePath:   filepath.Join(root, "config", "roles.yaml"),
		EnvPath:    envPath,
		SoulPath:   filepath.Join(root, "config", "soul.md"),
		SkillPath:  filepath.Join(root, "config", "skills.yaml"),
	})
	if err != nil {
		t.Fatalf("bootstrap.Build 失败: %v", err)
	}
	defer app.Close()

	// 复刻 cmd/tui 的本地 HTTP 路由（输入栏经 HTTP 访问后端，Gin 引擎）。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	api := router.Group("/api")
	server.RegisterSessionRoutes(api, app.Server)
	app.DAGHandler.RegisterRoutes(api)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 失败: %v", err)
	}
	defer ln.Close()
	go func() { _ = http.Serve(ln, router) }()
	httpAddr := "http://" + ln.Addr().String()

	modelName := app.RoleConfig.MetaAgent.ModelConfig.Model
	m := NewModel(app.Agent, app.DAGHandler, httpAddr, modelName)
	m.SetLogger(app.Logger)

	nm, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m = modelPtr(nm)

	snapView := func(tag string) {
		path := filepath.Join(snapDir, fmt.Sprintf("view_%s.txt", tag))
		_ = os.WriteFile(path, []byte(stripANSI(m.View())), 0o644)
	}

	results := make([]turnResult, 0, len(turns))
	flush := func() {
		data, _ := json.MarshalIndent(results, "", "  ")
		_ = os.WriteFile(filepath.Join(snapDir, "results.json"), data, 0o644)
	}
	defer flush()

	// tick 一步并返回。
	tick := func() {
		nm, _ := m.Update(tickMsg{})
		m = modelPtr(nm)
	}

	for i, question := range turns {
		turnStart := time.Now()
		tag := fmt.Sprintf("turn%02d", i+1)

		// 等上一轮会话彻底结束（首轮无会话直接通过）。
		deadline := time.Now().Add(60 * time.Second)
		for {
			s := m.selectedSession()
			if s == nil || s.Status != enums.SessionStatusRunning {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 等上一轮结束超时", tag)
			}
			time.Sleep(150 * time.Millisecond)
			tick()
		}

		// 基线：记录回车前会话状态，用于区分"本轮新活动"与"上一轮残留终态"。
		var baseID string
		baseAssist := 0
		hadSession := false
		if s := m.selectedSession(); s != nil {
			hadSession = true
			baseID = s.ID
			for _, msg := range s.Messages {
				if msg.Role == enums.ChatRoleAssistant && !strings.Contains(msg.Content, "[tool_call]") {
					baseAssist++
				}
			}
		}

		// 打字 + 回车（复刻 harness：一次性 KeyRunes，模拟真人按键间隔防粘贴误判）。
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(question)})
		m = modelPtr(nm)
		m.inputBar.lastKeyTime = time.Now().Add(-200 * time.Millisecond)
		nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = modelPtr(nm)

		// 等服务端受理本轮输入。
		// - 首轮（此前无会话）：pendingSelectID 由 createSession 后台 goroutine 经
		//   sharedState 指针共享写入（#47 已修复，不再落到废弃的 Model 副本），
		//   tick 消费后自动 selectSession。此处仍保留手动 selectSession 兜底
		//   （等效用户点击列表），防止时序边缘下驱动卡死。
		// - 后续轮：旧会话已是终态，必须等到状态回到 running 才算本轮被受理。
		deadline = time.Now().Add(60 * time.Second)
		for {
			tick()
			s := m.selectedSession()
			if !hadSession {
				if s == nil && len(m.sessions) > 0 {
					t.Logf("%s 警告：pendingSelectID 未被消费，走手动 selectSession 兜底（#47 回归？）", tag)
					m.selectSession(len(m.sessions) - 1)
					s = m.selectedSession()
				}
				if s != nil && s.Status != "" {
					break
				}
			} else if s != nil && (s.ID != baseID || s.Status == enums.SessionStatusRunning) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s 会话未受理本轮输入（60s 超时）", tag)
			}
			time.Sleep(150 * time.Millisecond)
		}

		// 等本轮跑完：状态脱离 running/awaiting_clarify，且确实观察到本轮新活动
		// （见过 running，或助手消息数比基线多），防止把上一轮终态当本轮结果。
		res := turnResult{Turn: i + 1, Question: question}
		sawRunning := false
		turnDeadline := time.Now().Add(turnTimeout)
		for {
			time.Sleep(150 * time.Millisecond)
			tick()
			s := m.selectedSession()
			if s == nil {
				continue
			}
			res.SessionID = s.ID
			if s.Status == enums.SessionStatusRunning {
				sawRunning = true
				continue
			}
			if s.Status == enums.SessionStatusAwaitingClarify {
				continue
			}
			assist := 0
			for _, msg := range s.Messages {
				if msg.Role == enums.ChatRoleAssistant && !strings.Contains(msg.Content, "[tool_call]") {
					assist++
				}
			}
			if sawRunning || assist > baseAssist || s.ID != baseID {
				res.Status = string(s.Status)
				break
			}
			if time.Now().After(turnDeadline) {
				res.Status = "timeout"
				res.TimedOut = true
				break
			}
		}

		// 抓本轮回复作为结果。助手终答优先取 Messages 最后一条（不含 [tool_call]）；
		// 实测 TUI 会话的答复常只落在 Events 流、Messages 为空，因此依次回退到
		// Session.Result 与最后一条 MetaAgent 文本事件。
		if s := m.selectedSession(); s != nil {
			for j := len(s.Messages) - 1; j >= 0; j-- {
				msg := s.Messages[j]
				if msg.Role == enums.ChatRoleAssistant && !strings.Contains(msg.Content, "[tool_call]") {
					res.Reply = msg.Content
					break
				}
			}
			if res.Reply == "" {
				res.Reply = s.Result
			}
			if res.Reply == "" {
				for j := len(s.Events) - 1; j >= 0; j-- {
					ev := s.Events[j]
					if ev.Agent == "MetaAgent" && ev.Message != "" &&
						!strings.Contains(ev.Message, "[tool_call]") &&
						!strings.HasPrefix(ev.Type, "tool") && ev.Type != "think" {
						res.Reply = ev.Message
						break
					}
				}
			}
		}
		res.ElapsedSec = time.Since(turnStart).Seconds()
		results = append(results, res)
		flush()
		snapView(tag)

		t.Logf("%s status=%s elapsed=%.1fs reply=%d字", tag, res.Status, res.ElapsedSec, len([]rune(res.Reply)))
		if res.TimedOut {
			t.Fatalf("%s 单轮超时（>%s），中止剧本", tag, turnTimeout)
		}
	}
	snapView("final")
	t.Logf("全部 %d 轮完成，结果在 %s", len(results), snapDir)
}
