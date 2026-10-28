# WorkBuddy 同预算盲评模板（2026-10-24）

> 状态：**工具与协议已就绪，尚未执行**。
> 盲评按定义需要人来打分（隐藏产物来源），并且要消耗媒体额度；Agent 无法自评代替。
> 本文件是执行清单 + 记分表 + 判定脚本约定，跑完把结果贴回 `doc/变更.md`。

依据：`doc/计划_媒体动画与记忆质量修复.md` §10（对擂方法与完成门槛）。

---

## 1. 同预算约束（缺一不可）

| 约束 | 做法 |
|---|---|
| 同原始参考 | 两边都用同一张原始参考图（记录 sha256） |
| 同任务文本 | 同一段任务书原文，不改写成"更适合自己"的版本 |
| 同语言模型 | 主执行模型都是 DeepSeek1017 |
| 同媒体能力 | 同一媒体通道与能力声明（`IMAGE_CAPABILITIES`/`VIDEO_CAPABILITIES` 一致） |
| 同调用预算 | 同一重试/重抽次数上限（建议：锚点 3 次、关键姿态每个 2 次、中间帧每帧 1 次） |
| 公平重抽 | **禁止** BMA 无限重抽而 WorkBuddy 只跑一次；重抽次数必须记录 |

---

## 2. 场景矩阵（≥20）

| # | 场景 | 检查点 |
|---|---|---|
| 1 | 单张立绘（t2i） | 身份一致性、构图、留白 |
| 2 | 参考图锚点（subject_reference） | 与参考图的角色一致性 |
| 3 | 局部编辑（image_edit，能力具备时） | 非编辑区是否保持 |
| 4 | 系列素材 6 张（共享锚点） | 六张之间的身份一致性 |
| 5 | 正面奔跑 16 帧（关键姿态路线） | 断腿/换角色/动作可读性 |
| 6 | 正面奔跑（视频先验路线） | 抽帧后的稳定性 |
| 7 | 4 帧走路 10fps | 相位完整性 |
| 8 | 循环接缝 | 末帧→首帧是否闪 |
| 9 | 路线修改："重新 AI 生图" | 是否保留锚点审批/视角/角色合同 |
| 10 | 路线修改："以后不走视频抽帧" | 旧路线是否作废、新路线是否生效 |
| 11 | 只回答不执行（"告诉我这个 skill 干嘛用的"） | 是否只回答、零生成 |
| 12 | "只改腿、其它不变"（能力不足时） | 是否如实上报能力不足而非硬凑 |
| 13 | 留白/裁切修复 | 是否用确定性手段而非重抽 |
| 14 | 锚点未批准就要求 16 帧 | 是否被审批门拦住 |
| 15 | 换参考图后要求继续 | 旧批准是否失效、是否重新审批 |
| 16 | 短视频 4s（i2v） | 是否走对通道（不出现 volcengine/openai 错路） |
| 17 | 视角修改（"改成侧视"） | 是否只改视角轴、不撤其他约束 |
| 18 | 产物反馈（"幅度太小"） | 是否记录反馈而不否决路线 |
| 19 | 批量 8 张独立素材 | 是否允许批量（非锚点） |
| 20 | 同一 session 连续两次纠正 | 第二次是否仍然生效（不是只过滤一轮） |
| 21 | 重启后续跑 | 撤回/审批状态是否仍在 |
| 22 | 子 Agent 派发后的项目经验注入 | 已撤回条目是否缺席 |

---

## 3. 记分表（每条场景一行）

```csv
scenario,arm,run,identity_consistency,aesthetics,anatomy,action_readability,stability,hard_fail,wall_clock_s,tokens,media_calls,cost,notes
1,bma,1,,,,,,"",,,,,
1,wb,1,,,,,,"",,,,,
```

- `identity_consistency / aesthetics / anatomy / action_readability / stability`：1~5 分（盲评人独立打分）。
- `hard_fail`：`断腿|换角色|多腿|缺肢|视角漂移|无动作` 之一或多（硬失败）。
- `run`：同场景重复执行的轮次（随机任务至少 3 轮）。

判定脚本建议（跑完记分表后用）：

```bash
python - <<'EOF'
import csv, statistics
rows = list(csv.DictReader(open('blind-eval.csv', encoding='utf-8-sig')))
def rate(arm, key):
    v = [float(r[key]) for r in rows if r['arm']==arm and r[key]]
    return statistics.mean(v) if v else 0
for arm in ('bma','wb'):
    print(arm, {k: round(rate(arm,k),2) for k in ('identity_consistency','aesthetics','anatomy','action_readability','stability')})
# 胜率 = bma 五项均值 > 同场景 wb 五项均值的场景占比
EOF
```

---

## 4. 完成门槛（计划 §10.2）

1. 指令回归场景（第 9~22 条）100% 通过；
2. 一次性任务指令写全局记忆 = 0；
3. `task_id → 产物` 对账准确率 100%；
4. 未配置 provider / 不支持能力的本地拦截率 100%（免费层冒烟已覆盖，见 `docker/media-smoke.mjs`）；
5. 锚点未批准时进入下一阶段的次数 = 0（免费层冒烟已覆盖）；
6. 立绘盲评 BMA 胜率 ≥ 50%，身份硬失败率 ≤ WorkBuddy；
7. 动画盲评 BMA 胜率 ≥ 50%，断腿/换角色等硬失败率 ≤ WorkBuddy；
8. 技术 audit 全过但视觉未审 ⇒ 只交付 `needs_review`。

前 5 条可由脚本/单测验证（已就绪）；后 3 条必须人工盲评。

---

## 5. 执行顺序建议

```bash
# 0) 免费层：桥的本地拦截（零额度，已通过 7/7）
node docker/media-smoke.mjs

# 1) 指令回归：用真实会话事件离线回放（不需要额度）
GOTOOLCHAIN=local go test ./internal/agent -run 'TestLifecycle|TestAxis|TestDecision' -count=1

# 2) 真实媒体最小冒烟（花钱，需 --yes-spend）
node docker/media-smoke.mjs --live --yes-spend --ref=assets/img/<已批准锚点>.png

# 3) 20+ 场景同预算对擂 → 填 blind-eval.csv → 人工盲评打分
```

盲评结束后把结论、费用、重抽次数写进 `doc/变更.md`。
