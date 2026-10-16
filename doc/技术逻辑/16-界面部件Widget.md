# 16 界面部件 Widget

> 只从代码还原。代码：`backend/internal/widgets/`（store 501 + 测试）、`backend/internal/server/widgets_http.go`、`backend/internal/domain/tool/widget_tools.go`、agent 侧 widgetData 瞬时通道、`web/src/components/WidgetFrame.vue` + `web/src/utils/widgetBus.ts` + `web/src/views/widgets/`。
> 定位（2026-10-13）：Agent 自写"界面渲染代码"的注册与运行容器——**插件是"给 Agent 加工具"，部件是"给用户加界面"**。典型场景：微信扫码二维码画布（到期自动推新码原地重绘，不整卡重渲染）、定时任务新闻的美化展示卡。

## 目录

- [16.1 四条链路总览](#161-四条链路总览)
- [16.2 widgets.Manager（目录包与热载）](#162-widgetsmanager目录包与热载)
- [16.3 ShowArtifact kind=widget（按标识调用）](#163-showartifact-kindwidget按标识调用)
- [16.4 瞬时数据通道 widget_data](#164-瞬时数据通道-widget_data)
- [16.5 错误回报闭环](#165-错误回报闭环)
- [16.6 Agent 自写工具 widget_save / widget_list](#166-agent-自写工具-widget_save--widget_list)
- [16.7 前端渲染 WidgetFrame 与 postMessage 协议](#167-前端渲染-widgetframe-与-postmessage-协议)
- [16.8 管理页与 API](#168-管理页与-api)
- [16.9 边界与坑](#169-边界与坑)
- [16.10 软删回收站·版本快照·owner·action 上行（2026-10 扩展）](#1610-软删回收站版本快照owneraction-上行2026-10-扩展)

---

## 16.1 四条链路总览

1. **注册/发现**：`widgets/<id>/` 目录包（与 `plugins.d/`、`schedules/` 平级，config.yaml 同目录推导），`widget.json` 清单即唯一事实源，mtime 增量热载，启停可手改文件热生效。
2. **调用**：`ShowArtifact kind=widget`（参数 `widget_id` + 可选 `data`）→ 工件引用 `path="widget://<id>"` 随 detail_json 落对话栏 → ArtifactCard 整卡渲染 WidgetFrame。
3. **实时数据**：**瞬时** widget_data 通道（不落库、最新值覆盖）——`POST /api/widgets/push`（bridge 等进程插件）或 `widget_save` 成功后的 `__reload__` → 会话内存态 → SSE 推帧 → widgetBus → postMessage → iframe 原地更新。
4. **错误回报**：iframe 内 JS 错误 → postMessage 给宿主 → 卡片错误条 + `POST /api/widgets/report` → System 会话事件（用户可见）+ 运行中会话邮箱注入"【部件报错】"（Agent 可见，可自行 widget_save 修代码）。

安全边界：iframe 一律 `sandbox="allow-scripts"`（**不给** allow-same-origin，与 ArtifactCard HTML 预览同口径），拿不到主应用 cookie/localStorage/父窗口 DOM；文件服务端 nosniff/no-referrer/双重越界校验；数据进出只走 postMessage。

## 16.2 widgets.Manager（目录包与热载）

`backend/internal/widgets/store.go`：

- **目录形态**：`widgets/<id>/widget.json` + Agent 自写渲染文件。清单 `{name, version?, description?, entry, enabled}`；**id=目录名不落盘**，正则 `^[a-z0-9][a-z0-9-]{0,63}$`；entry 缺省 `index.html`。
- **热载**：读路径（List/Get）按 `(missing, mtime, size)` 基线增量重读（直接仿 `internal/dag/filestore.go` 的 refreshLocked 模式）——手改 widget.json 即生效，无需 reload 端点（但仍提供 `POST /api/widgets/reload` 对齐插件页习惯）。
- **Save 校验**：文件路径拒绝绝对路径/`..`/隐藏文件名；文件数 ≤32、总大小 ≤1MB、清单本身 ≤64KB；entry 必须在文件列表中。写**临时目录 + 备份换名原子替换**，读侧看不到写了一半的目录；更新保留原 enabled/version。
- **SetEnabled**：临时文件 + rename 原子改写清单 enabled 字段（插件页/部件页开关等价于手改文件）。
- `Info` 线型：`{id, name, version, description, entry, enabled, updated_at(=清单 mtime), files[]}`。

## 16.3 ShowArtifact kind=widget（按标识调用）

`backend/internal/domain/tool/show_artifact.go` 的第五个 kind（独立分支，不走文件校验）：

- 参数：`widget_id`（必填）、`data`（初始数据 JSON 字符串，≤64KB）、`title`/`caption`；不校验扩展名、不要求工作区文件。
- 校验：经 Registry 注入的 `widgetLookup`（bootstrap 接 `widgets.Manager.Get`）判定部件**存在且启用**；未接线/不存在/停用均 validation_rejected 并说明。
- 产出 `Artifact{Kind:"widget", Path:"widget://<id>", Title, Caption, Data}`（`result.go` 的 Artifact 加 `Data` 字段），随 `mergeArtifactsJSON` 通用管线进 detail_json——历史回放同样出卡。
- prompts（leaf_common/domain_agent）ShowArtifact 指引段各补一句 widget 用法。

## 16.4 瞬时数据通道 widget_data

**刻意不落库、不进事件流**：二维码这类数据是瞬态的，落事件会污染时间线并在回放时推送过期内容。

- **会话内存态**：`reactInternalSession.widgetData map[widget_id]{seq, payload}`（session_react.go，store 锁与 setStreamingText 同款）；`ReactService.PushWidgetData(sessionID, widgetID, payload)`——会话不在内存**静默跳过**，payload 64KB 截断。
- **快照线型**：`Session.WidgetData map[string]string`（widget_id→最新 payload，`json:"widget_data,omitempty"`，ToServerSession 透传，不持久化）。
- **SSE 推帧**（stream_http.go）：150ms 轮询按 widget_id 比对 payload，变化即推 `{"type":"widget_data","widget_id":...,"payload":...}`；重连首帧快照带全量 map 天然补断档。
- **推送源**：`POST /api/widgets/push {session_id, widget_id, payload}` → 恒 204（会话不存在也静默——bridge 轮询场景不报错刷屏）。bridge 等 process 插件经 bma-plugin.json `env` 注入 `BMA_API_BASE`/`BMA_API_TOKEN` 走 /api 鉴权调用。

## 16.5 错误回报闭环

`POST /api/widgets/report {session_id, widget_id, error}` → `ReactService.ReportWidgetError`：

- 落 System 会话事件（对话流可见"部件 xxx 报错"）；
- 会话处于 running/awaiting_child/paused_on_child 时经 messenger 向 MetaAgent 邮箱注入"【部件报错】…（可修 widgets/<id>/ 后自动热载）"——Agent 下一轮看到可自行 widget_save 修代码，形成"写代码→出错→自己修"闭环；**awaiting_clarify 不含**（主循环阻塞在用户答复，注入无人即时消费）；messenger 未接线降级只落事件。
- 前端侧去重：同一条错误文本不重复 report（WidgetFrame `lastReported`）。

## 16.6 Agent 自写工具 widget_save / widget_list

`backend/internal/domain/tool/widget_tools.go`（registerDefaults 注册在 ShowArtifact 旁）。**挂载口径（2026-10-14 更新）**：三档顶层统一可用——cluster=meta 走角色授权（`domain/role/registry.go:188` 兜底 + `config/roles.yaml` meta_agent.tools），daily=domain 与 fast=doc_assistant 经 `mountTopLevelEssentials` 内建顶层必备清单预挂（role 列表未动，见 05-工具系统 §5.12）；**子 Agent 默认不给**——与"工具/插件分配权归顶层"一致：

- `widget_save{id, name?, description?, entry?, files:[{path, content}]}` → Manager.Save；成功且 ctx 带 sessionID 时向本会话推 `{"__widget_reload__":true}` 结构化热刷新帧——已打开的部件卡收到即热刷新 iframe（改代码无需用户手动刷新）。**增量模式**：`upsert:[{path,content}] + delete:[path...]`（与 files 互斥）只改列出文件、其余保持，局部修改不必整包重发。
- `widget_list`：列 id/name/description/enabled/updated_at。
- `widget_push_data{widget_id, payload}`：校验部件存在且启用后经 PushWidgetData 推瞬时数据（64KB 截断复用通道逻辑）；会话上下文缺失/部件不存在 → validation_rejected。
- `widget_delete{id}` / `widget_restore{name?}`：软删进回收站（回执带路径与恢复方式）/ 恢复（不带 name 列回收站条目）。
- **挂载与授权（2026-10-16 扩展）**：五件套（save/list/push_data/delete/restore）进 topLevelBuiltinTools 三档顶层预挂 + meta 角色授权（registry.go 兜底 + config/roles.yaml）+ **domain 角色天花板**——子 Agent 可经派发级 tools 授权拿到 widget 工具组（此前子 Agent 完全无部件工具，须回告顶层代办）。工具 schema 钉死数 35→38（registry_test.go 断言同步）。

## 16.7 前端渲染 WidgetFrame 与 postMessage 协议

`web/src/components/WidgetFrame.vue`（ArtifactCard 顶部分支：`kind==='widget' && path.startsWith('widget://')` → **整卡替换**，头部自带标题/新窗口/刷新，避免双标题行）：

- 挂载 `getWidget(id)`：不存在 → "不存在或已被删除"、`enabled=false` → "已停用"提示卡（均**不挂 iframe**）。
- iframe：`src=/api/widgets/<id>/files/<entry>`（逐段编码），`sandbox="allow-scripts"`、`referrerpolicy="no-referrer"`，默认高 420px。
- **postMessage 协议**（message 事件校验 `e.source === iframe.contentWindow`——沙箱为不透明源，无法按 origin 过滤；targetOrigin 只能 `'*'`）：
  - 宿主→部件：`{type:'init', payload}`（iframe load 或收到 ready 后发，payload=ShowArtifact 的 data，JSON.parse 失败给原文）；`{type:'data', payload}`（widgetBus 有更新时发，同样 tryParse）。
  - 部件→宿主：`{type:'ready'}`（触发补发 init）/ `{type:'error', message}`（错误条 + report）/ `{type:'resize', height}`（钳 120–2000px）。
- **热刷新**：payload 文本含 `__reload__` 或手动"刷新" → 重建 iframe（src 加 `?v=Date.now()`，`:key` 绑 src 整棵销毁重建）。
- **widgetBus**（`utils/widgetBus.ts`，~30 行零依赖）：模块级 `Map<widgetId, Ref<string>>`；`session/index.vue` 的 onEvent 拦截 `type==='widget_data'` 帧 → `setWidgetData` 后 return（**不进 events 时间线**），onSnapshot 逐项应用 `snap.widget_data`（重连补数据）。

## 16.8 管理页与 API

- 页面 `web/src/views/widgets/index.vue`（仿插件页）：卡片（名称/版本徽标/描述/ID/entry/文件数/updated_at/enabled 开关）+ 重载按钮 + 空态文案；路由 `/widgets`，侧边栏"资源库"组"界面部件"入口。
- API（全在 /api 鉴权组，handler 在 `server/widgets_http.go`）：

| 路由 | 说明 |
|---|---|
| `GET /api/widgets` / `GET /api/widgets/:id` | 列表 / 详情（Info 线型） |
| `POST /api/widgets/:id/enable` / `disable` | 热启停（原子改写 widget.json） |
| `POST /api/widgets/reload` | 手动重扫目录 |
| `GET /api/widgets/:id/files/*path` | 部件文件服务：防护完全照搬 workspace_http.go（Clean/IsAbs/`..` 双重校验、nosniff、no-referrer、ServeContent 流式） |
| `POST /api/widgets/push` | `{session_id, widget_id, payload}` → 204（静默） |
| `POST /api/widgets/report` | `{session_id, widget_id, error}` → 204（静默） |
| `POST /api/widgets/action` | `{session_id, widget_id, action, payload}` → 204（静默）：部件内用户交互上行（见 16.10） |
| `DELETE /api/widgets/:id` | 软删：目录移入 `widgets/.trash/<id>-del-<ts>/`，不存在幂等 `{deleted:false}` |
| `GET /api/widgets/trash` | 回收站条目列表（del=软删 / snap=覆写前快照） |
| `POST /api/widgets/trash/:name/restore` | 恢复条目（现存同名部件先入回收站不覆盖丢失） |

## 16.9 边界与坑

1. **widget_data 是瞬时态**：不落库、不持久化——重启/换会话即无；部件必须能容忍"打开时只有 init 数据"，后续数据等推送。
2. **热刷新信号已结构化**（2026-10-16）：`widget_save` 推 `{"__widget_reload__":true}` JSON 帧（WidgetFrame 按 `type` 识别）；旧子串包含匹配由前端兼容层负责过渡，业务 payload 别用这个词。
3. **push/report/action 恒 204**：调用方拿不到"会话不存在/部件不存在"信号——bridge 侧别拿 204 当成功投递凭证。
4. **部件文件服务对停用部件仍可读**（前端只在启用时渲染 iframe；管理页预览可用）——要严格化需在 files handler 加 enabled 校验。
5. **postMessage targetOrigin 只能是 `'*'`**（沙箱不透明源）：部件只收数据不收密钥，宿主侧不往部件发敏感信息。
6. **同一张卡多实例/同 id 多卡**：widgetBus 按 widget_id 分发，同 id 的多张卡都会收到同一份数据（当前语义为共享）。
7. **widget 工具组挂三档顶层 + 子 Agent 可授权**（2026-10-16：save/list/push_data/delete/restore 五件套，meta 角色授权 + domain 天花板 + `mountTopLevelEssentials` 预挂；子 Agent 经派发级 tools 授权使用）。
8. **明确不做**：部件间通信、部件主动调后端任意 API、FileViewer 全屏层集成。（版本历史/回滚已由 16.10 快照机制落地。）

## 16.10 软删回收站·版本快照·owner·action 上行（2026-10 扩展）

`backend/internal/widgets/store.go` + `server/widgets_http.go` + `domain/tool/widget_tools.go`（命名约定：回收站条目 `<id>-del-<ts>` / `<id>-snap-<ts>`，目录 `widgets/.trash/`）：

- **软删回收站**：`Manager.Delete(id)` 从物理 RemoveAll 改为把部件目录移入 `widgets/.trash/<id>-del-<ts>/`（`.trash` 隐藏前缀不进 List，Windows rename 不覆盖目录的既有处理保持）；缓存同步失效；不存在幂等（空条目名）。`TrashList()` 列条目 `{name, widget_id, kind:del|snap, deleted_at, files[]}`（只认 `-del-/-snap-` 形态，按时间倒序）；`Restore(name)` 还原——del 条目还原部件、snap 条目即回滚到覆写前版本；恢复时现存同名部件**先入回收站再换名**，不覆盖丢失。
- **版本快照**：`Save` 覆写已存在部件时旧目录整包快照进 `.trash/<id>-snap-<ts>/`（快照失败降级删除不阻断保存）——误改可回滚，替代"版本历史"诉求。
- **owner 归属（TODO #18-8 预留）**：`Manifest`/`Info` 加 `owner` 字段（omitempty，留空不透出）；`Registry.SetWidgetOwnerResolver(fn(sessionID))` 由 bootstrap 注入（运行态会话内存暂无 user_id，现返回空串，owner 来源接线后替换 TODO 标记）；`widget_save` 经 ctx sessionID 解析填充，更新未带 owner 保留原值，SetEnabled 原子改写清单时同步透传。
- **增量保存**：`Manager.SaveIncremental(id, mf, upsert, deletePaths)`；工具 `widget_save` 入参加 `upsert:[{path,content}] + delete:[path...]`（与 files 互斥）——只写列出文件、其余磁盘文件保持原样，局部修改不必整包重发；沿用"先快照再整目录替换"原子写骨架；entry 校验同款（删入口必须给新 entry）；保留 enabled/owner。
- **action 上行**：`POST /api/widgets/action {session_id, widget_id, action, payload}` → `ReactService.ReportWidgetAction`（仿 ReportWidgetError）：落"【部件操作】"System 会话事件 + 运行中会话邮箱注入（Agent 可感知卡片内交互）；会话不在内存静默 204，缺字段 400，未接线 501。
- **文件服务 CSP 加固**：全部部件文件响应带 `Content-Security-Policy: default-src 'self' 'unsafe-inline' data: blob:`；html/htm/svg 活动文档（可被顶层导航直接打开）叠加 `; sandbox allow-scripts`（禁表单提交/顶层导航/弹窗），与既有 nosniff/no-referrer 叠加生效。
- **子 Agent 授权**：五件套（save/list/push_data/delete/restore）进 meta 角色授权、domain 角色天花板（registry.go）与 topLevelBuiltinTools，子 Agent 可经派发级 tools 授权使用（见 16.6）。

