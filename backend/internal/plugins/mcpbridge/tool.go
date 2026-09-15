package mcpbridge

import (
	"context"        // 工具执行上下文
	"encoding/json"  // schema 转换
	"strings"        // 描述后缀拼接

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// remoteTool 把单个 MCP 远端工具包装为领域层 tool.Tool + SchemaSource：
//   - 本地注册名统一带 "<插件ID>__" 前缀（如 ui_preview__browser_find），跨插件
//     从根上保证不重名——此前裸名注册时 host_computer_use 与 ui_preview 撞名
//     browser_find/browser_tabs，全有或全无注册 × 不确定启用顺序会随机死一方；
//   - Execute 内用远端原始名发 CallTool（服务端只认原名），结果转 *tool.Result（设计文档 §3.2）；
//   - Description/InputSchema 透传服务端定义，使 Registry.Schema() 兜底路径
//     能生成带完整入参 schema 的 blades 工具定义；
//   - Destructive 按插件配置标记（computer_use 等敏感插件全工具接入审批链）。
type remoteTool struct {
	name        string // 本地注册名（带插件前缀，全局唯一）
	remoteName  string // MCP 服务端原始工具名（CallTool 请求用）
	description string
	schema      *jsonschema.Schema
	destructive bool
	bridge      *Bridge
	timeout     int64 // 未使用占位，超时统一由 Bridge 配置控制
}

// Name 实现 tool.Tool。
func (t *remoteTool) Name() string { return t.name }

// Aliases 实现 tool.Tool：MCP 工具无别名
//（刻意不把远端原名注册为别名——两个插件撞名时别名映射会静默覆盖，重引入不确定性）。
func (t *remoteTool) Aliases() []string { return nil }

// Execute 实现 tool.Tool：以远端原始名转发 CallTool 并转换结果；
// Result.Tool 回填本地注册名，与 LLM 侧工具调用名保持一致。
func (t *remoteTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	res := t.bridge.callTool(ctx, t.remoteName, args)
	res.Tool = t.name
	return res
}

// Description 实现 SchemaSource。
func (t *remoteTool) Description() string { return t.description }

// InputSchema 实现 SchemaSource：透传服务端入参 schema（可能为 nil）。
func (t *remoteTool) InputSchema() *jsonschema.Schema { return t.schema }

// Destructive 按插件配置标记敏感工具（接入 Registry 审批守卫链）。
func (t *remoteTool) Destructive() bool { return t.destructive }

// wrapTool 将远端 MCP 工具包装为本地 tool.Tool。
// 本地名 = b.id + "__" + 远端名（插件前缀防跨插件撞名）；远端原名保留在 remoteName。
func (b *Bridge) wrapTool(mt *mcp.Tool) tool.Tool {
	desc := mt.Description
	if desc == "" {
		desc = "MCP 远端工具 " + mt.Name
	}
	if b.settings.ToolDescriptionSuffix != "" {
		desc = strings.TrimSpace(desc) + " " + b.settings.ToolDescriptionSuffix
	}
	var schema *jsonschema.Schema
	if mt.InputSchema != nil {
		// MCP 服务端 schema 是任意 JSON；尽量无损转成 jsonschema.Schema
		//（provider 侧 schemaToMap 序列化回 map 后交给 LLM），失败退化为空对象 schema。
		if raw, err := json.Marshal(mt.InputSchema); err == nil {
			var s jsonschema.Schema
			if err := json.Unmarshal(raw, &s); err == nil {
				schema = &s
			}
		}
	}
	return &remoteTool{
		name:        b.id + "__" + mt.Name,
		remoteName:  mt.Name,
		description: desc,
		schema:      schema,
		destructive: b.settings.Destructive,
		bridge:      b,
	}
}
