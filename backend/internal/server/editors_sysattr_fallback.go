//go:build !windows && !darwin && !linux

package server

// editors_sysattr_fallback.go 兜底平台（plan9 等）：syscall.SysProcAttr 字段集与
// unix 不同，脱离启动按"直接 Start 不 Wait"处理（编辑器/打开器仍是独立进程，
// 只是与服务同进程组）。

import "syscall"

// detachSysProcAttr 兜底平台无脱离属性可用，返回 nil（仅继承句柄关闭等默认行为）。
func detachSysProcAttr() *syscall.SysProcAttr { return nil }
