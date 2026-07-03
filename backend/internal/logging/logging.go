// Package logging 提供按天分割、按入口分目录的日志输出。
//
// 设计意图：
//   - 项目有三个入口（backend HTTP 服务 / TUI / web），日志分开存放到子目录：
//     logs/backend/YYYY-MM-DD.log, logs/tui/YYYY-MM-DD.log, logs/web/YYYY-MM-DD.log
//   - 按天切割：写日志时检查当前日期，跨天则关闭旧文件、打开新文件
//   - 不引入 lumberjack 等额外依赖，纯标准库实现
//
// 使用方式：在 main.go 入口最早期调用 Init(entry, dir)，之后标准 log 包
// 的所有输出会同时写入 stderr 与对应日志文件。
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 标识日志来源入口。
type Entry string

const (
	EntryBackend Entry = "backend" // 后台 HTTP 服务入口
	EntryTUI     Entry = "tui"     // 终端 TUI 入口
	EntryWeb     Entry = "web"     // Web 前端入口
)

// Deprecated: 使用 EntryBackend 代替。
const EntryBrowser = EntryBackend

// dailyWriter 按天切换文件的 io.Writer。
// 跨天时关闭旧文件、打开新文件；同一天内复用已打开的句柄。
type dailyWriter struct {
	mu      sync.Mutex
	dir     string // 日志目录
	entry   Entry  // 入口名（用于文件名前缀）
	curDate string // 当前文件对应的日期 YYYY-MM-DD
	curFile *os.File
}

// Write 实现 io.Writer：每次写前检查日期，跨天则切换文件。
func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	// 首次写 / 跨天：切换文件
	if w.curFile == nil || w.curDate != today {
		if w.curFile != nil {
			// 旧文件关闭错误仅忽略：写入已经成功，关闭失败不影响日志
			_ = w.curFile.Close()
		}
		subDir := filepath.Join(w.dir, string(w.entry))
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			return 0, fmt.Errorf("create log dir: %w", err)
		}
		path := filepath.Join(subDir, fmt.Sprintf("%s.log", today))
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, fmt.Errorf("open log file: %w", err)
		}
		w.curFile = f
		w.curDate = today
	}
	return w.curFile.Write(p)
}

// Close 关闭当前打开的日志文件。供进程退出时调用。
func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curFile != nil {
		err := w.curFile.Close()
		w.curFile = nil
		return err
	}
	return nil
}

// 全局实例：Init 调用后持有，供 Close 时使用。
var (
	globalMu     sync.RWMutex
	globalWriter *dailyWriter
)

// Init 初始化全局日志输出：把标准 log 包的输出重定向到（可选 stderr +）按天分割文件。
//
// 参数：
//   - entry：入口标识（EntryBrowser / EntryTUI），决定文件名前缀
//   - dir：日志目录，空串则使用 ./logs
//   - silent：true 时仅写文件、不写 stderr。TUI 入口（bubbletea alt-screen 全屏
//     接管终端）必须传 true，否则日志会刷到屏幕上顶乱 TUI 布局、把输入框顶跑；
//     HTTP 入口传 false，开发期可在终端实时查看日志。
//
// 行为：
//   - dir 会被自动创建（MkdirAll）
//   - 多次调用安全：后续调用会先关闭前一个 writer 再启用新的
//   - 设置 log 标志：Ldate | Ltime | Lmicroseconds | Lshortfile，便于定位调用点
//
// 返回 close 函数，供 defer 调用关闭文件句柄。
func Init(entry Entry, dir string, silent bool) (close func() error, err error) {
	if dir == "" {
		dir = "logs"
	}

	globalMu.Lock()
	defer globalMu.Unlock()

	// 关闭前一个 writer（若有），避免句柄泄漏
	if globalWriter != nil {
		_ = globalWriter.Close()
	}

	w := &dailyWriter{dir: dir, entry: entry}
	// 预创建目录与首日文件，提前暴露错误避免运行时才发现
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// 触发一次 Write 打开首日文件
	if _, err := w.Write([]byte("")); err != nil {
		return nil, err
	}
	globalWriter = w

	if silent {
		// TUI 模式：仅写文件，让 bubbletea 独占终端，避免日志上屏顶乱布局
		log.SetOutput(w)
	} else {
		// MultiWriter 同时输出到 stderr 与文件：开发期可在终端实时查看，
		// 生产期可在文件中检索历史
		log.SetOutput(io.MultiWriter(os.Stderr, w))
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)

	return func() error { return w.Close() }, nil
}

// Close 关闭全局 writer（若有）。供进程优雅退出时调用。
func Close() error {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalWriter == nil {
		return nil
	}
	err := globalWriter.Close()
	globalWriter = nil
	return err
}

// Writer 返回当前全局 writer，供其他日志组件（如结构化 logger）复用同一文件。
// 未调用 Init 时返回 nil。
func Writer() io.Writer {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalWriter
}
