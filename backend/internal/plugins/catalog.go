package plugins

// catalog.go 实现插件目录检索（TODO #51 插件系统 Agent 自安装闭环）：
//   - 内置精选目录（builtinCatalog）：官方/知名 MCP server，settings 完整可直接安装；
//   - 远程 registry：MCP 官方 registry API（registry.modelcontextprotocol.io/v0/servers）
//     JSON 契约：{"servers":[{"server":{name,title,description,version,
//     remotes:[{type:"streamable-http",url}], packages:[{registryType:"npm",
//     identifier, transport:{type:"stdio"}}]}, "_meta":{...}}]}；
//     来源白名单 = config.PluginsConfig.RegistrySources（缺省官方端点），
//     只查询白名单内 URL，防 Agent 拉任意来源；
//   - Manager.Search 聚合三路：内置目录 + 已配置/已安装实例（带状态）+ 远程 registry
//     （失败仅记 note，不阻断内置结果）。

import (
	"context"   // 检索上下文（registry 请求取消/超时）
	"encoding/json" // registry 响应解析
	"fmt"       // 错误文案
	"io"        // 响应体读取
	"net/http"  // registry 查询
	"net/url"   // host 提取（来源标注）
	"strings"   // 大小写不敏感匹配与 ID 净化
	"time"      // registry 请求超时
)

// 远程 registry 请求超时：检索是工具调用路径，挂起会阻塞 ReAct 循环，必须短超时 fail-soft。
const registryFetchTimeout = 8 * time.Second

// 单次检索输出上限（内置目录 + 配置实例 + 远程聚合后截断，防上下文膨胀）。
const searchResultLimit = 30

// CatalogEntry 是目录检索结果条目（内置目录 / 远程 registry / 已配置实例统一形态）。
type CatalogEntry struct {
	// ID 插件 ID（安装时作为插件标识；远程条目经 sanitizeID 净化）。
	ID string
	// Name 展示名。
	Name string
	// Version 版本号（可空）。
	Version string
	// Description 一句话功能描述。
	Description string
	// Kind 插件形态（mcp/service）。
	Kind Kind
	// RequiresEnv 依赖的环境变量（缺失时 enable 拒绝）。
	RequiresEnv []string
	// Roles 可见角色白名单（缺省全角色）。
	Roles []string
	// Settings 完整插件配置（install 时直接持久化）。
	Settings map[string]any
	// Source 来源：builtin（内置目录）| mcp-registry（远程 registry）| config（已配置实例）。
	Source string
	// State 已配置实例的当前状态（Source=config 时有效，如 running/stopped）。
	State string
	// Tools 已配置实例当前注册的工具（Source=config 时有效）。
	Tools []string
}

// 来源常量。
const (
	sourceBuiltin  = "builtin"
	sourceRegistry = "mcp-registry"
	sourceConfig   = "config"
)

// 内置精选目录：官方 / 高知名度 MCP server，settings 完整、可直接 plugin_install。
// 命令统一 npx -y 拉取（Claude Code 同款惯例）；需 env 的条目在 RequiresEnv 注明。
var builtinCatalog = []CatalogEntry{
	{
		ID: "sequential_thinking", Name: "Sequential Thinking",
		Description: "思维链逐步分解（官方 example server）：把复杂问题拆成有序思考步骤，逐步收敛结论。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-sequential-thinking"}},
	},
	{
		ID: "filesystem", Name: "Filesystem",
		Description: "文件系统读写（官方参考 server）：list/read/write/search 指定根目录，适合需要结构化文件操作的场景。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-filesystem"}},
	},
	{
		ID: "fetch", Name: "Fetch",
		Description: "网页抓取（官方参考 server）：抓取 URL 转 Markdown，适合需要阅读网页正文的任务。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-fetch"}},
	},
	{
		ID: "memory", Name: "Knowledge Graph Memory",
		Description: "知识图谱记忆（官方参考 server）：实体/关系持久化记忆，跨会话知识积累。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-memory"}},
	},
	{
		ID: "time", Name: "Time",
		Description: "时间与时区（官方参考 server）：查询当前时间/时区转换，适合时间敏感任务。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-time"}},
	},
	{
		ID: "git", Name: "Git",
		Description: "Git 仓库操作（官方参考 server）：status/log/diff/blame 等只读操作，本系统内置 GitDiff/GitStatus 已有覆盖，按需安装。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-git"}},
	},
	{
		ID: "github", Name: "GitHub",
		Description: "GitHub API（官方参考 server）：issue/PR/repo 读写，需要 GITHUB_PERSONAL_ACCESS_TOKEN 环境变量。",
		Kind: KindMCP, Source: sourceBuiltin, RequiresEnv: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-github"}},
	},
	{
		ID: "gitlab", Name: "GitLab",
		Description: "GitLab API（官方参考 server）：project/issue/MR 操作，需要 GITLAB_PERSONAL_ACCESS_TOKEN 环境变量。",
		Kind: KindMCP, Source: sourceBuiltin, RequiresEnv: []string{"GITLAB_PERSONAL_ACCESS_TOKEN"},
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-gitlab"}},
	},
	{
		ID: "slack", Name: "Slack",
		Description: "Slack 工作区（官方参考 server）：频道/消息读写，需要 SLACK_BOT_TOKEN 环境变量。",
		Kind: KindMCP, Source: sourceBuiltin, RequiresEnv: []string{"SLACK_BOT_TOKEN"},
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-slack"}},
	},
	{
		ID: "firecrawl", Name: "Firecrawl",
		Description: "网页搜索/抓取/解析（云端免 key 端点）：search/scrape/parse，网络可达时零配置可用。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "http", "url": "https://mcp.firecrawl.dev/v2/mcp"},
	},
	{
		ID: "everything", Name: "Everything",
		Description: "全类型工具示例（官方测试 server）：echo 等演示工具，验证桥接链路用。",
		Kind: KindMCP, Source: sourceBuiltin,
		Settings: map[string]any{"transport": "stdio", "command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-everything"}},
	},
}

// SearchCatalog 在内置精选目录中按 query 做大小写不敏感子串匹配
// （id/name/description 任一命中）；query 为空返回全量目录。
func SearchCatalog(query string) []CatalogEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]CatalogEntry, 0, len(builtinCatalog))
	for _, e := range builtinCatalog {
		if q == "" || strings.Contains(strings.ToLower(e.ID), q) ||
			strings.Contains(strings.ToLower(e.Name), q) ||
			strings.Contains(strings.ToLower(e.Description), q) {
			out = append(out, e)
		}
	}
	return out
}

// registryServersResponse 对应 MCP 官方 registry API 的响应外壳。
type registryServersResponse struct {
	Servers []registryServerItem `json:"servers"`
}

type registryServerItem struct {
	Server registryServer       `json:"server"`
	Meta   registryServerMeta   `json:"_meta"`
}

type registryServer struct {
	Name        string                `json:"name"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Version     string                `json:"version"`
	Remotes     []registryRemote      `json:"remotes"`
	Packages    []registryPackage     `json:"packages"`
}

type registryRemote struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type registryPackage struct {
	RegistryType string            `json:"registryType"`
	Identifier   string            `json:"identifier"`
	Version      string            `json:"version"`
	Transport    registryTransport `json:"transport"`
}

type registryTransport struct {
	Type string `json:"type"`
}

type registryServerMeta struct {
	Official *registryOfficialMeta `json:"io.modelcontextprotocol.registry/official"`
}

type registryOfficialMeta struct {
	IsLatest bool `json:"isLatest"`
}

// fetchRemoteRegistry 查询远程 registry 来源（白名单 = 传入的 sources 本身，调用方保证
// 只传配置允许的 URL）。每个来源独立 fail-soft：失败记入 notes 不阻断其他来源。
// 返回条目已按 query 过滤、按名称去重（isLatest 优先）、转成可安装的 CatalogEntry。
func fetchRemoteRegistry(ctx context.Context, sources []string, query string) ([]CatalogEntry, []string) {
	var out []CatalogEntry
	var notes []string
	q := strings.ToLower(strings.TrimSpace(query))
	client := &http.Client{Timeout: registryFetchTimeout}
	seen := map[string]bool{}
	for _, src := range sources {
		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}
		entries, err := fetchRegistrySource(ctx, client, src, q)
		if err != nil {
			notes = append(notes, fmt.Sprintf("registry %s 不可用: %v", registryHost(src), err))
			continue
		}
		for _, e := range entries {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out, notes
}

// fetchRegistrySource 拉取单个 registry 来源全量清单并过滤/转换。
func fetchRegistrySource(ctx context.Context, client *http.Client, base, q string) ([]CatalogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed registryServersResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	// 去重：同一 name 多版本只留 isLatest（无 official 元数据的条目保留，宽松处理）。
	latest := map[string]registryServerItem{}
	for _, item := range parsed.Servers {
		meta := item.Meta.Official
		if meta == nil {
			if _, ok := latest[item.Server.Name]; !ok {
				latest[item.Server.Name] = item
			}
			continue
		}
		if meta.IsLatest {
			latest[item.Server.Name] = item
		}
	}
	host := registryHost(base)
	var out []CatalogEntry
	for name, item := range latest {
		srv := item.Server
		if q != "" && !strings.Contains(strings.ToLower(name), q) &&
			!strings.Contains(strings.ToLower(srv.Title), q) &&
			!strings.Contains(strings.ToLower(srv.Description), q) {
			continue
		}
		e, ok := registryEntryFromServer(name, srv, host)
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// registryEntryFromServer 把 registry server 描述转换为可安装条目：
//   - 优先 streamable-http remotes → transport=http + url；
//   - 否则 npm stdio package → transport=stdio + npx -y <identifier>；
//   - 两者皆无可安装形态 → 跳过。
func registryEntryFromServer(name string, srv registryServer, host string) (CatalogEntry, bool) {
	settings := map[string]any{}
	if len(srv.Remotes) > 0 {
		for _, r := range srv.Remotes {
			if r.Type == "streamable-http" && strings.HasPrefix(r.URL, "http") {
				settings["transport"] = "http"
				settings["url"] = r.URL
				break
			}
		}
	}
	if settings["transport"] == nil {
		for _, p := range srv.Packages {
			if p.RegistryType == "npm" && p.Identifier != "" && p.Transport.Type == "stdio" {
				settings["transport"] = "stdio"
				settings["command"] = "npx"
				settings["args"] = []any{"-y", p.Identifier}
				break
			}
		}
	}
	if settings["transport"] == nil {
		return CatalogEntry{}, false
	}
	title := srv.Title
	if title == "" {
		title = name
	}
	return CatalogEntry{
		ID:          sanitizeID(name),
		Name:        title,
		Version:     srv.Version,
		Description: srv.Description,
		Kind:        KindMCP,
		Settings:    settings,
		Source:      sourceRegistry,
		State:       host,
	}, true
}

// sanitizeID 把任意来源的 server 名净化成合法插件 ID（[a-zA-Z0-9._-]）。
func sanitizeID(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// registryHost 提取 registry URL 的 host 用于来源标注。
func registryHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
