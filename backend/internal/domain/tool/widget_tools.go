package tool

// widget_tools.go 实现界面部件工具组（Widget 机制 Agent 自写闭环）：
//   - widget_save  保存/更新一个界面部件（写 widgets/<id>/ 目录包 + 清单）；
//   - widget_list  列出全部部件（id/name/description/enabled/updated_at）。
//
// 定位：部件是"给用户加界面"的 Agent 自留地（微信二维码实时画布、定时新闻美化
// 卡片）——Agent 写好渲染代码后用 ShowArtifact(kind=widget) 拿给用户看，
// 后续数据更新走 widget_data 瞬时推送通道，不重建卡片。
//
// 依赖注入：widgets 包不反向依赖 tool，工具直接持有 *widgets.Manager 窄接口
//（bootstrap 装配，SetWidgetManager 注入）；未接线时返回未接线错误
//（与 plugin_*/schedule_task 同款语义）。
//
// 热刷新：widget_save 成功后经注入的 pushFn 向当前会话推 {"__reload__":true}，
// 已打开的卡片收到即原地重建 iframe——Agent 改代码用户端即时生效，无需重发工件。

import (
	"context" // 工具执行上下文
	"fmt"     // 结果文案
	"strings" // 文案拼接
	"time"    // updated_at 格式化

	"github.com/google/jsonschema-go/jsonschema" // 入参 schema

	"github.com/blockmemory/agent/backend/internal/widgets" // 部件目录包管理器
)

// widgetReloadPayload 是 widget_save 成功后推送的热刷新信号：前端卡片识别
// __reload__ 标记即重建 iframe（src 加时间戳绕缓存）。
const widgetReloadPayload = `{"__reload__":true}`

// WidgetPushFunc 是部件数据推送回调（bootstrap 接 ReactService.PushWidgetData）。
type WidgetPushFunc func(sessionID, widgetID, payload string)

// SetWidgetManager 注入部件管理面与数据推送回调（bootstrap 装配后调用）。
// mgr 为 nil 时 widget_* 工具返回未接线错误；pushFn 为 nil 时保存后不推热刷新
//（数据通道未装配的精简场景，保存本身仍可用）。
func (r *Registry) SetWidgetManager(mgr *widgets.Manager, pushFn WidgetPushFunc) {
	if r == nil {
		return
	}
	// 同步注入各工具实例（与 SetPluginManager 同款模式：工具构造时 mgr 为 nil）。
	if t, ok := r.tools["widget_save"].(*widgetSaveTool); ok {
		t.mgr = mgr
		t.pushFn = pushFn
	}
	if t, ok := r.tools["widget_list"].(*widgetListTool); ok {
		t.mgr = mgr
	}
}

// SetWidgetLookup 注入 ShowArtifact kind=widget 的部件校验闭包（bootstrap 接
// widgets.Manager.Get）；nil 时 kind=widget 一律拒绝并提示未接线。
func (r *Registry) SetWidgetLookup(fn func(id string) (name string, enabled bool, ok bool)) {
	if r == nil {
		return
	}
	if t, ok := r.tools["ShowArtifact"].(*showArtifactTool); ok {
		t.widgetLookup = fn
	}
}

// ---- widget_save ----

// widgetSaveTool 实现 widget_save：保存/更新界面部件目录包。
type widgetSaveTool struct {
	mgr    *widgets.Manager
	pushFn WidgetPushFunc
}

// Name 返回工具名称。
func (t *widgetSaveTool) Name() string { return "widget_save" }

// Aliases 返回别名列表（无别名）。
func (t *widgetSaveTool) Aliases() []string { return nil }

// Description 返回工具功能描述（注入 LLM function calling schema）。
func (t *widgetSaveTool) Description() string {
	return "创建/更新一个界面部件：把自写的渲染代码（HTML/JS/CSS）保存成 widgets/<id>/ 目录包，" +
		"之后用 ShowArtifact(kind=widget, widget_id=<id>) 拿给用户看（对话栏渲染成沙箱 iframe 卡片）。" +
		"适用：需要实时刷新或定制渲染的展示（二维码画布、轮询数据卡片、美化报表）。" +
		"参数：id（小写字母/数字/连字符）、files=[{path, content}]（含入口 index.html）、可选 name/description/entry；" +
		"更新会保留原启用状态，已打开的卡片自动热刷新。部件内 JS 通过 postMessage 接收宿主消息：" +
		"{type:'init'|'data', payload:'...'}（payload 是字符串，自行 JSON.parse）；向上报借用 " +
		"parent.postMessage({type:'error', message}, '*')。"
}

// InputSchema 返回入参 JSON Schema。
func (t *widgetSaveTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"id": {
				Type:        "string",
				Description: "部件 id（[a-z0-9][a-z0-9-]{0,63}，目录名）",
			},
			"name":        {Type: "string", Description: "展示名（可选，缺省用 id）"},
			"description": {Type: "string", Description: "一句话说明（可选）"},
			"entry":       {Type: "string", Description: "渲染入口文件（可选，缺省 index.html，必须在 files 中）"},
			"files": {
				Type:        "array",
				Description: "部件文件列表（至少含入口文件；总大小 ≤1MB，≤32 个）",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"path":    {Type: "string", Description: "相对部件目录的文件路径（如 index.html / app.js / css/a.css）"},
						"content": {Type: "string", Description: "文件内容"},
					},
					Required: []string{"path", "content"},
				},
			},
		},
		Required: []string{"id", "files"},
	}
}

// Execute 校验入参并保存部件；成功后向当前会话推热刷新信号。
// 校验类失败返回 validation_rejected（修正参数即可），写盘失败返回 execution_failed。
func (t *widgetSaveTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "widget_save"}
	if t.mgr == nil {
		res.Error = "widget_save 未接线（服务未注入部件管理面）"
		return res
	}
	id := strings.TrimSpace(strArg(args, "id"))
	if id == "" {
		res.Error = "id is required"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	// files 逐个解析（JSON 反序列化进 map 的元素类型是 map[string]any）。
	rawFiles, ok := args["files"].([]any)
	if !ok || len(rawFiles) == 0 {
		res.Error = "files 必填且至少一个文件（含入口 index.html）"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	files := make([]widgets.FileInput, 0, len(rawFiles))
	for i, raw := range rawFiles {
		fm, ok := raw.(map[string]any)
		if !ok {
			res.Error = fmt.Sprintf("files[%d] 须为 {path, content} 对象", i)
			res.Category = ResultCategoryValidationRejected
			return res
		}
		path, _ := fm["path"].(string)
		content, _ := fm["content"].(string)
		if path == "" {
			res.Error = fmt.Sprintf("files[%d].path 为空", i)
			res.Category = ResultCategoryValidationRejected
			return res
		}
		files = append(files, widgets.FileInput{Path: path, Content: content})
	}

	info, err := t.mgr.Save(id, widgets.Manifest{
		Name:        strings.TrimSpace(strArg(args, "name")),
		Description: strings.TrimSpace(strArg(args, "description")),
		Entry:       strings.TrimSpace(strArg(args, "entry")),
	}, files)
	if err != nil {
		res.Error = err.Error()
		res.Category = ResultCategoryValidationRejected
		return res
	}

	// 热刷新：当前会话里已打开的该部件卡片收到 __reload__ 即原地重建 iframe。
	// 无会话上下文（测试/程序化调用）或推送通道未接线时跳过，不影响保存本身。
	if t.pushFn != nil {
		if sessionID := SessionIDFromContext(ctx); sessionID != "" {
			t.pushFn(sessionID, id, widgetReloadPayload)
		}
	}

	res.Success = true
	res.Output = fmt.Sprintf("部件 %s 已保存（%d 个文件，入口 %s，%s）。展示给用户：ShowArtifact(kind=widget, widget_id=%q, title=卡片标题)；"+
		"之后改代码再调本工具即可，已打开的卡片会自动热刷新。",
		info.ID, len(info.Files), info.Entry, enabledLabelCN(info.Enabled), info.ID)
	return res
}

// enabledLabelCN 启停状态的中文回执文案。
func enabledLabelCN(enabled bool) string {
	if enabled {
		return "已启用"
	}
	return "未启用，需启用后才能展示"
}

// ---- widget_list ----

// widgetListTool 实现 widget_list：列出全部界面部件。
type widgetListTool struct {
	mgr *widgets.Manager
}

// Name 返回工具名称。
func (t *widgetListTool) Name() string { return "widget_list" }

// Aliases 返回别名列表（无别名）。
func (t *widgetListTool) Aliases() []string { return nil }

// Description 返回工具功能描述。
func (t *widgetListTool) Description() string {
	return "列出全部界面部件（id/名称/描述/启用状态/更新时间/文件数）。展示部件前先用它确认部件存在与启用状态；空列表说明还没用 widget_save 创建过部件。"
}

// InputSchema 返回入参 JSON Schema（无参数）。
func (t *widgetListTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
}

// Execute 列出全部部件信息。
func (t *widgetListTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "widget_list"}
	if t.mgr == nil {
		res.Error = "widget_list 未接线（服务未注入部件管理面）"
		return res
	}
	list := t.mgr.List()
	if len(list) == 0 {
		res.Success = true
		res.Output = "暂无界面部件。用 widget_save 创建：{id, files:[{path:\"index.html\", content:\"...\"}]}。"
		return res
	}
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个界面部件：\n", len(list))
	for _, info := range list {
		fmt.Fprintf(&b, "- %s（%s）%s entry=%s files=%d updated_at=%s",
			info.ID, info.Name, enabledLabelCN(info.Enabled), info.Entry, len(info.Files),
			info.UpdatedAt.Format(time.DateTime))
		if info.Description != "" {
			fmt.Fprintf(&b, " — %s", info.Description)
		}
		b.WriteString("\n")
	}
	res.Success = true
	res.Output = b.String()
	return res
}
