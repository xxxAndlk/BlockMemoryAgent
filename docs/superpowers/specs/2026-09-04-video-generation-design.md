# 生视频功能设计(方案 A:复用 open_design 插件链)

日期:2026-09-04
状态:已获用户批准(架构/工具接口/配置/错误处理四节均确认)

## 背景与目标

现有生图链 = `open_design` 守护进程(ghcr.io/nexu-io/od 本地补丁镜像)+ `ui_design`
MCP 桥(`docker/open-design-mcp`,工具 od_image_generate / od_image_list)。

目标:给 Agent 增加**文生视频**能力,兼容用户已有的两类上游网关:

1. 火山方舟 tasks API 形态(`POST /contents/generations/tasks` + 轮询);
2. OpenAI `/videos` 形态(`POST /videos` + `GET /videos/{id}` 轮询 + `/content` 下载)。

约束:daemon 镜像零改动;密钥只走 `.env` 环境变量插值,不落盘明文;v1 仅 t2v。

## 上游能力依据(实测 ghcr.io/nexu-io/od:latest,2026-09-04)

- daemon 原生支持 `surface: 'video'`(`media/index.js` 分发器)。
- `renderVolcengineVideo`:`POST {baseUrl}/contents/generations/tasks` → 轮询 →
  从临时 `video_url` 拉字节落项目目录。`baseUrl` 可经 media-config
  `providers.volcengine.baseUrl` 覆盖;apiKey 取 `ARK_API_KEY` 等 env。
  模型名须为目录 id(`doubao-seedance-2-0-260128` / `doubao-seedance-2-0-fast-260128` /
  `doubao-seedance-1-0-pro-250528`),但 `OD_MEDIA_MODEL_ALIASES`(JSON 映射 env,
  上游 issue #1277 机制)在渲染前把目录 id 改写成任意 wire 模型名,
  故自定义接入点 id 也接得住。
- `renderAIHubMixVideo`:OpenAI `/videos` 协议。model id 以 `aihubmix-` 前缀
  **自动旁路模型目录**(任意模型名);media-config `providers.aihubmix.model`
  可覆盖 wire 模型名;`baseUrl`/`apiKey` 均可经 media-config/env 注入;
  Veo(4/6/8s)、Sora(4/8/12s)、Wan(5/10s)时长分桶内置自动 snap。
- 媒体路由:`POST /api/projects/:id/media/generate`,body `{surface, model, prompt, aspect, length}`;
  轮询 `GET /api/projects/:id/media/tasks?includeDone=1`;产物下载
  `GET /api/projects/:id/files/{name}`。均有 `isLocalSameOrigin` 校验,桥已改写 Host 头。
- **唯一接不了的形态**:协议本身不兼容(如 Gemini 官方原生 `predictLongRunning`),
  daemon 无对应 renderer。解法:前置聚合网关归一化为 `/videos`(用户现有
  `GEMINI_IMAGE_URL` 网关以 OpenAI 兼容形态暴露 imagen,大概率同网关直接暴露 Veo
  `/videos`,即落在第 2 类零改动)。

## 架构与数据流

daemon 镜像与后端 Go 代码零改动。改动仅两处:

- `docker/open-design-mcp/server.js`(桥):新增 `od_video_generate` / `od_video_list`
- `config/plugins.yaml`:`open_design` / `ui_design` 两组 settings 增补

```
Agent → od_video_generate(桥)
      → POST /api/projects/bma-agent-artifacts/media/generate
        {surface:'video', model, prompt, aspect, length}
      → daemon 按 model 分发:
          doubao-seedance-* → renderVolcengineVideo(建任务+轮询+拉mp4)
          aihubmix-*       → renderAIHubMixVideo(POST /videos+轮询+/content下载)
      → 桥轮询 media/tasks 拿产物文件名 → 下载 mp4 → 写 ${WORKDIR} 映射目录
      → 返回宿主相对路径(HTML <video> 可直接引用)
```

纯转接:真实生成请求由本机 daemon 容器直连用户配置的 URL,不经任何第三方。
落地后重建 `bma/open-design-mcp:local` 镜像,经 `/api/plugins/reload` 热生效;
sync_files 改动在 daemon 健康就绪后自动深合并进容器 media-config.json。

## 桥改动(docker/open-design-mcp/server.js)

### od_video_generate

参数:

| 参数 | 必填 | 说明 |
|---|---|---|
| `prompt` | 是 | 视频描述(主体/动作/镜头/风格) |
| `aspect` | 否 | `1:1/16:9/9:16/4:3/3:4`,映射网关 ratio/size |
| `duration_sec` | 否 | 秒,缺省 5;daemon 侧自动 snap 到模型允许桶 |
| `provider` | 否 | `volcengine` / `openai`,缺省 env `OD_VIDEO_PROVIDER`(再缺省 `volcengine`) |
| `model` | 否 | 覆盖默认模型 id(高级用法) |
| `save_as` | 否 | 工作目录相对路径(如 `assets/video/intro.mp4`);缺省落 `.bma/od-artifacts` 时间戳命名。复用 resolveSaveAs 防穿越校验,先校验再发起生成 |

provider → model 解析:

- `volcengine` → 缺省 `OD_VOLCENGINE_VIDEO_MODEL`,再缺省 `doubao-seedance-2-0-fast-260128`
- `openai` → 缺省 `OD_OPENAI_VIDEO_MODEL`(如 `veo-3.1-fast`);桥自动补 `aihubmix-`
  前缀(目录旁路要求),已带前缀不重复补

行为:`ensureProject` → POST media/generate `{surface:'video', model, prompt, aspect, length}`
→ 轮询任务(复用 waitTask,**超时参数化**:视频用 `OD_VIDEO_TIMEOUT_MS` 缺省 720000,
图片仍用 `OD_TIMEOUT_MS` 180s,互不影响)→ `extractTaskFile`,失败时视频版
fallback(`VIDEO_EXTS = ['.mp4','.webm','.mov']` 过滤最新产物)→ `downloadFile`
→ 写 save_as 或缺省目录 → 返回
`{path, filename, bytes, provider, model, duration_sec, elapsedSec, hint}`。

视频**不做白底抠除**(图片专属逻辑)。

### od_video_list

复用 listImages 逻辑,过滤 `VIDEO_EXTS`,返回 `{project, count, videos}`。

### 工具描述要点

注明:单条约 30s–5min,勿重复提交相同 prompt;正式素材务必 save_as 直落目标路径;
provider 选择语义(火山=方舟 tasks 协议网关,openai=OpenAI /videos 协议网关)。

## 配置改动(config/plugins.yaml)

### open_design.settings(service 插件)

- `env` 增:
  - `OD_AIHUBMIX_API_KEY: ${VIDEO_OPENAI_API_KEY:}`
  - `OD_MEDIA_MODEL_ALIASES: ${OD_MEDIA_MODEL_ALIASES:}`(可选,方舟网关自定义模型名时填
    如 `{"doubao-seedance-2-0-fast-260128":"我的接入点id"}`)
- `sync_files` 的 media-config.json `providers` 增:
  - `aihubmix: {baseUrl: ${VIDEO_OPENAI_URL:}, model: ${VIDEO_OPENAI_MODEL:}}`
  - `volcengine: {baseUrl: ${VIDEO_VOLC_URL:}}`
  - 空串键跳过(沿用现有 custom-image 同款行为)

### ui_design.settings(mcp 插件)

- `env` 增:`OD_VIDEO_PROVIDER`、`OD_VOLCENGINE_VIDEO_MODEL`、`OD_OPENAI_VIDEO_MODEL`、
  `OD_VIDEO_TIMEOUT_MS`(均可选,有缺省)
- `exec_timeout_sec: 1200` 已覆盖视频轮询上限,不动;roles 沿用 `["meta","domain","ui_assistant"]`
- 注释块补两行视频工具说明(保持注释与实现一致)

### .env(用户填写,不入库)

- `VIDEO_OPENAI_URL` / `VIDEO_OPENAI_MODEL` / `VIDEO_OPENAI_API_KEY`(OpenAI /videos 网关)
- `VIDEO_VOLC_URL`(可选,方舟兼容网关;缺省 daemon 内置官方地址)
- `OD_MEDIA_MODEL_ALIASES`(可选,见上)

## 错误处理

- 未配 `OD_API_TOKEN`:报"检查 .env 后重启后端"(现有同款文案)
- 所选 provider 缺 env(openai 缺 `VIDEO_OPENAI_URL` 或 `VIDEO_OPENAI_API_KEY`):
  fail-fast 中文指引,不发起生成
- daemon 任务 `failed/interrupted`:透传 daemon error.message
- 轮询超时(720s):提示 taskId 与"稍后调 od_video_list 查看结果"(与图片超时文案同款)
- 协议填错槽(方舟 URL 填进 openai 槽等):daemon 返回非 2xx/非 JSON,截断透传原文

## 验证(桥无单测框架,沿用项目冒烟惯例)

1. `docker build -t bma/open-design-mcp:local docker/open-design-mcp`
2. `POST /api/plugins/reload` 后 tool_catalog 可见 `od_video_generate` / `od_video_list`
3. 两条 provider 各跑一条 5s t2v 冒烟:确认 mp4 落盘、返回相对路径、HTML `<video>` 可引用
4. 失败路径抽查:未配 env 文案、非法 save_as 拒止、超时文案
5. 按项目惯例在 `doc/变更.md` 补变更条目

## 明确不做(v1,YAGNI)

- i2v 首帧图生视频(daemon 两条 renderer 均支持 imageRef,后续需要再加)
- 音频 surface、独立 video 插件、daemon 补丁、视频缩略图回显
