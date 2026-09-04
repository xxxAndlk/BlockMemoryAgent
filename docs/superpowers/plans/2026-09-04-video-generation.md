# 生视频功能(od_video_generate)实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 Agent 增加文生视频能力,复用 open_design 插件链,兼容火山方舟 tasks 与 OpenAI /videos 两种上游协议(v1 仅 t2v)。

**Architecture:** daemon 镜像与后端 Go 零改动。`docker/open-design-mcp/server.js` 新增 `od_video_generate`/`od_video_list` 工具,走 daemon 同一 `/media/generate` 端点(`surface:'video'`);`config/plugins.yaml` 增补 provider 槽位配置;daemon 侧 volcengine(目录 id + `OD_MEDIA_MODEL_ALIASES` 别名)与 aihubmix(`/videos` 协议,`aihubmix-` 前缀目录旁路)两 renderer 原生现成。

**Tech Stack:** Node 20(stdio MCP bridge,@modelcontextprotocol/sdk + zod)、Docker、YAML 配置。

**Spec:** `docs/superpowers/specs/2026-09-04-video-generation-design.md`

## Global Constraints

- daemon 镜像 `bma/open-design-daemon:local` **禁止改动/重建**;后端 Go 代码不动。
- 密钥只走 `${VAR}` / `${VAR:default}` 环境变量插值,任何文件不落盘明文密钥。
- stdout 只承载 MCP JSON-RPC 帧,桥内日志一律 `log()`(stderr)。
- 视频**不做**白底抠除(图片专属逻辑);v1 不做 i2v。
- 本仓库桥接层(docker/*-mcp)无 JS 测试框架,沿用项目冒烟惯例验证;**不新增测试框架**。
- 后端 API 基址:`http://localhost:10010`(config.yaml `addr: ${HTTP_ADDR:":10010"}`);若 `.env` 配了鉴权 token,curl 需带 `Authorization: Bearer <token>`。
- 桥镜像构建:`docker build -t bma/open-design-mcp:local docker/open-design-mcp`(FROM 走 daocloud 加速,勿改回 Docker Hub 直链)。
- 改动文件清单:`docker/open-design-mcp/server.js`、`config/plugins.yaml`、`.env.example`、`doc/变更.md`。

---

### Task 1: 桥重构预备——waitTask/newestImageFallback/listImages 参数化

**Files:**
- Modify: `docker/open-design-mcp/server.js`

**Interfaces:**
- Consumes: 现有 `odJson`、`OD_PROJECT_ID`、`POLL_INTERVAL_MS`、`OD_TIMEOUT_MS`、`IMAGE_EXTS`。
- Produces(后续任务依赖):
  - `waitTask(taskId, { timeoutMs?, label?, listTool? }?)` — label 默认 `'图像'`,listTool 默认 `'od_image_list'`,timeoutMs 默认 `OD_TIMEOUT_MS`。**签名从 `(taskId, startedAtMs)` 变更**(startedAtMs 原本就未被函数体使用)。
  - `newestImageFallback(sinceMs, exts = IMAGE_EXTS)` — 新增第二参,默认行为不变。
  - `listMediaFiles({ limit, exts, key })` — listImages 的泛化;`listImages({ limit })` 保留为薄封装,返回形状不变(`{project, count, images}`)。

- [ ] **Step 1: 改 waitTask 签名与文案参数化**

`docker/open-design-mcp/server.js` 中,把现有 `waitTask` 整个函数(当前在第 144–160 行)替换为:

```js
// waitTask 轮询任务直至 done/failed。label 用于错误文案("图像"/"视频"),
// listTool 为超时后引导查询的列表工具名。视频生成 30s–5min,超时由调用方传入。
async function waitTask(taskId, { timeoutMs = OD_TIMEOUT_MS, label = '图像', listTool = 'od_image_list' } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const data = await odJson(`/api/projects/${OD_PROJECT_ID}/media/tasks?includeDone=1`);
    const tasks = Array.isArray(data?.tasks) ? data.tasks : [];
    const task = tasks.find((t) => t?.taskId === taskId);
    if (task?.status === 'done') return task;
    if (task?.status === 'failed' || task?.status === 'interrupted') {
      const msg = task?.error?.message || JSON.stringify(task?.error || '未知错误');
      throw new Error(`${label}生成失败（${task.status}）：${msg}`);
    }
    if (Date.now() > deadline) {
      throw new Error(`等待生成超时（${timeoutMs}ms），taskId=${taskId}；可稍后调 ${listTool} 查看结果`);
    }
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
}
```

同时把 `generateImage` 里的调用 `const task = await waitTask(taskId, startedAtMs);` 改为:

```js
  const task = await waitTask(taskId);
```

- [ ] **Step 2: newestImageFallback 加扩展名参数**

把 `newestImageFallback(sinceMs)` 签名改为 `newestImageFallback(sinceMs, exts = IMAGE_EXTS)`,函数体内 `IMAGE_EXTS.some(...)` 一处改为 `exts.some(...)`,其余不动。改后完整函数:

```js
// newestImageFallback task.file 缺文件名时,取项目里 task 启动后最新修改的对应类型文件。
async function newestImageFallback(sinceMs, exts = IMAGE_EXTS) {
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const images = files
    .filter((f) => exts.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .filter((f) => !sinceMs || Number(f?.mtime || f?.updatedAt || f?.modifiedAt || 0) >= sinceMs - 5000)
    .sort((a, b) => Number(b?.mtime || b?.updatedAt || b?.modifiedAt || 0) - Number(a?.mtime || a?.updatedAt || a?.modifiedAt || 0));
  const first = images[0];
  return first ? String(first.name || first.fileName) : null;
}
```

- [ ] **Step 3: listImages 泛化为 listMediaFiles**

把现有 `listImages` 函数替换为下面三个函数(`listImages` 行为不变,新增 `listVideos` 供 Task 2 注册工具用):

```js
// listMediaFiles 列项目文件按扩展名过滤(图片/视频共用)。
async function listMediaFiles({ limit, exts, key }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  await ensureProject();
  const data = await odJson(`/api/projects/${OD_PROJECT_ID}/files`);
  const files = Array.isArray(data?.files) ? data.files : [];
  const items = files
    .filter((f) => exts.some((ext) => String(f?.name || f?.fileName || '').toLowerCase().endsWith(ext)))
    .map((f) => ({
      name: String(f.name || f.fileName),
      size: Number(f.size || f.bytes || 0),
      updatedAt: f.mtime || f.updatedAt || f.modifiedAt || null,
    }))
    .sort((a, b) => Number(b.updatedAt || 0) - Number(a.updatedAt || 0))
    .slice(0, limit || 20);
  return { project: OD_PROJECT_ID, count: items.length, [key]: items };
}

async function listImages({ limit }) {
  return listMediaFiles({ limit, exts: IMAGE_EXTS, key: 'images' });
}

async function listVideos({ limit }) {
  return listMediaFiles({ limit, exts: VIDEO_EXTS, key: 'videos' });
}
```

- [ ] **Step 4: 语法校验 + 常量占位**

`VIDEO_EXTS` 在 Step 3 被引用、Task 2 才定义——本步先在常量区(`const IMAGE_EXTS = ...` 之后)加:

```js
const VIDEO_EXTS = ['.mp4', '.webm', '.mov'];
```

运行:

```bash
node --check docker/open-design-mcp/server.js
```

预期:无输出(退出码 0)。

- [ ] **Step 5: Commit**

```bash
git add docker/open-design-mcp/server.js
git commit -m "refactor(open-design-mcp): 参数化 waitTask/listImages 预备视频工具"
```

---

### Task 2: 桥新增 od_video_generate / od_video_list

**Files:**
- Modify: `docker/open-design-mcp/server.js`

**Interfaces:**
- Consumes: Task 1 的 `waitTask(taskId, opts)`、`newestImageFallback(sinceMs, exts)`、`listVideos({ limit })`;现有 `resolveSaveAs`、`ensureProject`、`odJson`、`downloadFile`、`slugify`、`WORKSPACE_DIR`、`OUTPUT_DIR`、`OUTPUT_REL_PREFIX`、`OD_API_TOKEN`。
- Produces:
  - 环境变量:`OD_VIDEO_PROVIDER`(默认 `volcengine`)、`OD_VOLCENGINE_VIDEO_MODEL`(默认 `doubao-seedance-2-0-fast-260128`)、`OD_OPENAI_VIDEO_MODEL`(默认空)、`OD_VIDEO_TIMEOUT_MS`(默认 `720000`)。
  - `generateVideo({ prompt, aspect, duration_sec, provider, model, save_as })` → `{path, filename, bytes, provider, model, duration_sec, elapsedSec, hint}`。
  - MCP 工具 `od_video_generate`、`od_video_list`。

- [ ] **Step 1: 常量区加视频 env**

在 Task 1 加的 `VIDEO_EXTS` 之后追加:

```js
// ---- 视频生成(od_video_generate,2026-09-04)----
// 双协议兼容(设计 docs/superpowers/specs/2026-09-04-video-generation-design.md):
//   - volcengine:daemon 内置 renderVolcengineVideo(方舟 tasks 协议),模型须为目录 id
//     (doubao-seedance-*);wire 模型名可被 daemon 的 OD_MEDIA_MODEL_ALIASES 别名改写
//     (自定义接入点场景,env 在 open_design service 插件侧注入)。
//   - openai:daemon 内置 renderAIHubMixVideo 讲 OpenAI /videos 协议;模型 id 带
//     aihubmix- 前缀触发 daemon 目录旁路(任意模型名),用户给裸名时桥自动补前缀;
//     网关 baseUrl/key 由 daemon media-config providers.aihubmix 提供(桥侧不可见,
//     缺失时 daemon 任务 failed、错误原文透传)。
const OD_VIDEO_PROVIDER = (process.env.OD_VIDEO_PROVIDER || 'volcengine').toLowerCase();
const OD_VOLCENGINE_VIDEO_MODEL = process.env.OD_VOLCENGINE_VIDEO_MODEL || 'doubao-seedance-2-0-fast-260128';
const OD_OPENAI_VIDEO_MODEL = process.env.OD_OPENAI_VIDEO_MODEL || '';
// 视频 30s–5min(ark 队列峰值更久),远超图片的 180s 上限,独立超时。
const OD_VIDEO_TIMEOUT_MS = Number(process.env.OD_VIDEO_TIMEOUT_MS || 720000);
```

- [ ] **Step 2: provider/model 解析与 generateVideo**

在 `generateImageBatch` 之后、`listMediaFiles` 之前插入:

```js
function resolveVideoProvider(provider) {
  const p = String(provider || OD_VIDEO_PROVIDER).toLowerCase();
  if (p !== 'volcengine' && p !== 'openai') {
    throw new Error(`未知视频 provider：${p}（可选 volcengine / openai；对应网关协议分别为方舟 tasks 与 OpenAI /videos）`);
  }
  return p;
}

function resolveVideoModel(provider, model) {
  if (provider === 'openai') {
    const m = model || OD_OPENAI_VIDEO_MODEL;
    if (!m) {
      throw new Error('provider=openai 但未配置视频模型：.env 填 VIDEO_OPENAI_MODEL（你的 /videos 网关模型名，如 veo-3.1-fast）后 POST /api/plugins/reload');
    }
    return m.startsWith('aihubmix-') ? m : `aihubmix-${m}`;
  }
  return model || OD_VOLCENGINE_VIDEO_MODEL;
}

// generateVideo 文生视频(t2v):daemon media/generate surface='video',length 传时长秒数。
// 无白底抠除(图片专属);产物 mp4 经 downloadFile 落盘,路径语义与 generateImage 一致。
async function generateVideo({ prompt, aspect, duration_sec: durationSec, provider, model, save_as: saveAs }) {
  if (!OD_API_TOKEN) {
    throw new Error('未配置 OD_API_TOKEN（插件 settings.env 缺失）；请检查 .env 后重启后端');
  }
  // 先校验 save_as(fail fast:路径非法不必浪费一次生成)。
  const saveTarget = saveAs ? resolveSaveAs(saveAs) : null;
  const useProvider = resolveVideoProvider(provider);
  const useModel = resolveVideoModel(useProvider, model);
  const startedAtMs = Date.now();
  await ensureProject();
  const gen = await odJson(`/api/projects/${OD_PROJECT_ID}/media/generate`, {
    method: 'POST',
    body: {
      surface: 'video',
      model: useModel,
      prompt,
      ...(aspect ? { aspect } : {}),
      ...(durationSec ? { length: durationSec } : {}),
    },
  });
  const taskId = gen?.taskId;
  if (!taskId) throw new Error(`OD 未返回 taskId：${JSON.stringify(gen).slice(0, 300)}`);
  log('视频任务已受理:', taskId, 'provider=', useProvider, 'model=', useModel);
  const task = await waitTask(taskId, { timeoutMs: OD_VIDEO_TIMEOUT_MS, label: '视频', listTool: 'od_video_list' });
  const fileName = (await extractTaskFile(task)) || (await newestImageFallback(startedAtMs, VIDEO_EXTS));
  if (!fileName) throw new Error('任务完成但未找到产物文件名');
  const bytes = await downloadFile(fileName);
  const ext = (fileName.match(/\.[a-z0-9]+$/i)?.[0]) || '.mp4';
  let absPath;
  let relPath;
  if (saveTarget) {
    relPath = /\.[a-z0-9]+$/i.test(saveTarget.rel) ? saveTarget.rel : saveTarget.rel + ext;
    absPath = `${WORKSPACE_DIR}/${relPath}`;
  } else {
    const outName = `${new Date().toISOString().replace(/[:.]/g, '').slice(0, 15)}-${slugify(prompt)}${ext}`;
    relPath = `${OUTPUT_REL_PREFIX}/${outName}`;
    absPath = join(OUTPUT_DIR, outName);
  }
  mkdirSync(dirname(absPath), { recursive: true });
  writeFileSync(absPath, bytes);
  const elapsedSec = Math.round((Date.now() - startedAtMs) / 1000);
  log('视频已落盘:', relPath, `${bytes.length}B`, `${elapsedSec}s`);
  return {
    path: relPath,
    filename: relPath.split('/').pop(),
    bytes: bytes.length,
    provider: useProvider,
    model: useModel,
    duration_sec: durationSec || 5,
    elapsedSec,
    hint: '在 HTML 中以该相对路径（path 字段）用 <video src> 引用此视频',
  };
}
```

- [ ] **Step 3: 注册两个 MCP 工具**

在 `od_image_list` 的 `server.registerTool` 块之后、`await server.connect(...)` 之前插入:

```js
server.registerTool('od_video_generate', {
  title: 'Open Design 视频生成',
  description:
    '经本地 open_design 守护进程生成短视频（文生视频 t2v）。' +
    'provider 二选一：volcengine=方舟 tasks 协议网关（模型为 daemon 目录 id，自定义接入点由 daemon 的 OD_MEDIA_MODEL_ALIASES 别名改写）；' +
    'openai=OpenAI /videos 协议网关（模型名任意，需 .env 配 VIDEO_OPENAI_URL/VIDEO_OPENAI_MODEL/VIDEO_OPENAI_API_KEY）。' +
    '正式素材务必用 save_as 直落工作目录相对路径（如 assets/video/intro.mp4），一视频一份；缺省落 .bma/od-artifacts（时间戳命名，仅草稿）。' +
    '单条约 30 秒–5 分钟，请勿重复提交相同 prompt；超时（默认 720s）后可调 od_video_list 查看结果。',
  inputSchema: {
    prompt: z.string().min(1).describe('视频描述（建议具体描述主体/动作/镜头运动/风格）'),
    aspect: z.enum(['1:1', '16:9', '9:16', '4:3', '3:4']).optional().describe('画幅比例，缺省由模型决定'),
    duration_sec: z.number().int().min(2).max(15).optional().describe('时长秒数，缺省 5；daemon 自动 snap 到模型允许桶（Veo 4/6/8、Sora 4/8/12、Seedance 常用 5/10）'),
    provider: z.enum(['volcengine', 'openai']).optional().describe(`网关协议，缺省 ${OD_VIDEO_PROVIDER}（env OD_VIDEO_PROVIDER 可改）`),
    model: z.string().optional().describe('覆盖默认模型 id（openai 侧给裸模型名即可，桥自动补 aihubmix- 前缀；volcengine 侧须为 daemon 目录 id）'),
    save_as: z.string().optional().describe('产物落盘的工作目录相对路径（如 assets/video/intro.mp4）。目录自动创建；无扩展名时补实际格式；不可含 .. 或盘符'),
  },
}, async (args) => {
  try {
    const result = await generateVideo(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_video_generate 失败: ${err.message}` }], isError: true };
  }
});

server.registerTool('od_video_list', {
  title: 'Open Design 已生成视频列表',
  description: '列出 BMA 专用 OD 项目中已生成的视频文件（名称/大小/更新时间），用于复用历史产物。',
  inputSchema: {
    limit: z.number().int().min(1).max(100).optional().describe('返回条数上限，缺省 20'),
  },
}, async (args) => {
  try {
    const result = await listVideos(args);
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] };
  } catch (err) {
    return { content: [{ type: 'text', text: `od_video_list 失败: ${err.message}` }], isError: true };
  }
});
```

- [ ] **Step 4: 语法校验**

```bash
node --check docker/open-design-mcp/server.js
```

预期:无输出(退出码 0)。

- [ ] **Step 5: Commit**

```bash
git add docker/open-design-mcp/server.js
git commit -m "feat(open-design-mcp): 新增 od_video_generate/od_video_list 文生视频工具"
```

---

### Task 3: plugins.yaml 配置 + .env.example

**Files:**
- Modify: `config/plugins.yaml`
- Modify: `.env.example`

**Interfaces:**
- Consumes: Task 2 的桥侧 env 名(`OD_VIDEO_PROVIDER`/`OD_VOLCENGINE_VIDEO_MODEL`/`OD_OPENAI_VIDEO_MODEL`/`OD_VIDEO_TIMEOUT_MS`)。
- Produces: `.env` 可配项 `VIDEO_OPENAI_URL`/`VIDEO_OPENAI_MODEL`/`VIDEO_OPENAI_API_KEY`/`VIDEO_VOLC_URL`/`VIDEO_VOLC_MODEL`/`VIDEO_PROVIDER`/`OD_MEDIA_MODEL_ALIASES`/`OD_VIDEO_TIMEOUT_MS`(全部可选,不配则 openai 路径不可用、volcengine 走官方地址与默认模型)。

- [ ] **Step 1: open_design service 插件 env 增补**

`config/plugins.yaml` 中,`open_design.settings.env` 块(现有 `OD_API_TOKEN`/`ARK_API_KEY`/`OD_CUSTOM_IMAGE_API_KEY` 三行)内,`OD_CUSTOM_IMAGE_API_KEY: ${ARK_API_KEY:}` 行之后追加:

```yaml
        # 生视频(2026-09-04):OD_AIHUBMIX_API_KEY = OpenAI /videos 协议网关的 key
        #(daemon aihubmix provider 槽位);方舟 tasks 协议网关继续用 ARK_API_KEY。
        # OD_MEDIA_MODEL_ALIASES 可选:方舟侧自定义接入点 id 时填 JSON 映射,如
        # {"doubao-seedance-2-0-fast-260128":"ep-xxx"}(daemon 渲染前改写 wire 模型名)。
        OD_AIHUBMIX_API_KEY: ${VIDEO_OPENAI_API_KEY:}
        OD_MEDIA_MODEL_ALIASES: ${OD_MEDIA_MODEL_ALIASES:}
```

- [ ] **Step 2: open_design sync_files 加视频 provider 槽位**

同文件 `sync_files` 的 media-config.json `providers` 块(现有仅 `custom-image`)内,`custom-image` 条目之后追加:

```yaml
              # 生视频双协议槽位(2026-09-04):aihubmix = OpenAI /videos 协议网关
              #(任意模型名,目录旁路);volcengine = 方舟 tasks 协议网关,baseUrl
              # 缺省官方地址,仅自定义网关时需覆盖。空串键跳过(同 custom-image)。
              aihubmix:
                baseUrl: ${VIDEO_OPENAI_URL:}
                model: ${VIDEO_OPENAI_MODEL:}
              volcengine:
                baseUrl: ${VIDEO_VOLC_URL:}
```

- [ ] **Step 3: ui_design mcp 插件 env 增补 + 注释更新**

`ui_design.settings.env` 块内,`OD_OUTPUT_REL_PREFIX: .bma/od-artifacts` 行之后追加:

```yaml
        # 生视频(od_video_generate):provider 缺省 volcengine(方舟 tasks 协议);
        # openai = OpenAI /videos 协议网关(模型名裸填,桥自动补 aihubmix- 前缀)。
        # 视频单条 30s–5min,桥侧超时独立(默认 720s,图片仍 180s)。
        OD_VIDEO_PROVIDER: ${VIDEO_PROVIDER:}
        OD_VOLCENGINE_VIDEO_MODEL: ${VIDEO_VOLC_MODEL:}
        OD_OPENAI_VIDEO_MODEL: ${VIDEO_OPENAI_MODEL:}
        OD_VIDEO_TIMEOUT_MS: ${OD_VIDEO_TIMEOUT_MS:}
```

同插件头部注释块中"od_image_generate 生成图片(单张 prompt / 批量 prompts 数组桥内顺序串行)并落盘 volumes 映射的宿主目录(返回 workspace 相对路径供 HTML/CSS 引用),od_image_list 列历史产物。"一句之后追加一行注释:

```yaml
  # 2026-09-04 新增 od_video_generate(文生视频,双协议:方舟 tasks / OpenAI /videos)
  # 与 od_video_list;视频产物 mp4 同机制落盘,HTML <video> 引用。
```

- [ ] **Step 4: .env.example 补视频配置样例**

`.env.example` 末尾追加:

```bash
# ---- 生视频(od_video_generate,2026-09-04 新增;全部可选)----
# OpenAI /videos 协议网关(POST {URL}/videos 建任务 + 轮询;模型名任意):
# VIDEO_OPENAI_URL=https://your-gateway
# VIDEO_OPENAI_MODEL=veo-3.1-fast
# VIDEO_OPENAI_API_KEY=sk-xxx
# 方舟 tasks 协议网关(缺省官方地址与 doubao-seedance-2-0-fast-260128;自定义接入点用别名):
# VIDEO_VOLC_URL=https://ark.cn-beijing.volces.com/api/v3
# VIDEO_VOLC_MODEL=doubao-seedance-2-0-fast-260128
# OD_MEDIA_MODEL_ALIASES={"doubao-seedance-2-0-fast-260128":"你的接入点id"}
# 其他:
# VIDEO_PROVIDER=volcengine
# OD_VIDEO_TIMEOUT_MS=720000
```

- [ ] **Step 5: YAML 语法校验**

```bash
node -e "const y=require('js-yaml');" 2>/dev/null || true
python -c "import yaml,sys; yaml.safe_load(open('config/plugins.yaml',encoding='utf-8')); print('plugins.yaml OK')"
```

预期:输出 `plugins.yaml OK`(若 python 无 yaml 模块,改用后端 reload 实测,见 Task 4 Step 2)。

- [ ] **Step 6: Commit**

```bash
git add config/plugins.yaml .env.example
git commit -m "feat(plugins): open_design/ui_design 增生视频双协议配置槽"
```

---

### Task 4: 重建桥镜像 + reload + 可见性/回归验证

**Files:** 无(纯运维验证)

**Interfaces:**
- Consumes: Task 1–3 全部产物。
- Produces: 运行态确认工具可见、生图回归不坏。

- [ ] **Step 1: 重建桥镜像**

```bash
docker build -t bma/open-design-mcp:local docker/open-design-mcp
```

预期:构建成功,无 npm 报错。

- [ ] **Step 2: reload 插件**

```bash
curl -s -X POST http://localhost:10010/api/plugins/reload
```

预期:返回 2xx JSON;后端日志无 plugins.yaml 解析错误。

- [ ] **Step 3: 确认 daemon media-config 合并生效**

```bash
docker exec bma-plugin-svc-open_design cat /app/.od/media-config.json
```

预期:JSON 含 `providers.aihubmix` 与 `providers.volcengine` 键(`.env` 未配 VIDEO_OPENAI_URL 时 aihubmix 下 baseUrl/model 键按空串跳过,属正常)。

- [ ] **Step 4: 确认工具可见**

```bash
curl -s http://localhost:10010/api/plugins | python -c "import json,sys; d=json.load(sys.stdin); print([p for p in d if p.get('id')=='ui_design'])"
```

预期:ui_design 插件 `tools` 列表含 `od_image_generate`、`od_image_list`、`od_video_generate`、`od_video_list` 四项。

- [ ] **Step 5: 生图回归冒烟(防 Task 1 重构破坏现有行为)**

经 daemon 直发一条最小生图任务(与桥同参数形状,Host 头改写绕 isLocalSameOrigin):

```bash
TOKEN=$(grep -E '^OD_API_TOKEN=' .env | cut -d= -f2)
AUTH=$(printf 'open-design:%s' "$TOKEN" | base64 -w0)
curl -s -X POST http://localhost:7456/api/projects/bma-agent-artifacts/media/generate \
  -H "Host: localhost:7456" -H "Authorization: Basic $AUTH" -H "Content-Type: application/json" \
  -d '{"surface":"image","model":"custom-image","prompt":"一个红色圆形图标,纯白背景"}'
```

预期:返回 `{"taskId":"..."}`;随后 `curl -s "http://localhost:7456/api/projects/bma-agent-artifacts/media/tasks?includeDone=1" -H "Host: localhost:7456" -H "Authorization: Basic $AUTH"` 轮询到 `status:"done"`。

- [ ] **Step 6: Commit(若有修复)**

本任务纯验证,正常无提交;若验证暴露了 Task 1–3 的问题,修复后按对应任务的 commit 信息风格补提交。

---

### Task 5: 双 provider 真机冒烟(用户 .env 门禁)

**Files:** 无(依赖用户在 `.env` 填真实 URL/模型/key)

**Interfaces:**
- Consumes: Task 4 完成态 + 用户 `.env` 至少配通一条 provider(`VIDEO_VOLC_*`/`ARK_API_KEY` 或 `VIDEO_OPENAI_*` 三件套)。
- Produces: 两条协议各一条真实 mp4 产物落盘证据。

- [ ] **Step 1: 与用户确认 .env 已配哪条 provider;都没配则请用户至少配一条后 reload,再继续**

- [ ] **Step 2: volcengine 路径冒烟(若已配)**

```bash
TOKEN=$(grep -E '^OD_API_TOKEN=' .env | cut -d= -f2)
AUTH=$(printf 'open-design:%s' "$TOKEN" | base64 -w0)
curl -s -X POST http://localhost:7456/api/projects/bma-agent-artifacts/media/generate \
  -H "Host: localhost:7456" -H "Authorization: Basic $AUTH" -H "Content-Type: application/json" \
  -d '{"surface":"video","model":"doubao-seedance-2-0-fast-260128","prompt":"一只橘猫在窗台上伸懒腰,阳光斜照,固定镜头","aspect":"16:9","length":5}'
```

预期:返回 taskId;轮询 tasks 端点 5 分钟内 `done`;`ls workspace 下 .bma/od-artifacts/` 或经 od_video_list 语义确认 mp4 产物存在且可播放(`ffprobe` 或文件大小 > 100KB 粗判)。

- [ ] **Step 3: openai 路径冒烟(若已配)**

同上,body 改为:

```json
{"surface":"video","model":"aihubmix-<VIDEO_OPENAI_MODEL 值>","prompt":"一只橘猫在窗台上伸懒腰,阳光斜照,固定镜头","aspect":"16:9","length":5}
```

预期:同 Step 2;失败时读 task 的 error.message——`no AIHubMix API key` 说明 `VIDEO_OPENAI_API_KEY` 未注入,404/非 JSON 说明 `VIDEO_OPENAI_URL` 协议形态不符(回到设计文档"唯一接不了的形态"一节与用户核对)。

- [ ] **Step 4: 端到端走桥(可选但推荐)**:在 TUI 会话里让 Agent 调一次 `od_video_generate`(save_as 到临时路径),确认返回 `path` 字段且 HTML `<video>` 可引用。

---

### Task 6: doc/变更.md 条目

**Files:**
- Modify: `doc/变更.md`

**Interfaces:**
- Consumes: Task 1–5 实际结果。
- Produces: 项目惯例的溯源条目(任务 124,2026-09-04,插入在 `## 2026-08-28` 之前,文件按日期倒序;注意该文件为 CRLF 行尾)。

- [ ] **Step 1: 写入条目**

在 `---`(头部说明之后)与 `## 2026-08-28` 之间插入:

```markdown
## 2026-09-04

### 任务 124:生视频能力(od_video_generate 复用 open_design 插件链,双协议兼容)

- **人物**:Kimi
- **背景**:用户有生视频模型与 URL,要求生图插件链增加生视频,且火山方舟 tasks 与 OpenAI /videos 两种协议都兼容。实测 daemon(ghcr.io/nexu-io/od)原生支持 surface='video':renderVolcengineVideo(方舟 tasks,目录 id + OD_MEDIA_MODEL_ALIASES 别名改写 wire 模型名)与 renderAIHubMixVideo(OpenAI /videos,aihubmix- 前缀目录旁路)双 renderer 现成,故 daemon 镜像零改动,改动收敛在桥与配置。设计:docs/superpowers/specs/2026-09-04-video-generation-design.md。
- **落地**:
  - `docker/open-design-mcp/server.js`:waitTask 参数化(timeoutMs/label/listTool,startedAtMs 废参删除)、newestImageFallback 加扩展名参数、listImages 泛化 listMediaFiles;新增 od_video_generate(provider 二选一、aspect、duration_sec、save_as 直落、OD_VIDEO_TIMEOUT_MS 默认 720s 独立超时)与 od_video_list。
  - `config/plugins.yaml`:open_design env 增 OD_AIHUBMIX_API_KEY/OD_MEDIA_MODEL_ALIASES,sync_files media-config providers 增 aihubmix(OpenAI /videos 网关)与 volcengine(baseUrl 可覆盖)槽位;ui_design env 增 OD_VIDEO_PROVIDER/OD_VOLCENGINE_VIDEO_MODEL/OD_OPENAI_VIDEO_MODEL/OD_VIDEO_TIMEOUT_MS。
  - `.env.example` 补 VIDEO_OPENAI_*/VIDEO_VOLC_* 等可选样例。
- **测试**:node --check 通过;镜像重建 + /api/plugins/reload 后 ui_design 工具面 4 项可见;media-config.json 容器内确认合并;生图回归冒烟通过;双 provider 真机冒烟(按用户 .env 实际配置)结果:____。
- **遗留**:v1 仅 t2v;i2v 首帧(daemon 双 renderer 均支持 imageRef)、音频 surface、视频缩略图回显未做;协议不兼容网关(如 Gemini 原生 predictLongRunning)需前置聚合层归一化为 /videos。
```

- [ ] **Step 2: Commit**

```bash
git add doc/变更.md
git commit -m "docs: 变更.md 任务 124 生视频能力"
```

---

## Self-Review 记录

- **Spec 覆盖**:双协议(任务 2/3/5)、t2v only(全局约束)、save_as/默认落盘(任务 2)、独立超时(任务 2/3)、双 provider 配置槽(任务 3)、错误处理(任务 2 桥侧 fail-fast + daemon 透传,见下)、冒烟验证(任务 4/5)、变更.md(任务 6)——全覆盖。
- **与 spec 的一处偏差(有意)**:spec §错误处理写"未配 VIDEO_OPENAI_URL/VIDEO_OPENAI_API_KEY 桥侧 fail-fast";实际这两个值经 sync_files 只注入 daemon,桥侧不可见,无法提前校验——缺失时 daemon 任务立即 failed、错误原文(`no AIHubMix API key — ...set OD_AIHUBMIX_API_KEY`)经桥透传,配合工具描述中的 .env 指引,定位成本等同。桥侧 fail-fast 保留 OD_API_TOKEN 与 openai 模型名两项。
- **占位符扫描**:Task 5 Step 2/3 的 `<VIDEO_OPENAI_MODEL 值>` 为操作指引占位(执行时按用户 .env 实际值替换),Task 6 条目"结果:____"待冒烟后回填——均为运维占位,非代码占位。
- **类型一致**:`waitTask` 新签名 `(taskId, {timeoutMs,label,listTool})` 在 Task 1 定义、Task 2 generateVideo 使用一致;`newestImageFallback(startedAtMs, VIDEO_EXTS)` 与 Task 1 签名一致;`listVideos({limit})` Task 1 定义、Task 2 注册使用一致。
