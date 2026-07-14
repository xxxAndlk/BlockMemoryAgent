// Package logging 是 internal/logoutput 之上的一个薄外观层。
//
// 它保留现有的 Entry 常量与一个 Init 辅助函数，供希望获得现成 writer 的调用方使用
// （可选择性地与 os.Stderr 组合）。本包不再持有包级全局状态，也不修改标准 log 包的输出。
package logging

import (
	"io" // io.WriteCloser 接口
	"os" // os.Stderr 标准错误输出

	"github.com/blockmemory/agent/backend/internal/logoutput"
)

// Entry 标识日志来源入口点。
type Entry = logoutput.Entry

const (
	EntryBackend Entry = logoutput.EntryBackend // 后端服务入口
	EntryTUI     Entry = logoutput.EntryTUI     // 终端 TUI 入口
	EntryWeb     Entry = logoutput.EntryWeb     // Web 前端入口
)

// 已弃用：请改用 EntryBackend。
const EntryBrowser = EntryBackend

// Init 为指定 entry 在 dir 目录下创建一个按日轮转的日志 writer。
//
// 当 silent 为 false 时，返回的 writer 同时会把写入转发到 os.Stderr，
// 与之前 HTTP 入口的行为保持一致。当 silent 为 true 时（例如 TUI 的 alt-screen 模式），
// 仅返回文件 writer。
//
// 调用方拥有返回的 writer 并必须在关闭时调用 Close。本函数不会修改全局 log 包的输出。
func Init(entry Entry, dir string, silent bool) (io.WriteCloser, error) {
	// 创建底层按日轮转的日志 writer。
	w, err := logoutput.NewWriter(entry, dir)
	if err != nil {
		return nil, err
	}
	// silent 模式只返回文件 writer，不向 stderr 转发。
	if silent {
		return w, nil
	}
	// 非 silent 模式：同时写入文件与 stderr。
	return &multiWriter{file: w, stderr: os.Stderr}, nil
}

// multiWriter 同时向 stderr 与按日日志文件写入。
// Close 仅关闭文件端；stderr 保持打开。
type multiWriter struct {
	file   io.WriteCloser // 按日轮转日志文件 writer
	stderr io.Writer      // 标准错误输出
}

// Write 实现 io.Writer，同时向文件与 stderr 写入 p。
func (m *multiWriter) Write(p []byte) (int, error) {
	// 先写文件；文件出错仍尝试 stderr，避免日志丢失。
	nFile, errFile := m.file.Write(p)
	nStderr, errStderr := m.stderr.Write(p)
	// 文件端出错时返回文件写入量与文件错误。
	if errFile != nil {
		return nFile, errFile
	}
	// stderr 端出错时返回 stderr 写入量与 stderr 错误。
	if errStderr != nil {
		return nStderr, errStderr
	}
	// 正常 writer 二者写入量应相等；若不等则返回较小值。
	if nFile != nStderr {
		return nFile, nil
	}
	return nFile, nil
}

// Close 实现 io.WriteCloser，仅关闭文件端。
func (m *multiWriter) Close() error {
	return m.file.Close()
}
