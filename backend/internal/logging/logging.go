// Package logging 提供按天分割、按入口分文件的日志输出。
//
// 设计意图：
//   - 项目有两个入口（浏览器 HTTP 服务 / TUI），日志需要分开存放，
//     便于按入口排查问题：browser-YYYY-MM-DD.log 与 tui-YYYY-MM-DD.log
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
	EntryBrowser Entry = "browser" // 浏览器 HTTP 服务入口
	EntryTUI     Entry = "tui"     // 终端 TUI 入口
)

// dailyWriter 按天切换文件的 io.Writer。
// 跨天时关闭旧文件、打开新文件；同一天内复用已打开的句柄。
type dailyWriter struct {
	mu       sync.Mutex
	dir      string // 日志目录
	entry    Entry  // 入口名（用于文件名前缀）
	curDate  string // 当前文件对应的日期 YYYY-MM-DD
	curFile  *os.File
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
		if err := os.MkdirAll(w.dir, 0o755); err != nil {
			return 0, fmt.Errorf("create log dir: %w", err)
		}
		path := filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.entry, today))
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
	globalMu      sync.Mutex
	globalWriter  *dailyWriter
)

// Init 初始化全局日志输出：把标准 log 包的输出重定向到 stderr + 按天分割文件。
//
// 参数：
//   - entry：入口标识（EntryBrowser / EntryTUI），决定文件名前缀
//   - dir：日志目录，空串则使用 ./logs
//
// 行为：
//   - dir 会被自动创建（MkdirAll）
//   - 多次调用安全：后续调用会先关闭前一个 writer 再启用新的
//   - 设置 log 标志：Ldate | Ltime | Lmicroseconds | Lshortfile，便于定位调用点
//
// 返回 close 函数，供 defer 调用关闭文件句柄。
func Init(entry Entry, dir string) (close func() error, err error) {
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

	// MultiWriter 同时输出到 stderr 与文件：开发期可在终端实时查看，
	// 生产期可在文件中检索历史
	log.SetOutput(io.MultiWriter(os.Stderr, w))
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
