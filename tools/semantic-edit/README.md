# 语义编辑 worker（tools/semantic-edit）

Harness 根治计划 WP4 §8.3 的实现：把 Vue SFC / CSS / TS / JS 的结构化定位做成独立
worker 进程，**只返回最小 source-range TextEdit 与诊断，从不写用户文件**。写盘由 Go
侧（`backend/internal/domain/edit`）在合同授权与检查点之后执行。

## 为什么单独一个进程

- 语言编译器（`@vue/compiler-sfc`、`postcss`、`typescript`）是 Node 生态，不能塞进 Go 二进制；
- 独立进程可以做超时、崩溃重启、输出上限；主进程即使 worker 崩溃也不会损坏用户文件；
- 依赖版本在这里**显式固定**（`package.json` 的精确版本 + `package-lock.json`），
  不依赖 web 项目的传递依赖——web 的 Vue 升级不应该悄悄改变编辑行为。

## 协议

行分隔 JSON（stdin 一行请求，stdout 一行响应）。每条请求带 `request_id`，响应回显它。

```
{"request_id":"r1","method":"plan_css_edit","params":{...}}
{"request_id":"r1","ok":true,"result":{...}}
{"request_id":"r1","ok":false,"error":{"code":-32006,"message":"target_ambiguous: 2 ..."}}
```

| method | 作用 |
|---|---|
| `ping` | 版本/语言/`writes_files:false` |
| `parse_sfc` | SFC 块范围 + 解析诊断 |
| `find_css_declaration` | 定位 selector 下唯一 property 声明（`.vue` 自动取 `<style>` 块并加基址） |
| `plan_css_edit` | 定位 + 生成最小 TextEdit（只替换声明，含结尾分号） |
| `find_template_node` | 按 tag + 锚点/文本定位模板节点（同 tag 嵌套用计数配对） |
| `plan_template_edit` | 定位 + 生成整段替换的 TextEdit |
| `find_ts_symbol` | TypeScript AST 定位声明 + 语法诊断（不做类型检查，避免依赖 tsconfig） |

错误码：`-32700` JSON 解析、`-32601` 未知方法、`-32602` 参数缺失、
`-32001` 依赖缺失、`-32002` 读文件失败、`-32003` CSS 解析失败、`-32004`
`expected_matches != 1`（禁止批量替换）、`-32005` 目标不存在、`-32006` 目标歧义、
`-32007` 指纹不符、`-32008` 响应超限、`-32010/-32011` 缺 template/style 块。

## 硬约束（代码里逐条落地）

- **不写文件**：worker 只 `readFileSync` 与 stdout；没有任何 `writeFile`/`unlink`。
- **不 eval、不动态 import 用户模块**：依赖是静态 import 的白名单三项。
- **不猜**：目标 0 个 / 多个、`expected_matches != 1`、指纹不符一律报错返回，
  绝不"自动全部替换"，也绝不静默回退正则。
- **只返回最小编辑**：CSS 只替换声明段（含分号）、模板只替换命中节点，
  未修改区域 byte-for-byte 不变（`worker.test.mjs` 断言）。

## 用法

```bash
cd tools/semantic-edit
npm ci                 # 需要 npm 源；离线安装包应预置 node_modules
node --test            # 15 条行为测试
node worker.mjs        # 常驻 stdio 服务（Go 侧用这个）
node worker.mjs --once '{"request_id":"p","method":"ping"}'
```

Go 侧配置（`config/config.yaml` 的 `agent.semantic_worker_path`）指向 `worker.mjs`；
桌面版应把本目录的 `node_modules` 一并打进安装包，并用包内 Node 启动（不得在用户机器
上 `npx` 下载依赖）。当前该接线尚未完成，见 `doc/Harness根治_实施记录.md` §5。
