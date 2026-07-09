## 待完成 / 开放项

1. **遗留表 `memory_write_failures` 清理**
   - 背景：P0-2 回调写入优化已移除后台 worker、重试与死信机制，`migrations/004_memory_write_failures.sql` 仅保留空表定义，避免删除已有数据。
   - 开放动作：确认生产/历史环境中该表无未处理死信数据后，可安全删除该文件并在 `doc/项目说明.md` 迁移列表中移除。

2. **配置层拆分后续**
   - `config/infrastructure.yaml` 与 `config/agent-policy.yaml` 已从 `config/config.yaml` 拆分，启动时 `config.Load` 会深度合并。
   - 开放动作：Web UI 增加配置拆分可视化/编辑页；评估是否将 `config/config.yaml` 精简为仅含 `include` 引用的入口文件。

3. **设计文档 v3 漂移章节归档**
   - `doc/设计文档_v3.md` 的 §4/§8/§9 已标记为历史设计。
   - 开放动作：后续若这些章节完全过时，可直接删除并用简短历史说明替代。

4. **记忆层简化评测（P3-1）**
   - 真实 Embedding（P3-3）接入后，跑 `test/coding/` + `test/api/` 全套并调用 `GET /api/memory/eval`，观察 Raw/Standard 分布。
   - 评测后再决定：若 Standard 占比高且召回无损失，可进一步简化为仅保留 Raw 7 天 + 摘要永久；否则维持两级。

5. **测试 demo 文档维护**
   - `doc/test/测试demo.md` 已更新为使用项目内置集成测试与编程场景验证的指引。
   - 开放动作：随新端到端场景（浏览器自动化、自定义工具、MCP 等）落地持续补充 demo。

## 已完成（已归档到 git 历史）

- 风险审计修复（P0/P1/P2）
- 5 路径智能路由、四层 Agent 编排、Plan-and-Execute、Self-Reflection
- 记忆层主线接入（Write/Compress/Search/Assemble/Snapshot）
- 真实 Embedding 接入与 `embed` 配置迁移到 `roles.yaml`
- 原生多协议模型接入（OpenAI / Anthropic / Ollama）
- TUI 重写与优化、集成测试模块整理
- 大文件拆分与技术债清理（多轮）
- 当前 Track 6：迁移清理、配置分层拆分、文档权威更新
