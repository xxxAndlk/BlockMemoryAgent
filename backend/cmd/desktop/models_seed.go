//go:build windows

// 模型注册表种子：只负责把包内 config-templates\models.json 铺到 config\ 下。
//
// 2026-11-18 起首启向导整页移除（用户要求"进入应用不弹任何配置页"）：桌面外壳不再
// 拦截启动流程，也不再把模型 Key 写成 .env 的火山预置值；模型一律由用户在应用内
// 模型页手动新增（新装机器拿到的是一份空注册表模板，{"models": []}）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// seedModelsTemplate 把 config\models.json 模板铺到 config\ 下（仅缺失或空文件时铺）。
//
// models.json 本体在禁运清单里（用户注册表可能含明文 Key），随包只带一份**空注册表
// 模板**；这里只补空缺，绝不覆盖用户已写过的注册表。
func seedModelsTemplate(root string) {
	dst := filepath.Join(root, "config", "models.json")
	// 0 字节也按"缺失"处理：老安装出现过空占位文件，空 JSON 会让注册表加载失败
	//（模型页新增模型报"模型注册表未启用"）。
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return
	}
	for _, src := range []string{
		filepath.Join(root, "app", "versions", currentVersion(root), "config-templates", "models.json"),
		filepath.Join(root, "config-templates", "models.json"),
	} {
		if fileExists(src) {
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if data, err := os.ReadFile(src); err == nil && len(data) > 0 {
				_ = os.WriteFile(dst, data, 0o644)
			}
			return
		}
	}
}

// currentVersion 读当前版本指针（用于找版本目录里的配置模板）。
func currentVersion(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "app", "current.json"))
	if err != nil {
		return ""
	}
	var cur struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil {
		return ""
	}
	return cur.Version
}
