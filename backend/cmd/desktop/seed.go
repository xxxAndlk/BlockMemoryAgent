//go:build windows

// 安装向导种子（config\install-seed.json）的消费逻辑。
//
// 为什么要有这一层：安装向导（installer\blockmemoryagent.iss）里让用户填 API Key /
// 接口地址，是最符合桌面软件习惯的体验（双击 → 选目录 → 填配置 → 安装）。但
// **Inno Setup 6 没有 UTF-8 文件 API**（实测 SaveStringToUTF8File 不存在，
// SaveStringToFile 按 ANSI 写），直接写 .env 会把里面的中文注释搞成乱码。
// 所以向导只落一份**纯 ASCII** 的种子 JSON，由这里在首启时合并进 .env：
//   * 值是 ASCII（key / url），JSON 本身纯 ASCII → Inno 写 ANSI 也不失真；
//   * .env 可能已被用户改过 → 合并（只覆盖种子里的非空项，其余原样保留）；
//   * 合并成功后删除种子文件，避免以后误以为还能再改它生效（真改配置走设置页）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// installSeed 是安装向导写下的凭据种子。字段名与 .iss 里 WriteInstallSeed 产出的 JSON 对齐。
type installSeed struct {
	ModelAPIKey  string `json:"model_api_key"`
	ModelBaseURL string `json:"model_base_url"`
	EmbedBaseURL string `json:"embed_base_url"`
	EmbedAPIKey  string `json:"embed_api_key"`
	// Provider 是向导里选的服务商索引（0=Anthropic 1=DeepSeek 2=OpenAI 3=ARK 4=自定义）。
	// 目前只用于诊断日志：真正的路由由 base_url 决定，不需要额外映射表。
	Provider string `json:"provider"`
}

// seedPath 返回种子文件路径。
func seedPath(root string) string {
	return filepath.Join(root, "config", "install-seed.json")
}

// readInstallSeed 读种子文件；不存在/解析失败都返回 nil（不是错误——正常升级没有种子）。
func readInstallSeed(root string) *installSeed {
	raw, err := os.ReadFile(seedPath(root))
	if err != nil {
		return nil
	}
	var s installSeed
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	if strings.TrimSpace(s.ModelAPIKey) == "" && strings.TrimSpace(s.EmbedAPIKey) == "" {
		return nil
	}
	return &s
}

// applyInstallSeed 把种子合并进 .env，然后删除种子文件。
//
// 返回 true 表示确实写入了内容（调用方据此跳过首启向导）。写失败时保留种子文件，
// 下次启动还会重试——比"静默丢掉用户刚填的 Key"好得多。
func applyInstallSeed(root string) bool {
	seed := readInstallSeed(root)
	if seed == nil {
		return false
	}
	envPath := filepath.Join(root, ".env")
	set := func(k, v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return setEnvValue(envPath, k, v)
	}

	// 模型 Key 写到 ANTHROPIC_API_KEY：config\models.json 里 7 个主力模型都引用它，
	// 向导里的"模型服务商"实际改的是 base_url（同一套 Anthropic 协议兼容端点）。
	if err := set("ANTHROPIC_API_KEY", seed.ModelAPIKey); err != nil {
		return false
	}
	if err := set("ANTHROPIC_BASE_URL", seed.ModelBaseURL); err != nil {
		return false
	}
	if err := set("EMBEDDING_BASE_URL", seed.EmbedBaseURL); err != nil {
		return false
	}
	if err := set("EMBEDDING_API_KEY", seed.EmbedAPIKey); err != nil {
		return false
	}

	// models.json 模板也顺带铺好（安装包带的是去密钥模板，缺失才铺）。
	seedModelsTemplate(root)

	// 合并成功 → 删种子。删不掉不算失败（已生效），下次启动会再合并一次（幂等）。
	_ = os.Remove(seedPath(root))
	return true
}
