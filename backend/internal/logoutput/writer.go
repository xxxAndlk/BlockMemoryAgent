// Package logoutput 提供一个支持按日轮转的日志 writer。
//
// 它不了解结构化日志或标准 log 包；只产生一个 io.WriteCloser，
// 向按天轮转并按入口组织的文件写入（backend/tui/web）。
// 调用方可以自行决定是否与 os.Stderr、slog 或其他 writer 组合使用。
package logoutput

import (
	"fmt"           // 错误格式化
	"io"            // io.WriteCloser / io.Writer 接口
	"os"            // 文件操作
	"path/filepath" // 路径拼接
	"sync"          // Mutex 保护并发写入
	"time"          // 日期计算与格式化
)

// Entry 标识日志文件组织的来源入口点。
type Entry string

const (
	EntryBackend Entry = "backend" // HTTP 后端服务入口
	EntryTUI     Entry = "tui"     // 终端 TUI 入口
	EntryWeb     Entry = "web"     // Web 前端入口
)

// 已弃用：请改用 EntryBackend。
const EntryBrowser = EntryBackend

// dailyWriter 按天轮转日志文件，可并发安全使用。
type dailyWriter struct {
	mu      sync.Mutex // 保护 curFile / curDate 等字段的并发访问
	dir     string     // 日志根目录
	entry   Entry      // 来源入口
	curDate string     // 当前打开文件对应的日期（YYYY-MM-DD）
	curFile *os.File   // 当前打开的日志文件句柄
}

// Write 实现 io.Writer。首次写入时打开当天日志文件，并在日期变化时切换到新文件。
func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()         // 加锁：可能修改 curFile / curDate
	defer w.mu.Unlock() // 函数退出时释放锁

	// 空写入直接返回 0，不触发文件打开逻辑。
	if len(p) == 0 {
		return 0, nil
	}

	today := time.Now().Format("2006-01-02")
	// 首次写入或跨天时，打开/切换到当天的日志文件。
	if w.curFile == nil || w.curDate != today {
		if w.curFile != nil {
			// 尽力关闭前一天的文件句柄，错误忽略。
			_ = w.curFile.Close()
		}
		// 子目录按 entry 组织：logs/<entry>/。
		subDir := filepath.Join(w.dir, string(w.entry))
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			return 0, fmt.Errorf("create log dir: %w", err)
		}
		// 日志文件名为日期：YYYY-MM-DD.log。
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

// Close 实现 io.WriteCloser。
func (w *dailyWriter) Close() error {
	w.mu.Lock()         // 加锁：关闭并清空 curFile
	defer w.mu.Unlock() // 函数退出时释放锁
	if w.curFile != nil {
		err := w.curFile.Close()
		w.curFile = nil // 清空句柄，防止关闭后复用
		return err
	}
	return nil
}

// NewWriter 为指定 entry 创建一个按日轮转的日志 writer。
// 返回的 writer 写入 dir/logs/<entry>/YYYY-MM-DD.log。
// dir 为空时默认使用 "logs"。
func NewWriter(entry Entry, dir string) (io.WriteCloser, error) {
	// dir 为空时回退到默认 "logs" 目录。
	if dir == "" {
		dir = "logs"
	}
	// 预先创建日志根目录，使配置错误在初始化阶段暴露。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &dailyWriter{dir: dir, entry: entry}
	// 提前触发一次文件创建，让权限/路径等问题在 NewWriter 时就暴露。
	if _, err := w.Write([]byte("")); err != nil {
		return nil, err
	}
	return w, nil
}
