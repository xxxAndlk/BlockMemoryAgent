package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// looksLikeHome 判断 dir 是否为 BMA 安装目录(含 config/config.yaml)。
func looksLikeHome(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "config", "config.yaml"))
	return err == nil && !info.IsDir()
}

// resolveHome 是 HomeDir 的纯函数核心,便于测试。
// 顺序:BMA_HOME env > exe 上级(bin 再上一级;兼容开发态 backend/)> cwd。
func resolveHome(env, exePath, cwd string) (string, error) {
	if env != "" {
		if looksLikeHome(env) {
			return filepath.Clean(env), nil
		}
		return "", fmt.Errorf("BMA_HOME=%s 无效:缺少 config/config.yaml", env)
	}
	if exePath != "" {
		dir := filepath.Dir(exePath)
		cands := []string{dir, filepath.Dir(dir)} // exe 同级、上级(bin/ 或 backend/ 布局)
		if filepath.Base(dir) == "bin" {
			cands = []string{filepath.Dir(dir), dir}
		}
		for _, c := range cands {
			if looksLikeHome(c) {
				return filepath.Clean(c), nil
			}
		}
	}
	if looksLikeHome(cwd) {
		return filepath.Clean(cwd), nil
	}
	return "", fmt.Errorf("无法定位安装目录:请设置 BMA_HOME 环境变量,或用 -config 等 flag 显式指定")
}

// HomeDir 解析 BMA 安装目录(BMA_HOME)。
func HomeDir() (string, error) {
	exe := ""
	if p, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		exe = p
	}
	cwd, _ := os.Getwd()
	return resolveHome(os.Getenv("BMA_HOME"), exe, cwd)
}

// ResolveUnderHome 把相对路径拼到 home 下;绝对路径原样返回。
func ResolveUnderHome(home, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(home, p)
}
