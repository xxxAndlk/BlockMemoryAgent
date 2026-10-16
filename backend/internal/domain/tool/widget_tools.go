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
	"context"  // 工具执行上下文
	"fmt"      // 结果文案
	"path/filepath" // 回收站路径回执文案（Windows 反斜杠转正斜杠）
	"strings" // 文案拼接
	"time"    // updated_at 格式化

	"github.com/google/jsonschema-go/jsonschema" // 入参 schema

	"github.com/blockmemory/agent/backend/internal/widgets" // 部件目录包管理器
)

// widgetReloadPayload 是 widget_save 成功后推送的热刷新信号：前端卡片按结构化帧
// {type:"__widget_reload__"} 识别即重建 iframe（src 加时间戳绕缓存）。
const widgetReloadPayload = `{"__widget_reload__":true}`

// WidgetPushFunc 是部件数据推送回调（bootstrap 接 ReactService.PushWidgetData）。
type WidgetPushFunc func(sessionID, widgetID, payload string)

// WidgetOwnerResolver 是部件归属解析回调（bootstrap 接会话 owner 来源）：
// sessionID -> owner（user_id）。返回空串表示来源未接线，owner 字段留空（TODO #18-8）。
type WidgetOwnerResolver func(sessionID string) string

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
	if t, ok := r.tools["widget_push_data"].(*widgetPushDataTool); ok {
		t.mgr = mgr
		t.pushFn = pushFn
	}
	if t, ok := r.tools["widget_delete"].(*widgetDeleteTool); ok {
		t.mgr = mgr
	}
	if t, ok := r.tools["widget_restore"].(*widgetRestoreTool); ok {
		t.mgr = mgr
	}
}

// SetWidgetOwnerResolver 注入部件归属解析回调（bootstrap 装配后调用）；
// nil / 返回空串时 widget_save 落盘的 owner 字段留空。
func (r *Registry) SetWidgetOwnerResolver(fn WidgetOwnerResolver) {
	if r == nil {
		return
	}
	if t, ok := r.tools["widget_save"].(*widgetSaveTool); ok {
		t.ownerResolver = fn
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

// widgetSaveTool 实现 widget_save：保存/更新界面部件目录包（全量 files 或增量 upsert/delete）。
type widgetSaveTool struct {
	mgr           *widgets.Manager
	pushFn        WidgetPushFunc
	ownerResolver WidgetOwnerResolver
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
		"两种模式：全量 files=[{path, content}]（含入口 index.html，新建/整包重写）；或增量 upsert=[{path, content}] + delete=[path...]（与 files 互斥，只改列出的文件，其余保持原样，须为已有部件）。" +
		"可选 name/description/entry；更新会保留原启用状态，已打开的卡片自动热刷新。部件内 JS 通过 postMessage 接收宿主消息：" +
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
			"entry":       {Type: "string", Description: "渲染入口文件（可选，缺省 index.html / 增量模式沿用现有清单）"},
			"files": {
				Type:        "array",
				Description: "全量模式：部件文件列表（至少含入口文件；总大小 ≤1MB，≤32 个）。与 upsert/delete 互斥",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"path":    {Type: "string", Description: "相对部件目录的文件路径（如 index.html / app.js / css/a.css）"},
						"content": {Type: "string", Description: "文件内容"},
					},
					Required: []string{"path", "content"},
				},
			},
			"upsert": {
				Type:        "array",
				Description: "增量模式：覆盖/新增的文件列表（与 files 互斥；其余文件保持原样）",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"path":    {Type: "string", Description: "相对部件目录的文件路径"},
						"content": {Type: "string", Description: "文件内容"},
					},
					Required: []string{"path", "content"},
				},
			},
			"delete": {
				Type:        "array",
				Description: "增量模式：待删除的文件路径列表（不存在则报错）",
				Items:       &jsonschema.Schema{Type: "string"},
			},
		},
		Required: []string{"id"},
	}
}

// Execute 校验入参并保存部件；成功后向当前会话推热刷新信号。
// 校验类失败返回 validation_rejected（修正参数即可），写盘失败返回 execution_failed。
// 两种模式（互斥）：全量 files（新建/整包重写）与增量 upsert+delete（局部改不丢其余文件）。
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

	// 归属解析：会话上下文经 ownerResolver 取 owner（未接线留空，TODO #18-8）。
	var owner string
	if t.ownerResolver != nil {
		if sessionID := SessionIDFromContext(ctx); sessionID != "" {
			owner = t.ownerResolver(sessionID)
		}
	}
	mf := widgets.Manifest{
		Name:        strings.TrimSpace(strArg(args, "name")),
		Description: strings.TrimSpace(strArg(args, "description")),
		Entry:       strings.TrimSpace(strArg(args, "entry")),
		Owner:       owner,
	}

	// 增量模式：upsert / delete 任一出现即走增量（与 files 互斥）。
	rawUpsert, hasUpsert := args["upsert"].([]any)
	rawDelete, hasDelete := args["delete"]
	_, hasFiles := args["files"]
	if hasUpsert || hasDelete {
		if hasFiles {
			res.Error = "upsert/delete（增量）与 files（全量）互斥，二选一"
			res.Category = ResultCategoryValidationRejected
			return res
		}
		var upsert []widgets.FileInput
		var delPaths []string
		if hasUpsert {
			upsert = make([]widgets.FileInput, 0, len(rawUpsert))
			for i, raw := range rawUpsert {
				fm, ok := raw.(map[string]any)
				if !ok {
					res.Error = fmt.Sprintf("upsert[%d] 须为 {path, content} 对象", i)
					res.Category = ResultCategoryValidationRejected
					return res
				}
				path, _ := fm["path"].(string)
				content, _ := fm["content"].(string)
				if path == "" {
					res.Error = fmt.Sprintf("upsert[%d].path 为空", i)
					res.Category = ResultCategoryValidationRejected
					return res
				}
				upsert = append(upsert, widgets.FileInput{Path: path, Content: content})
			}
		}
		if dl, ok := rawDelete.([]any); ok {
			for _, p := range dl {
				s, _ := p.(string)
				delPaths = append(delPaths, s)
			}
		}
		info, err := t.mgr.SaveIncremental(id, mf, upsert, delPaths)
		if err != nil {
			res.Error = err.Error()
			res.Category = ResultCategoryValidationRejected
			return res
		}
		t.pushReload(ctx, id)
		res.Success = true
		res.Output = t.saveOutput(info, "增量更新")
		return res
	}

	// 全量模式（原语义）：files 逐个解析（JSON 反序列化进 map 的元素类型是 map[string]any）。
	rawFiles, ok := args["files"].([]any)
	if !ok || len(rawFiles) == 0 {
		res.Error = "files 必填且至少一个文件（含入口 index.html）；或用 upsert/delete 增量更新"
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

	info, err := t.mgr.Save(id, mf, files)
	if err != nil {
		res.Error = err.Error()
		res.Category = ResultCategoryValidationRejected
		return res
	}

	t.pushReload(ctx, id)
	res.Success = true
	res.Output = t.saveOutput(info, "已保存")
	return res
}

// pushReload 向当前会话推热刷新信号；无会话上下文（测试/程序化调用）或推送通道
// 未接线时跳过，不影响保存本身。
func (t *widgetSaveTool) pushReload(ctx context.Context, id string) {
	if t.pushFn != nil {
		if sessionID := SessionIDFromContext(ctx); sessionID != "" {
			t.pushFn(sessionID, id, widgetReloadPayload)
		}
	}
}

// saveOutput 保存/增量更新成功回执（带 ShowArtifact 调用示例）。
func (t *widgetSaveTool) saveOutput(info *widgets.Info, verb string) string {
	return fmt.Sprintf("部件 %s %s（%d 个文件，入口 %s，%s）。展示给用户：ShowArtifact(kind=widget, widget_id=%q, title=卡片标题)；"+
		"之后改代码再调本工具即可（局部改用 upsert/delete 增量模式），已打开的卡片会自动热刷新。",
		info.ID, verb, len(info.Files), info.Entry, enabledLabelCN(info.Enabled), info.ID)
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

// ---- widget_push_data ----

// widgetPushDataTool 实现 widget_push_data：向当前会话的部件卡片推送瞬时数据。
type widgetPushDataTool struct {
	mgr    *widgets.Manager
	pushFn WidgetPushFunc
}

// Name 返回工具名称。
func (t *widgetPushDataTool) Name() string { return "widget_push_data" }

// Aliases 返回别名列表（无别名）。
func (t *widgetPushDataTool) Aliases() []string { return nil }

// Description 返回工具功能描述。
func (t *widgetPushDataTool) Description() string {
	return "向当前会话已展示的界面部件卡片推送最新数据（payload 为 JSON 字符串，≤64KB，超限截断；" +
		"部件内 postMessage 收到 {type:'data', payload}）。用于二维码/行情/轮询类卡片的数据刷新，" +
		"只推最新值不落历史。部件须已用 widget_save 创建；无会话上下文（非对话内调用）时不可用。"
}

// InputSchema 返回入参 JSON Schema。
func (t *widgetPushDataTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"widget_id": {Type: "string", Description: "目标部件 id（widget_list 可查）"},
			"payload":   {Type: "string", Description: "数据 JSON 字符串（≤64KB，超限截断）"},
		},
		Required: []string{"widget_id", "payload"},
	}
}

// Execute 校验部件存在后经 pushFn 推送（64KB 截断在 PushWidgetData 内完成）。
func (t *widgetPushDataTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "widget_push_data"}
	if t.mgr == nil || t.pushFn == nil {
		res.Error = "widget_push_data 未接线（服务未注入部件管理面/数据通道）"
		return res
	}
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		res.Error = "widget_push_data 需要会话上下文（当前会话内调用）；请改为在任务规格中说明数据，由顶层 Agent 在对话内推送"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	widgetID := strings.TrimSpace(strArg(args, "widget_id"))
	payload, _ := args["payload"].(string)
	if widgetID == "" {
		res.Error = "widget_id is required"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if info, ok := t.mgr.Get(widgetID); !ok {
		res.Error = fmt.Sprintf("部件不存在: %s（widget_list 可查已有部件，先用 widget_save 创建）", widgetID)
		res.Category = ResultCategoryValidationRejected
		return res
	} else if !info.Enabled {
		res.Error = fmt.Sprintf("部件 %s 已停用（管理页或 POST /api/widgets/%s/enable 可启用）", widgetID, widgetID)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if payload == "" {
		res.Error = "payload 必填（JSON 字符串，可为 \"{}\"）"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	t.pushFn(sessionID, widgetID, payload)
	res.Success = true
	res.Output = fmt.Sprintf("已向部件 %s 推送数据（%d 字节，超 64KB 截断）。", widgetID, len(payload))
	return res
}

// ---- widget_delete ----

// widgetDeleteTool 实现 widget_delete：软删部件（移入回收站，可恢复）。
type widgetDeleteTool struct {
	mgr *widgets.Manager
}

// Name 返回工具名称。
func (t *widgetDeleteTool) Name() string { return "widget_delete" }

// Aliases 返回别名列表（无别名）。
func (t *widgetDeleteTool) Aliases() []string { return nil }

// Description 返回工具功能描述。
func (t *widgetDeleteTool) Description() string {
	return "删除一个界面部件（软删：目录移入回收站 widgets/.trash/<id>-del-<ts>/，不物理删除）。" +
		"误删用 widget_restore 恢复。部件被删除后其卡片不再可展示/推送。"
}

// InputSchema 返回入参 JSON Schema。
func (t *widgetDeleteTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"id": {Type: "string", Description: "待删除的部件 id"},
		},
		Required: []string{"id"},
	}
}

// Execute 软删部件，回执带回收站条目名与恢复方式。
func (t *widgetDeleteTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "widget_delete"}
	if t.mgr == nil {
		res.Error = "widget_delete 未接线（服务未注入部件管理面）"
		return res
	}
	id := strings.TrimSpace(strArg(args, "id"))
	if id == "" {
		res.Error = "id is required"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	entry, err := t.mgr.Delete(id)
	if err != nil {
		res.Error = err.Error()
		res.Category = ResultCategoryValidationRejected
		return res
	}
	res.Success = true
	if entry == "" {
		res.Output = fmt.Sprintf("部件 %s 不存在（幂等，无需删除）。", id)
		return res
	}
	res.Output = fmt.Sprintf("部件 %s 已移入回收站 %s（未物理删除）。误删可调 widget_restore{name=%q} 恢复；管理页回收站列表同理。",
		id, filepath.ToSlash(filepath.Join(".trash", entry)), entry)
	return res
}

// ---- widget_restore ----

// widgetRestoreTool 实现 widget_restore：从回收站恢复部件。
type widgetRestoreTool struct {
	mgr *widgets.Manager
}

// Name 返回工具名称。
func (t *widgetRestoreTool) Name() string { return "widget_restore" }

// Aliases 返回别名列表（无别名）。
func (t *widgetRestoreTool) Aliases() []string { return nil }

// Description 返回工具功能描述。
func (t *widgetRestoreTool) Description() string {
	return "从回收站恢复界面部件（widget_delete 软删的 <id>-del-<ts> 条目，或覆写前自动快照 <id>-snap-<ts> 回滚到旧版本）。" +
		"不带 name 时列出回收站全部条目。恢复同名部件时现存版本会先移入回收站，不会覆盖丢失。"
}

// InputSchema 返回入参 JSON Schema。
func (t *widgetRestoreTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string", Description: "回收站条目名（widget_restore 不带 name 可列出全部条目）"},
		},
	}
}

// Execute 恢复回收站条目；不带 name 时列出全部条目。
func (t *widgetRestoreTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "widget_restore"}
	if t.mgr == nil {
		res.Error = "widget_restore 未接线（服务未注入部件管理面）"
		return res
	}
	name := strings.TrimSpace(strArg(args, "name"))
	if name == "" {
		list := t.mgr.TrashList()
		if len(list) == 0 {
			res.Success = true
			res.Output = "回收站为空。"
			return res
		}
		var b strings.Builder
		fmt.Fprintf(&b, "回收站共 %d 条（widget_restore{name} 恢复）：\n", len(list))
		for _, e := range list {
			fmt.Fprintf(&b, "- %s（部件 %s，%s，%d 文件，%s）\n",
				e.Name, e.WidgetID, e.Kind, len(e.Files), e.DeletedAt.Format(time.DateTime))
		}
		res.Success = true
		res.Output = b.String()
		return res
	}
	info, err := t.mgr.Restore(name)
	if err != nil {
		res.Error = err.Error()
		res.Category = ResultCategoryValidationRejected
		return res
	}
	res.Success = true
	res.Output = fmt.Sprintf("已恢复部件 %s（%d 个文件，入口 %s，%s）。展示给用户：ShowArtifact(kind=widget, widget_id=%q)。",
		info.ID, len(info.Files), info.Entry, enabledLabelCN(info.Enabled), info.ID)
	return res
}
