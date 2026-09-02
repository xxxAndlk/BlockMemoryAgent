# 宿主机直控与可选工作目录 设计文档

- 日期:2026-09-02
- 状态:已与用户确认,待写实现计划
- 目标:让 Agent 具备类似 codex/qoder 桌面端的宿主机操作能力——TUI 以启动目录为工作目录、Web 端每会话可选工作目录、真机 GUI 控制(看真机屏幕/动真机键鼠),替代受限于 Docker 沙箱的 computer-use 玩法。

## 背景与现状

- 后端进程级单一 workDir:`bootstrap/bootstrap.go:196` 取 `os.Getwd()`,注入 `tool.NewBuiltinRegistry` / `agentSvc.SetWorkDir` / `plugins.WithWorkDir`。内置工具(RunCommand/ReadFile/WriteFile 等)直接在宿主机执行,沙箱检查(`sandbox.go` `isPathAllowed`)以该 workDir + `tool_sandbox_allowed_paths` 为界。
- TUI(`backend/cmd/tui/main.go`):工作目录已是 cwd 语义,但 5 个配置 flag 默认值(`config/config.yaml` 等)均相对 cwd——离开 BMA 仓库根目录即启动失败,实际被钉死在仓库根。
- Web(`backend/main.go` + `web/`):前端无工作目录概念,`createSession` 只发 `{goal, images}`;会话支持并行,但全部共享进程级 workDir。
- computer_use 插件(`config/plugins.yaml:81-117`):Docker Xvfb 虚拟桌面沙箱,AI 操作的不是真机;仅 `/workspace` 映射宿主工作目录;`@zavora-ai/computer-use-mcp` 官方本身支持 Windows 原生运行(stdio 备选配置已在注释中伏笔)。
- 安全现状:`tool_approval_disabled: true` 全信任模式(单人开发机),用户确认本期保持全信任。

## 已确认的决策

| 决策点 | 结论 |
|---|---|
| 能力范围 | 两者都要:任意工作目录文件/shell + 真机 GUI 控制 |
| Web 目录粒度 | 每会话独立目录,多会话可并行跨项目 |
| 安全/审批 | 保持全信任,不新增审批链 |
| 平台范围 | 先只做 Windows,预留平台抽象 |
| GUI 实现 | 方案 A:stdio 直起 `@zavora-ai/computer-use-mcp`(与沙箱同款工具面) |
| 安装形态 | install.ps1 安装脚本,引入安装目录 BMA_HOME |

## S1 · 安装目录 BMA_HOME + TUI 任意目录启动

### 安装目录布局(示例 `D:\data\bma`)

```
D:\data\bma\
├── bin\            # tui.exe、bma-server.exe
├── config\         # config.yaml / roles.yaml / plugins.yaml / skills.yaml / soul.md / user_profile.md
├── plugins.d\      # 外部插件包(bundle 扫描目录)
├── web\dist\       # 前端静态资源(server 托管)
├── logs\           # 日志(logging.dir 相对 BMA_HOME 解析)
└── .env
```

### install.ps1

- 提示输入安装目录 → 复制 binaries/config/skills/web 资源 → 写入用户级 `BMA_HOME` 环境变量(可选把 `bin` 加入 PATH)。
- 幂等:重复运行 = 升级;`bin/` 覆盖,`config/` 与 `.env` 已存在则不覆盖。
- 检测宿主机 node(S3 前置),缺失时告警不阻断。
- Makefile 增加 `make dist`:把构建产物按安装布局输出到 `dist/`,install.ps1 从 `dist/` 拷贝。

### 运行时解析顺序

TUI 与 server 共用新增 helper(如 `config.HomeDir()`):

1. `BMA_HOME` 环境变量(指向的目录须含 `config/config.yaml`,否则视为未设置并告警);
2. exe 所在目录及其上级(候选须含 `config/config.yaml`;`bin\` 布局时上级优先,开发态 `backend/tui.exe` 命中上级即仓库根);
3. 兼容现状的 cwd(含 `config/config.yaml` 才接受,否则启动报错)。

5 个现有 flag(`-config/-roles/-env/-soul/-skills`)仍可单独覆盖对应文件。`logging.dir`、`plugins.yaml`/`plugins.installed.yaml`/`plugins.d/`、skills 路径均相对 home 解析。

### 工作目录语义(不变)

- TUI:工作目录 = 启动目录(cwd);
- Web:每会话选择(S2),默认 = server 启动目录。

工作目录与安装目录从此完全解耦。

## S2 · Web 每会话工作目录(核心改造)

### 后端

1. **API**:`POST /api/sessions` 请求体增加可选 `work_dir`(绝对路径)。服务端校验:必须存在且为目录,否则 400;为空回落 server 启动目录(向后兼容,TUI 与旧客户端不受影响)。
2. **目录浏览 API**:新增 `GET /api/fs/browse?path=` 返回指定路径的子目录列表(只列目录不列文件);Windows 根层(空 path)返回盘符列表。供前端目录选择器使用。
3. **会话模型**:`server.Session` DTO 与运行时 `reactInternalSession` 增加 `WorkDir` 字段;新增迁移 `migrations/008_session_work_dir.sql`(session_history 加列),`restore_sessions` 恢复时带回。
4. **工具执行器按会话解析 workDir**:`tool.Executor` 的 workDir 单例改为按会话解析(ctx 已有会话标识,经 `tool.WithWorkDir(ctx)` 注入、`WorkDirFromContext(ctx)` 取出);相对路径 `resolvePath`、沙箱 `isPathAllowed`(界 = 该会话 workDir + 全局 `tool_sandbox_allowed_paths`)、会话临时目录 `.bma/tmp/<sid>` 全部改用该会话的 workDir。
5. **`.bma/` 状态目录跟随会话项目目录**:tmp / snapshots / shared / project_preferences 每个项目目录一份(per-project 语义,与 codex 一致);DB 全局记忆不受影响。
6. `production_workdir` 审批语义不变(全信任模式下本不触发)。

### 前端(web/)

- 新建会话对话框增加目录选择器:文本框直输路径 + 经 browse API 的浏览树;记住最近使用的目录。
- 会话列表项显示所属工作目录。

### 已知限制(本期不解决,写入文档)

- Docker 插件(ui_design / ui_preview / 沙箱 computer_use)的 `${WORKDIR}` 挂载在容器首次创建时钉死,无法跟随每会话目录——仍挂载 server 启动目录;每会话目录只对内置工具与宿主机插件生效。

## S3 · 真机 GUI 控制插件

### plugins.yaml 新增(纯配置,零新代码)

```yaml
host_computer_use:
  kind: mcp
  enabled: true
  settings:
    transport: stdio
    command: npx
    args: ["-y", "@zavora-ai/computer-use-mcp"]
    destructive: true
    roles: ["meta"]
    image_passthrough: true   # 截图回传多模态模型(与 ui_preview 同机制)
```

- 沙箱版 `computer_use` 保留但改 `enabled: false`,需要隔离环境时手动 enable。
- **两者不建议同时启用**(工具面高度重叠易混淆);实现时验证两插件工具名按插件 id 隔离、无冲突。
- 前置:宿主机 Node 20+(install.ps1 检测并告警)。

### 已知缺口(本期不做)

- 沙箱版自加的 `screen_record` 录屏工具依赖 X11(x11grab),宿主机 Windows 版暂不提供;后期可移植(ffmpeg gdigrab + 本地 stdio 代理)。

## 安全边界

- 本期保持全信任(`tool_approval_disabled: true`),不新增审批链;`host_computer_use` 以用户账号权限直操真机桌面,风险在文档与插件注释中明示。
- 内置工具的命令黑名单(hard block)与写路径沙箱不受影响,继续生效。
- Web 端已有 Token 鉴权(`http.auth_enabled`),browse API 挂在同一鉴权组下。

## 错误处理

- S1:home 解析失败 → 启动即报错并提示设置 `BMA_HOME` 或使用 flag。
- S2:`work_dir` 不存在/非目录/无权限 → 创建会话 400;运行中目录被删 → 工具执行报错回 Agent;browse API 对不存在路径返回错误,前端回落上级。
- S3:node/npx 缺失 → 插件 enable 失败,错误信息指向安装 Node 20+;锁屏/权限问题由 zavora `doctor` 工具诊断。

## 测试

- S1:Go 单测覆盖 home 解析顺序(env > exe 上级 > cwd);手工冒烟:从任意目录启动 `tui.exe`。
- S2:Go 单测(按会话 workdir 解析、沙箱逃逸按会话隔离、 browse API 含盘符枚举、会话恢复带回 workDir);前端手工冒烟(选目录建会话、跨目录并行两会话)。
- S3:冒烟——enable 后 `screenshot` 回传真机截图;对记事本做点击/输入验证键鼠。

## 实施顺序

S1 → S3 → S2(先小后大;S1/S3 互不依赖,S2 最大且独立)。
