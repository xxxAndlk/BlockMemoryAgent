# Web 端 UI 重设计 — 设计文档（精简版）

日期：2026-09-03
状态：预览图已经用户逐页确认（`doc/image/redesign/01–16`），本文档为其实现依据的定稿摘要。

## 背景

BlockMemoryAgent 现有 Web 端（Vue 3 + element-plus + tailwind）为深色单主题、平铺导航（11 个一级页面）、会话对话与监控分离两页。配合已上线的会话级 `work_dir`、`host_computer_use` 宿主直控插件、learned_skills 技能库等能力，按 codex/qoder 桌面端范式重设计整套 UI。TUI 不在本期范围（"看着办"）。

## 视觉规范（定稿）

- 浅色优先双主题（`html.dark` class 切换，localStorage `bma:theme` 持久化）。
- 主色靛蓝 `#4F5DFF`，主色浅底 `#EEF0FF`；页面底 `#F6F7F9`，卡片 `#FFFFFF`，边框 `#E5E7EB`。
- 圆角 10px，弱阴影 `0 1px 2px rgba(16,24,40,.05)`。
- 语义色：成功 `#16A34A` / 失败 `#DC2626` / 警告 `#D97706` / 停用 `#9CA3AF`。
- 深色主题对应：页面 `#0F1115`、卡片 `#1A1D24`、边框 `#2A2D35`、主色 `#6B77FF`。
- 路径 / 日志 / 代码一律等宽字体（ui-monospace 栈）。
- 实现方式：CSS 变量语义 token（`--bma-*`）+ tailwind 颜色映射变量，element-plus 用 `--el-*` 覆写跟随主题。

## 信息架构（定稿）

侧栏分 4 组 + 底部 1 预留项：

- 工作台：首页 `/dashboard`、会话 `/session`
- 项目：工作目录 `/projects`（吞并原「项目偏好」编辑器为右侧面板，原 `/project-prefs` 路由删除并重定向）
- 资源库：技能库 `/skills`、插件 `/plugins`、知识库 `/knowledge`
- 记忆：记忆中心 `/memory`、用户画像 `/profile`、人格配置 `/soul`、会话历史 `/history`
- 系统：系统设置 `/settings`
- 底部预留：工作流编排 `/workflow`（虚线框 + Beta 徽标，占位页）

原「项目偏好」独立导航项删除（用户明确："项目偏好导航栏似乎没有必要"）。

## 页面设计要点

### 01 首页 `/dashboard`
左列：创建会话卡（目标输入 + WorkDirPicker + 快捷模板 + Run）、会话列表（搜索 + 状态过滤 chip + 行内进度）。右列：统计概览（会话总数/完成率/LLM 调用/超时率 + Token 趋势折线）、最近活动。逻辑沿用现 dashboard，全面换浅色 token。

### 02/03 会话 `/session`（同一页两视图）
- 原 `/chat` 与 `/session`（监控）合并为单页 `/session`，标题旁分段切换器「💬 对话 | 📊 监控」（query `?view=chat|monitor`，缺省 chat）；`/chat` 重定向到 `/session`（保留 `?id=`）。
- 布局：左 280px 会话列表（沿用 chat 页）；中栏随视图切换（对话 = ChatHeader+MessageList+ChatInput；监控 = 执行日志/Skill 装配/文件预览/日志分析 四 Tab，沿用现监控中栏）；右栏 320px 统一 5 Tab：
  - 任务看板：角色层级树 + 任务列表 + 进度 + 约束（现 chat/session 右栏与左栏内容合并）
  - 工具：本会话工具调用流水（tool_call/tool_exec 事件，可展开 args/输出）
  - 文件：现 FilePreview 组件
  - 记忆：本会话沉淀（evolution_log 按 `source_session` 过滤 + 源自本会话的经验技能）
  - 指标：MetricsCard + TokenMetricsCard + MailboxCard + HealthCard
- 会话页接受 `?work_dir=` query：预填并固化工作目录（供工作目录页"发起新会话"跳转）。

### 04 工作目录 `/projects`
按 `work_dir` 聚合会话（listSessions 分组）+ localStorage `bma:workdirs` 手工添加目录。卡片：目录路径（等宽）、会话数、运行中数、最近活跃；操作：发起新会话（→ `/session?work_dir=`）、查看会话。右侧：选中目录的「项目偏好」编辑器（原 project-prefs 页逻辑，按目录参数加载）。WorkDirPicker 添加目录。

### 05 技能库 `/skills`
三 Tab：经验技能（现有 learned_skills 卡片+编辑对话框）/ 内置技能（新增，`GET /api/skills` 只读卡片：name/domain/description/tool_ref/cost/tags）/ 进化日志（现有时间线）。

### 06 插件 `/plugins`
卡片网格沿用；新增形态徽标约定（PluginInfo 无 transport 字段，按 id/kind 判定）：`host_computer_use`→「宿主直控」warning 徽标；`computer_use`→「Docker 沙箱」info 徽标；`kind==='service'`→「HTTP 服务」。保留启用开关、重载、missing_env/last_error 展示。

### 07/16 记忆中心 `/memory`
全局记忆聚合总览 + 项目筛选。**不使用** `/api/memory/search|levels|eval`（后端 ReAct 重构后均为空壳桩，返回 note "removed"）。
数据来源：`/api/skills/learned`、`/api/evolution/log`、`/api/profile`、`/api/project/preferences`、`/api/sessions`。
- 顶部：项目下拉（全部项目 + 会话去重 work_dir）+ 类型 chip（全部/用户偏好/项目经验/技能新建/技能更新）。
- 统计卡：经验技能总数 / 启用中 / 进化条目数 / 涉及会话数。
- 主体：左进化日志时间线（筛选后），右经验技能列表 + 用户画像摘要（→/profile）+ 项目偏好摘要（选中项目时，→/projects）。
- 项目归属映射：经 `source_session` → 会话 → `work_dir`；无 source_session 的条目属「全局」，仅"全部项目"时显示。

### 08 系统设置 `/settings`
外观（浅色/深色单选，useTheme）；服务信息（`/api/status`：program/mode/soul/llm_provider/llm_model）；连接健康（`/api/health`：postgres/redis/llm 在线与延迟）；资源目录说明卡（BMA_HOME 安装目录静态说明：config/、skills、plugins.d、workspace 等）。无配置写 API，全部只读 + 前端主题。

### 09 工作流编排 `/workflow`
FeaturePlaceholder 改造：SetUp 图标 + Beta 徽标 + 规划能力清单（可视化编排/节点依赖/运行观测），纯占位。

### 其余
- 用户画像 `/profile`：现有编辑器，仅换 token。
- 人格配置 `/soul`：当前人格（`/api/status`.soul）展示 + soul.md 编辑能力占位说明（无人格写 API）。
- 会话历史 `/history`：新表格页（listSessions：id/goal/状态/work_dir/起止时间/耗时，搜索 + 状态过滤，行点击 → `/session?id=&view=monitor`）。
- 知识库 `/knowledge`：占位页换 token。

## 约束与边界

- 不改后端；只消费现有 API。`/api/memory/*` 为空壳，禁止接入。
- 不引入新依赖；不引入前端测试框架；验证 = `cd web && npm run build`（vue-tsc 类型检查）+ 对照 `doc/image/redesign/*.png` 逐页视觉比对。
- 执行期间不做任何 git 提交/变异操作（用户既定要求）。
- 旧文件清理：`web/src/views/chat/`（迁移后）、`web/src/views/project-prefs/` 删除；`@/views/chat` 绝对引用全部改写。
