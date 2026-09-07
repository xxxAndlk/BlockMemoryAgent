# SWE 对照迭代日志

基准：12 个有效 SWE-bench 真实 issue 任务（flask-5014 / requests-5414 / requests-6028 / pylint-6903 / pylint-7080 / pylint-7277 / pytest-10051 / pytest-10356 / pytest-7432 / pytest-7521 / pytest-7982 / sphinx-10466），全部过 oracle（base=false, fix=true）。判分 EXCLUDE_ENV 双侧同剔。

## 基线（iter0）
- claude r1（原 6 题）：6/6，中位 757s
- bma r1（原 6 题）：2/6

## iter1 — 工具调用健壮性修复（代码）
- 改动：`backend/internal/agent/react_types.go` + `react_agent.go`：非法工具参数 JSON 不再静默丢弃（曾导致"假成功"），加 maxBadToolCallResponses=3 + nudge 重试守卫，带测试。
- 结果（原 6 题）：5/6（pytest-10356 未解），中位 3635.5s
- 归档：`test/swe/archive/iter1_bma/`

## iter2 — thinking 降档（配置）
- 改动：`config/roles.yaml` 执行角色 thinking 降档（glm-5.3/glm-5.3-flash 只能 low，ark 端点拒绝 disabled；deepseek-v4-flash/gemini-flash/doubao-lite/kimi-k3 用 off；meta 保留 max）
- 结果（12 题矩阵，含 2 弃题；有效 10 题）：7/10（pylint-7080、pylint-7277、pytest-10356 未解），中位 2775s
  - pytest-7432/7982 首轮判 FAIL 系判分环境问题（pytest 5.x/6.x 在 py3.10 下 AST 收集全崩；test_files.txt CRLF 致重判时基线恢复失效）。修复 lib.sh：按任务镜像覆盖（tasks/<iid>/image）+ 清单去 \r，重判后两题均 resolved=true。
- 归因：失败/超时 run 输出体量 57k-140k tokens（glm-5.3-flash ≈46 tok/s 吞吐受限，输出体量=墙钟）；pylint-7080 全程耗在侦察+复现脚本，超时前未落源码修复（diff 里只有测试文件 21 行）；每次 llm_input 重复注入全量项目概览。
- 归档：`test/swe/archive/iter2_bma/`

## iter2→D1 改进（提示词，通用）
- `config/roles.yaml` domain_agent【侦察纪律】追加硬约束：侦察读/搜合计 ≤8 次后到限必须 patch-first；禁止"先复现再修"的独立复现脚本，修复后现成命令一次验证；改动一律 EditFile 增量编辑禁止整文件重写；每轮分析文字 ≤3 行。
