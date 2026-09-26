package server

// capabilities.go 装机能力自检（TODO #18-1 T28）：
// GET /api/capabilities 逐项报告 LLM / Postgres / Redis / 嵌入 / 插件 / 工作目录六类
// 能力的健康状态，每项 {name, ok, detail, missing, hint}——ok=false 时给可行动的
// 中文修复提示（装机向导与 settings 能力面板的数据源）。
//
// 设计口径：
//   - 全部廉价检查（ping / os.Stat / 配置读取），不真调 LLM API——生产启动是严格
//     模式（bootstrap.Build 连通性校验失败即拒启），进程活着本身就证明 LLM 通。
//   - 全字段 nil 安全：测试桩缺任一依赖时对应项降级为"未接线"，不 panic。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
)

// CapabilityItem 单项能力自检结果。
type CapabilityItem struct {
	Name    string   `json:"name"`              // 能力名（llm/postgres/redis/embed/plugins/workdir）
	OK      bool     `json:"ok"`                // 是否就绪
	Detail  string   `json:"detail,omitempty"`  // 现状说明（模型名/延迟/问题插件清单）
	Missing []string `json:"missing,omitempty"` // 缺失的环境变量或配置键
	Hint    string   `json:"hint,omitempty"`    // ok=false 时的可行动修复提示
}

// CapabilitiesHandler 处理 GET /api/capabilities — 六类能力逐项自检。
func (h *APIHandler) CapabilitiesHandler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	items := []CapabilityItem{
		h.capabilityLLM(),
		h.capabilityPostgres(ctx),
		h.capabilityRedis(ctx),
		h.capabilityEmbed(),
		h.capabilityPlugins(),
		h.capabilityWorkDir(),
	}
	allOK := true
	for _, it := range items {
		if !it.OK {
			allOK = false
			break
		}
	}
	// default_work_dir：新会话未选工作目录时的落盘根（进程默认工作目录，bootstrap 经
	// SetDefaultWorkDir 注入）。前端用它填目录选择器的占位文案，免得用户新建会话时
	// 不知道文件会落到哪儿（与 workdir 探针同源，都取 h.defaultWorkDir）。
	c.JSON(200, gin.H{"items": items, "all_ok": allOK, "default_work_dir": h.defaultWorkDir})
}

// capabilityLLM 检查 LLM 配置可解析（生产严格启动已保证连通，这里只报当前生效模型）。
func (h *APIHandler) capabilityLLM() CapabilityItem {
	if h.modelFactory == nil {
		return CapabilityItem{Name: "llm", OK: false, Detail: "模型工厂未接线",
			Hint: "正常生产启动不会出现此项；若持续存在请检查启动日志"}
	}
	st, err := h.modelFactory.CurrentModelInfo("meta")
	if err != nil {
		return CapabilityItem{Name: "llm", OK: false, Detail: err.Error(),
			Hint: "检查 config/models.json 的 api_key/base_url 与 roles.yaml 模型绑定，改后重启生效"}
	}
	return CapabilityItem{Name: "llm", OK: true,
		Detail: fmt.Sprintf("%s/%s（启动连通性校验已通过）", st.Provider, st.Model)}
}

// capabilityPostgres Postgres 探活（nil 存储=未配置，区分于连不上）。
func (h *APIHandler) capabilityPostgres(ctx context.Context) CapabilityItem {
	if h.pgStore == nil || h.pgStore.DB() == nil {
		return CapabilityItem{Name: "postgres", OK: false, Detail: "未配置",
			Hint: "在 config.yaml 填 postgres 连接参数（docker compose up -d postgres 可起本地实例）"}
	}
	start := time.Now()
	if err := h.pgStore.DB().PingContext(ctx); err != nil {
		return CapabilityItem{Name: "postgres", OK: false, Detail: err.Error(),
			Hint: "数据库连不上：确认 Postgres 已启动、config.yaml 连接参数与端口正确"}
	}
	return CapabilityItem{Name: "postgres", OK: true,
		Detail: fmt.Sprintf("connected（%dms）", time.Since(start).Milliseconds())}
}

// capabilityRedis Redis 探活。
func (h *APIHandler) capabilityRedis(ctx context.Context) CapabilityItem {
	if h.redisStore == nil {
		return CapabilityItem{Name: "redis", OK: false, Detail: "未配置",
			Hint: "在 config.yaml 填 redis 连接参数（docker compose up -d redis 可起本地实例）"}
	}
	start := time.Now()
	if err := h.redisStore.Ping(ctx); err != nil {
		return CapabilityItem{Name: "redis", OK: false, Detail: err.Error(),
			Hint: "Redis 连不上：确认服务已启动、地址与端口正确"}
	}
	return CapabilityItem{Name: "redis", OK: true,
		Detail: fmt.Sprintf("connected（%dms）", time.Since(start).Milliseconds())}
}

// capabilityEmbed 嵌入配置检查（pseudo/onnx 免 Key；openai/local 需要密钥或本地端点）。
func (h *APIHandler) capabilityEmbed() CapabilityItem {
	if h.roleCfg == nil {
		return CapabilityItem{Name: "embed", OK: false, Detail: "角色配置未接线"}
	}
	emb := h.roleCfg.Embed
	switch emb.Provider {
	case "", "pseudo", "onnx":
		return CapabilityItem{Name: "embed", OK: true,
			Detail: fmt.Sprintf("provider=%s（无需外部服务）", emb.Provider)}
	case "openai", "local":
		if emb.APIKey == "" && emb.BaseURL == "" {
			return CapabilityItem{Name: "embed", OK: false,
				Detail:  fmt.Sprintf("provider=%s 但未配置 api_key/base_url", emb.Provider),
				Missing: []string{"roles.yaml: embed.api_key"},
				Hint:    "在 roles.yaml embed 段补 api_key（或 base_url 指向本地嵌入服务）；离线场景可改 provider=pseudo/onnx"}
		}
		return CapabilityItem{Name: "embed", OK: true,
			Detail: fmt.Sprintf("provider=%s model=%s", emb.Provider, emb.Model)}
	default:
		return CapabilityItem{Name: "embed", OK: false,
			Detail:  fmt.Sprintf("未知 provider=%s", emb.Provider),
			Hint:    "provider 取值：pseudo | openai | local | onnx"}
	}
}

// capabilityPlugins 插件健康聚合：启用中的插件缺 env / 上次启动出错即不合格。
// 逐项列出问题插件（装机向导按提示补 .env 变量），停用插件不计入（用户主动关的）。
func (h *APIHandler) capabilityPlugins() CapabilityItem {
	if h.pluginMgr == nil {
		return CapabilityItem{Name: "plugins", OK: true, Detail: "插件管理器未接线（无插件模式）"}
	}
	var problems []string
	for _, p := range h.pluginMgr.List() {
		if !p.Enabled {
			continue
		}
		if len(p.MissingEnv) > 0 {
			problems = append(problems, fmt.Sprintf("%s 缺环境变量 %v", p.ID, p.MissingEnv))
		}
		if p.LastError != "" {
			problems = append(problems, fmt.Sprintf("%s 异常: %s", p.ID, p.LastError))
		}
	}
	if len(problems) > 0 {
		detail := problems[0]
		if len(problems) > 1 {
			detail = fmt.Sprintf("%s 等 %d 项", detail, len(problems))
		}
		return CapabilityItem{Name: "plugins", OK: false, Detail: detail,
			Hint: "在 .env 补齐缺失变量后到插件页重载（或重启服务）；不需要的插件可直接停用"}
	}
	return CapabilityItem{Name: "plugins", OK: true, Detail: "全部启用插件就绪"}
}

// capabilityWorkDir 进程默认工作目录可写性探针（建临时文件后即删）。
func (h *APIHandler) capabilityWorkDir() CapabilityItem {
	dir := h.defaultWorkDir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return CapabilityItem{Name: "workdir", OK: false, Detail: err.Error()}
		}
		dir = wd
	}
	probe := filepath.Join(dir, ".bma_capability_probe.tmp")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return CapabilityItem{Name: "workdir", OK: false, Detail: fmt.Sprintf("%s 不可写: %v", dir, err),
			Hint: "检查目录权限或 BMA_HOME 配置；工具产出/技能库/日志都依赖该目录可写"}
	}
	_ = os.Remove(probe)
	return CapabilityItem{Name: "workdir", OK: true, Detail: dir + " 可写"}
}
