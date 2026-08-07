package tool

// project_refresh.go 提供文件变更后去抖异步刷新 .bma/PROJECT.md managed 区的机制。
// WriteFile 成功 / RunCommand 命中 rm/mv/mkdir/touch 等删改类命令后 schedule，
// 安静期（delay）触发一次 project.RefreshProjectDoc（LLM 按职责重分区，nil 走启发式），
// 使领域影响范围随文件增删改自动更新。全程背景 ctx，不阻塞工具主路径，错误仅 slog。

import (
	"context"
	"strings"
	"sync"
	"time"
)

// projectRefreshDelay 是文件变更后触发 PROJECT.md 刷新的去抖延迟。
// Agent 连续写文件时不断重置计时器，仅在安静期触发一次，避免每写一次就跑 LLM。
const projectRefreshDelay = 3 * time.Second

// projectRefresher 去抖异步刷新 PROJECT.md。连续 schedule 重置计时器，延迟后触发一次 refresh。
type projectRefresher struct {
	mu      sync.Mutex
	timer   *time.Timer
	workDir string
	delay   time.Duration
	refresh func(ctx context.Context, workDir string)
}

// newProjectRefresher 构造去抖刷新器。refresh 在背景 ctx 调用，错误由 refresh 自身处理。
func newProjectRefresher(delay time.Duration, refresh func(ctx context.Context, workDir string)) *projectRefresher {
	return &projectRefresher{delay: delay, refresh: refresh}
}

// schedule 重置计时器，delay 后触发一次 refresh。连续调用以最后一次 workDir 为准。
func (p *projectRefresher) schedule(workDir string) {
	if p == nil || workDir == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workDir = workDir
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(p.delay, p.fire)
}

// fire 执行 refresh。读 workDir 后释放锁，避免 refresh 持锁阻塞 schedule。
func (p *projectRefresher) fire() {
	p.mu.Lock()
	wd := p.workDir
	p.mu.Unlock()
	if wd == "" || p.refresh == nil {
		return
	}
	// 背景 ctx：不绑工具调用 ctx，避免会话取消后刷新丢失。refresh 内部自处理错误（slog）。
	p.refresh(context.Background(), wd)
}

// commandAffectsFiles 判断 RunCommand 命令是否可能增删改文件，需触发 PROJECT.md 刷新。
// best-effort：首个非选项 token（命令本身）命中 rm/mv/mkdir/touch/cp 触发；
// git 命令再看其首个非选项子命令是否 rm/mv（git rm/git mv）。避免把文件名误判为命令。
// 漏匹配（如 find -delete）仅导致领域范围滞后到下次显式 RefreshProjectDoc，可接受。
func commandAffectsFiles(cmd string) bool {
	if cmd == "" {
		return false
	}
	tokens := strings.Fields(cmd)
	cmdTok := ""
	var rest []string
	for i, t := range tokens {
		if strings.HasPrefix(t, "-") {
			continue
		}
		cmdTok = t
		rest = tokens[i+1:]
		break
	}
	switch cmdTok {
	case "rm", "mv", "mkdir", "touch", "cp":
		return true
	case "git":
		for _, t := range rest {
			if strings.HasPrefix(t, "-") {
				continue
			}
			return t == "rm" || t == "mv"
		}
	}
	return false
}

