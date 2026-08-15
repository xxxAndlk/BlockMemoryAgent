<!-- bma:managed begin -->
# 项目概览

- 根目录: D:\WebData\github\BlockMemoryAgent\backend\internal\tui
- 生成时间: 2026-08-15 18:32:51

## 推荐领域拆分

按文件职责/实体由 LLM 分区（无 LLM 时按依赖图聚类兜底）。Agent 无明确领域归属时参考本表定位。
源码文件后括注总行数与顶层符号（`名称 L行号`，`()` 后缀为函数），可直接按符号带 offset 精读，跳过逐页扫描。

### `agent_tree_panel.go` - 入口聚合（agent_tree_panel.go）
- 影响目录: (根级)
- 影响文件 (1):
  - agent_tree_panel.go (683 行): type agentTreeNode L19, type AgentTreePanel L47, NewAgentTreePanel() L53, rebuild() L59, orchestratorNodesToTreeNodes() L134, agentNodeName() L166, fixedRoleDisplayNames L182, filterPrevRoundNodes() L195, orchestratorNodeDepth() L214, orchestratorStatusToRole() L233, deriveDomainTaskStatuses() L251, buildLines() L278
- 语言: Go

### `agent_tree_panel_test.go` - 入口聚合（agent_tree_panel_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - agent_tree_panel_test.go (197 行): TestAgentNodeName() L19, TestFilterPrevRoundNodes() L46, TestAgentTreePanelRebuildFiltersPreviousRound() L76, TestBoardSnapshotFiltersPreviousRound() L129, TestAgentsPanelSecondRoundRefresh() L162
- 语言: Go

### `chat_panel.go` - 入口聚合（chat_panel.go）
- 影响目录: (根级)
- 影响文件 (1):
  - chat_panel.go (597 行): type ChatPanel L16, NewChatPanel() L47, currentItem() L57, scrollToItem() L74, scrollToItemBottom() L87, itemUp() L125, itemDown() L131, gotoTop() L137, gotoBottom() L143, scrollbarArea() L155, thumbBounds() L168, updateDrag() L201
- 语言: Go

### `chat_panel_test.go` - 入口聚合（chat_panel_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - chat_panel_test.go (69 行): TestCollectItemsCapsHistory() L17, TestCollectItemsBelowCapKeepsAll() L49
- 语言: Go

### `flow_panel.go` - 入口聚合（flow_panel.go）
- 影响目录: (根级)
- 影响文件 (1):
  - flow_panel.go (199 行): type flowEvent L14, type flowCard L20, maxFlowEvents L26, flowColW L29, flowColGap L32, collectFlowCards() L37, flowEventsFor() L49, agentIDFromDetail() L78, formatFlowEvent() L89, flowPanelCards() L120, subAgentFlowPanelHeight() L131, flowPanelCols() L142
- 语言: Go

### `flow_panel_test.go` - 入口聚合（flow_panel_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - flow_panel_test.go (177 行): flowTestNodes() L13, TestCollectFlowCards_DomainOnly() L23, TestFlowEventsFor_AgentIDMatch() L34, TestFlowEventsFor_FallbackNameMatch() L76, TestFormatFlowEvent_SubAgentDone() L89, TestFormatFlowEvent_Error() L102, TestFlowPanelCols() L111, TestSubAgentFlowPanelHeight() L121, TestRenderSubAgentFlowPanel() L145
- 语言: Go

### `height_assert_test.go` - 入口聚合（height_assert_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - height_assert_test.go (22 行): TestViewTotalLinesEqualsHeight() L12
- 语言: Go

### `helpers.go` - 入口聚合（helpers.go）
- 影响目录: (根级)
- 影响文件 (1):
  - helpers.go (1280 行): ansiRegex L21, stripANSI() L24, sanitizeToolText() L31, flashMsg() L40, hasPlan() L45, showChatDetail() L52, currentRoundStart() L74, boardSnapshot() L94, showPlanDetailByIndex() L140, showAgentDetailByIndex() L166, type chatItem L194, buildPlanLines() L210
- 语言: Go

### `helpers_test.go` - 入口聚合（helpers_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - helpers_test.go (517 行): TestEventChatItemToolOutputCollapsed() L19, TestEventChatItemToolCallCollapsed() L98, TestEventChatItemVerboseOutputTruncated() L153, TestChatItemsCollapseToolEvents() L199, TestChatItemsDeduplicateSummary() L240, titlesOf() L259, TestFirstMessagePendingDisplay() L269, TestFirstMessageFallbackWhenSessionMissingUserMessage() L309, TestChatItemsMergeToolCallPairs() L347, TestChatItemsAgentDoneAsAssistant() L380, TestChatItemsClarifyPrefixStripped() L419, TestEventChatItemSystemPauseShown() L436
- 语言: Go

### `input.go` - 入口聚合（input.go）
- 影响目录: (根级)
- 影响文件 (1):
  - input.go (434 行): pasteEnterThreshold L22, handleInputKey() L25, submitInput() L208, postJSON() L349, maxRetries L362, createSession() L396, getJSON() L415, requestTimeout L434
- 语言: Go

### `input_bar.go` - 入口聚合（input_bar.go）
- 影响目录: (根级)
- 影响文件 (1):
  - input_bar.go (232 行): type InputBar L12, NewInputBar() L28, reset() L36, isMultiline() L44, insertRunes() L49, backspace() L55, delete() L68, moveLeft() L80, moveRight() L87, moveHome() L94, moveEnd() L99, getHistory() L104
- 语言: Go

### `keys.go` - 入口聚合（keys.go）
- 影响目录: (根级)
- 影响文件 (1):
  - keys.go (88 行): fullHelpText L44
- 语言: Go

### `live_harness_test.go` - 入口聚合（live_harness_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - live_harness_test.go (160 行): liveHarnessRoot L31, liveHarnessSnapDir() L34, TestLiveHarness() L40
- 语言: Go

### `live_items_test.go` - 入口聚合（live_items_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - live_items_test.go (81 行): TestAppendLiveItems() L14
- 语言: Go

### `live_multiturn_test.go` - 入口聚合（live_multiturn_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - live_multiturn_test.go (353 行): liveMultiturnRoot() L44, buildMergedEnvFile() L55, type turnResult L88, TestLiveMultiturn() L99
- 语言: Go

### `model.go` - 入口聚合（model.go）
- 影响目录: (根级)
- 影响文件 (1):
  - model.go (892 行): type Model L22, type sharedState L111, newSharedState() L122, setFlash() L125, getFlash() L133, setPendingSelect() L145, takePendingSelect() L152, hasPendingSelect() L164, ensureShared() L174, NewModel() L182, currentWorkDir() L215, Init() L224
- 语言: Go

### `model_test.go` - 入口聚合（model_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - model_test.go (652 行): type mockProviderForTUI L34, Generate() L39, testAgent() L44, type mockAgentForPlan L56, CreateSession() L68, ResumeSession() L73, Send() L78, Stream() L83, Query() L88, Control() L93, List() L98, Get() L103
- 语言: Go

### `multiline_input_test.go` - 入口聚合（multiline_input_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - multiline_input_test.go (209 行): TestMultilinePasteCollapsesInput() L13, TestMultilineInputBackspaceClearsAll() L47, TestSingleLineInputStillShowsCursor() L69, TestMultilineSubmitPreservesContent() L96, TestPasteEnterInsertsNewline() L121, TestRapidPasteAccumulatesLines() L160
- 语言: Go

### `overlay_panel.go` - 入口聚合（overlay_panel.go）
- 影响目录: (根级)
- 影响文件 (1):
  - overlay_panel.go (243 行): type OverlayPanel L10, NewOverlayPanel() L24, open() L29, openHelp() L38, close() L48, clampCursor() L53, render() L68, openHelpPopup() L129, togglePlanPopup() L134, toggleAgentsPopup() L155, toggleLogPopup() L176, openOverlay() L202
- 语言: Go

### `styles.go` - 入口聚合（styles.go）
- 影响目录: (根级)
- 影响文件 (1):
  - styles.go (216 行): type Styles L56, NewStyles() L160
- 语言: Go

### `subagent_nodes_test.go` - 入口聚合（subagent_nodes_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - subagent_nodes_test.go (21 行): TestSubAgentRoleLabelAndID() L8
- 语言: Go

### `task_brief_cache.go` - 入口聚合（task_brief_cache.go）
- 影响目录: (根级)
- 影响文件 (1):
  - task_brief_cache.go (131 行): type TaskBriefCache L26, NewTaskBriefCache() L36, Get() L45, Set() L57, Warm() L75, summarize() L110, summarizeTaskTitle() L129
- 语言: Go

### `tree_nodes_test.go` - 入口聚合（tree_nodes_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - tree_nodes_test.go (68 行): TestOrchestratorNodesToTreeNodes_Depth() L15, TestOrchestratorStatusToRole() L56
- 语言: Go

### `view.go` - 入口聚合（view.go）
- 影响目录: (根级)
- 影响文件 (1):
  - view.go (681 行): View() L18, singleColumnView() L28, renderTopBar() L71, renderRightPanels() L142, renderPlanPanel() L187, formatPlanSnapshot() L259, planTaskLine() L310, planFooterLines() L349, deriveDomainTaskStatuses() L386, strongerTaskStatus() L410, planTaskDomain() L425, statusColor() L437
- 语言: Go

### `view_test.go` - 入口聚合（view_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - view_test.go (192 行): TestAgentTreePrefix() L17, TestBuildAgentCardRendersNameAndStatus() L52, TestBuildMetaCardRendersNameOnly() L80, type stubAgent L101, Board() L104, Profile() L109, SaveProfile() L114, CreateSession() L117, ResumeSession() L122, Send() L127, Stream() L130, Query() L135
- 语言: Go

### `wheel_scroll_test.go` - 入口聚合（wheel_scroll_test.go）
- 影响目录: (根级)
- 影响文件 (1):
  - wheel_scroll_test.go (99 行): newOverflowModel() L17, TestChatWheelScroll() L45, TestChatScrollbarClick() L81
- 语言: Go
<!-- bma:managed end -->

<!-- 标记区外可写人手补充；RefreshProjectDoc 只重写上方 managed 区，不覆盖本提示以下内容。 -->
