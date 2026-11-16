//go:build windows

// 首启向导：把"这台机器能不能干活"变成一个可引导的流程。
//
// 新装机器上的 .env 是模板值（占位 Key），config\models.json 也没有 —— 后端能起来，
// 但一次会话都跑不了（模型没有可用 Key），记忆召回也降级。商业应用的做法是首启
// 弹一个引导页：填完写进 .env，再继续启动。这里在原生窗口里做同样的事：
// 缺 Key 才引导，已经配过的一律不打扰（老用户不会被弹窗骚扰）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// setupState 首启向导要引导的配置项。
type setupState struct {
	NeedModel bool   // 缺模型 Key
	NeedEmbed bool   // 缺嵌入 Key
	ModelURL  string // 模型接口地址（可改）
	EmbedURL  string // 嵌入服务地址（可改）
	Err       string // 上次提交的错误（回显用）
}

// placeholderKey 判断是不是模板占位值（空 / your- / change-me / xxx 之类）。
func placeholderKey(v string) bool {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return true
	}
	for _, p := range []string{"your-", "your_", "change-me", "changeme", "xxx", "placeholder", "sk-your"} {
		if strings.Contains(v, p) {
			return true
		}
	}
	return false
}

// checkSetup 检查是否需要引导配置。
func checkSetup(root string) *setupState {
	envPath := filepath.Join(root, ".env")
	st := &setupState{
		ModelURL: readEnvValue(envPath, "ANTHROPIC_BASE_URL"),
		EmbedURL: readEnvValue(envPath, "EMBEDDING_BASE_URL"),
	}
	st.NeedModel = placeholderKey(readEnvValue(envPath, "ANTHROPIC_API_KEY"))
	st.NeedEmbed = placeholderKey(readEnvValue(envPath, "EMBEDDING_API_KEY"))
	return st
}

// applySetup 把向导填的值写进 .env（只写非空项，不动用户已有的其它配置）。
func applySetup(root, modelKey, modelURL, embedURL, embedKey string) error {
	envPath := filepath.Join(root, ".env")
	set := func(k, v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return setEnvValue(envPath, k, v)
	}
	if err := set("ANTHROPIC_API_KEY", modelKey); err != nil {
		return err
	}
	if err := set("ANTHROPIC_BASE_URL", modelURL); err != nil {
		return err
	}
	if err := set("EMBEDDING_BASE_URL", embedURL); err != nil {
		return err
	}
	if err := set("EMBEDDING_API_KEY", embedKey); err != nil {
		return err
	}
	// 模型注册表缺失时用模板补一份（models.json 在安装包里是"去密钥"的模板）。
	seedModelsTemplate(root)
	return nil
}

// seedModelsTemplate 首次运行时把 config\models.json 模板铺到 config\ 下。
//
// models.json 不在安装包里（原禁运清单：用户的注册表可能含明文 Key），改成随包
// 带一份**只含 ${VAR} 引用**的模板，装机时"缺失才铺"，用户可在模型页随时改。
func seedModelsTemplate(root string) {
	dst := filepath.Join(root, "config", "models.json")
	if fileExists(dst) {
		return
	}
	for _, src := range []string{
		filepath.Join(root, "app", "versions", currentVersion(root), "config-templates", "models.json"),
		filepath.Join(root, "config-templates", "models.json"),
	} {
		if fileExists(src) {
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			if data, err := os.ReadFile(src); err == nil {
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

// wizardDone 一次性关闭：向导提交或跳过后放行启动流程。
type wizardGate struct {
	ch   chan struct{}
	once sync.Once
}

func newWizardGate() *wizardGate { return &wizardGate{ch: make(chan struct{})} }

// open 放行（可重复调用，只生效第一次）。
func (g *wizardGate) open() { g.once.Do(func() { close(g.ch) }) }

// wait 等待用户完成向导。
func (g *wizardGate) wait() { <-g.ch }

// wizardHTML 首启引导页。
func wizardHTML(st *setupState) string {
	errBox := ""
	if st.Err != "" {
		errBox = "<div class=\"err\">" + htmlEscape(st.Err) + "</div>"
	}
	modelBlock := ""
	if st.NeedModel {
		modelBlock = `
      <label>模型 API Key<span class="req">必填</span></label>
      <input id="modelKey" type="password" placeholder="sk-..." autocomplete="off">
      <label>模型接口地址<span class="opt">可选</span></label>
      <input id="modelUrl" type="text" value="` + htmlEscape(st.ModelURL) + `" placeholder="https://api.anthropic.com">`
	}
	embedBlock := ""
	if st.NeedEmbed {
		embedBlock = `
      <label>嵌入服务地址<span class="opt">可选，留空则记忆召回降级</span></label>
      <input id="embedUrl" type="text" value="` + htmlEscape(st.EmbedURL) + `" placeholder="http://127.0.0.1:11434">
      <label>嵌入 API Key<span class="opt">可选</span></label>
      <input id="embedKey" type="password" placeholder="端点带鉴权时必填" autocomplete="off">`
	}
	return "<html><head><meta charset=\"utf-8\"><style>" +
		"body{font-family:'Segoe UI',sans-serif;background:#0d1929;color:#e2e8f0;padding:32px;line-height:1.7;" +
		"max-width:720px;margin:0 auto}" +
		"h1{font-size:20px;color:#5eead4;margin:0 0 6px}h2{font-size:13px;color:#94a3b8;font-weight:400;margin:0 0 22px}" +
		"label{display:block;margin:16px 0 6px;font-size:13px;color:#cbd5e1}" +
		"input{width:100%;box-sizing:border-box;padding:9px 11px;border-radius:8px;border:1px solid #24405c;" +
		"background:#0f2235;color:#e2e8f0;font-size:13px}" +
		"input:focus{outline:none;border-color:#5eead4}" +
		".req{color:#f87171;font-size:11px;margin-left:6px}.opt{color:#64748b;font-size:11px;margin-left:6px}" +
		".err{background:#3b1d24;border:1px solid #7f1d1d;color:#fecaca;padding:10px 12px;border-radius:8px;font-size:12px;margin-bottom:12px}" +
		".row{margin-top:26px;display:flex;gap:12px;align-items:center}" +
		"button{padding:9px 20px;border:0;border-radius:8px;font-size:13px;cursor:pointer}" +
		"#go{background:#5eead4;color:#0d1929;font-weight:600}#skip{background:#1e3a52;color:#cbd5e1}" +
		".tip{font-size:12px;color:#64748b;margin-top:18px}" +
		"</style></head><body>" +
		"<h1>欢迎使用 BlockMemoryAgent</h1>" +
		"<h2>先填好接入凭据，我们就能真正开始干活。填完会保存到安装目录的 .env，之后随时能在设置里改。</h2>" +
		errBox +
		modelBlock + embedBlock +
		"<div class=\"row\"><button id=\"go\" onclick=\"submitSetup()\">开始使用</button>" +
		"<button id=\"skip\" onclick=\"skipSetup()\">稍后再说</button></div>" +
		"<div class=\"tip\">跳过也能用：模型与记忆相关能力会保持降级，直到你在设置里补齐凭据。</div>" +
		"<script>" +
		"function collect(id){var e=document.getElementById(id);return e?e.value:''}" +
		"function submitSetup(){" +
		"  bmaSetupSubmit(collect('modelKey'),collect('modelUrl'),collect('embedUrl'),collect('embedKey'))" +
		"    .then(function(){document.getElementById('go').textContent='正在启动…';})" +
		"    .catch(function(e){alert('保存失败：'+e)})" +
		"}" +
		"function skipSetup(){ bmaSetupSkip().then(function(){}) }" +
		"</script></body></html>"
}
