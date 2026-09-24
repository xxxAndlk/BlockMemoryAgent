package subagent

// expand.go 展开式召回（TODO #22④，LCM 只读展开器 + Hermes delegation grant）。
//
// 机制：老段折 DAG 摘要后，agent 遇摘要点（包体讲不细）调 expand_memory 派**只读**展开器
// 沿摘要链定位细节所属段，再用低层工具取原文（workspace 文件 / .bma/returns 全文 /
// .bma/tool_outputs 落盘件），答案 ≤2K tok 回灌。
//
// 约束（LCM/Hermes 同款）：
//   - delegation grant：token 预算 cap（expandGrantTokens）+ TTL（expandTTL）双闸；
//   - 结构性禁递归：展开器工具面只给低层展开工具（ReadFile/SearchInFiles/ListDir），
//     无 call_sub_agent/send_message——派不出下级；
//   - 答案硬顶 2K tok（返回前截断，不靠提示词自觉）。
//
// 与 salvage 合成完整记忆环：失败轨迹由 salvage 主动重注（现状），成功细节由本工具
// 按需展开——一推一拉，压缩不再等于失忆。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/google/jsonschema-go/jsonschema"
)

// delegation grant 参数（TODO #22④）。
const (
	expandGrantTokens   = 8000                 // 展开器 token 预算 cap（delegation grant）
	expandMaxIter       = 6                    // 展开器最大 LLM 轮数（防探查失控）
	expandTTL           = 3 * time.Minute      // 展开器 TTL：超时即截答返回已有部分
	expandAnswerMaxRune = 2000                 // 答案硬顶 ~2K tok（CJK 1 tok ≈ 1 rune）
	expandToolRuneCap   = 16000                // 单工具结果进展开器历史的截断
	expandReadTools     = "ReadFile, SearchInFiles, ListDir" // 结构性禁递归的低层工具面
)

// expandChainFn 提取摘要包链回调（bootstrap 接 memory.Pipeline.Bundles）；nil=无链可展开。
type expandChainFn func(agentID string) []string

// RegisterExpandTool 注册 expand_memory 工具（展开式召回，TODO #22④）。
// 可见角色由 roles.yaml / role.Registry 白名单控制（meta + domain）。
func (d *Dispatcher) RegisterExpandTool(r *tool.Registry) {
	r.Register(&expandMemoryTool{dispatcher: d})
}

// expandMemoryTool 实现 expand_memory：派只读展开器沿摘要链取回被压缩掉的细节。
type expandMemoryTool struct{ dispatcher *Dispatcher }

func (t *expandMemoryTool) Name() string { return "expand_memory" }

func (t *expandMemoryTool) Aliases() []string { return nil }

func (t *expandMemoryTool) Description() string {
	return "展开式召回：压缩摘要讲不细时，派一个**只读**展开器沿摘要链定位细节并取回原文。" +
		"适用：早期对话细节被压进【压缩摘要】/【探查摘要】，你需要确切的文件内容、结论依据或命令输出。" +
		"参数 query=要取回的细节（越具体越好：文件名/函数名/结论关键词）。" +
		"展开器只持 ReadFile/SearchInFiles/ListDir（派不出下级、写不了文件），" +
		"答案上限 2000 字，超时/超预算截答返回已有部分。" +
		"与失败打捞互补：失败轨迹已主动注入（salvage），成功细节用本工具按需展开。"
}

func (t *expandMemoryTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query": {Type: "string", Description: "要取回的细节：文件名/函数名/结论关键词"},
		},
		Required: []string{"query"},
	}
}

// Execute 同步派只读展开器（delegation grant 双闸 + 答案硬顶）。
func (t *expandMemoryTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return &tool.Result{Tool: "expand_memory", Error: "query is required", Category: tool.ResultCategoryValidationRejected}
	}
	caller := agent.AgentIDFromContext(ctx)
	if caller == "" {
		return &tool.Result{Tool: "expand_memory", Error: "missing caller agent context"}
	}

	// 摘要链（leaf→condensed）：展开器沿链定位细节所属段，再取原文。
	chain := ""
	if d.expandChain != nil {
		if bundles := d.expandChain(caller); len(bundles) > 0 {
			var b strings.Builder
			b.WriteString("【摘要链】（旧→新，每包是一段被压缩的早期对话摘要）：\n")
			for i, p := range bundles {
				fmt.Fprintf(&b, "包%d: %s\n", i+1, truncateRunes(p, 600))
			}
			chain = b.String()
		}
	}

	task := buildExpandTask(query, chain)

	// delegation grant：TTL ctx + token/轮数 cap；超预算截答（Run 返回 LimitReached 部分产出）。
	ttlCtx, cancel := context.WithTimeout(ctx, expandTTL)
	defer cancel()

	answer, err := d.runExpander(ttlCtx, caller, task)
	if err != nil && answer == "" {
		return &tool.Result{Tool: "expand_memory", Error: fmt.Sprintf("展开失败: %v", err)}
	}
	answer = truncateRunes(strings.TrimSpace(answer), expandAnswerMaxRune)
	if answer == "" {
		return &tool.Result{Tool: "expand_memory", Error: "展开器无产出（细节可能从未入账；用 ReadFile 直接读源文件）"}
	}
	if err != nil {
		answer += "\n\n（展开器提前收口: " + err.Error() + "）"
	}
	return &tool.Result{Tool: "expand_memory", Success: true, Output: answer}
}

// buildExpandTask 组装展开器任务：摘要链 + 查询 + 只读纪律 + 答案硬顶。
func buildExpandTask(query, chain string) string {
	var b strings.Builder
	if chain != "" {
		b.WriteString(chain)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, `【展开式召回】只读展开器。
要取回的细节：%s
工作方式：
1. 先从摘要链判断细节大概属于哪段/哪个文件，再用 ReadFile/SearchInFiles 取原文（.bma/returns/ 是历史回传全文、.bma/tool_outputs/ 是工具输出落盘件，均可读）；
2. 只用 %s 工具（无写入、无派发能力）；
3. 答案不超过 2000 字：只给要取回的细节原文/摘录 + 出处（文件:行号），不要复述任务。
`, query, expandReadTools)
	return b.String()
}

// runExpander 构造并驱动只读展开器子 Agent（一次性、不入树、不 notify）。
// 工具面硬编码低层展开工具（结构性禁递归）；LoopConfig=delegation grant 预算。
func (d *Dispatcher) runExpander(ctx context.Context, parentID, task string) (string, error) {
	roleID := roleIDFromAgentID(parentID)
	provider, err := d.providerForAgent(ctx, roleID, parentID)
	if err != nil {
		return "", fmt.Errorf("get model: %w", err)
	}
	subID := parentID + "/expander-" + fmt.Sprintf("%d", d.seq.Add(1))

	// 展开器角色：合成定义（不入 role.Registry——不是编排角色，是工具内部探针）。
	roleDef := types.RoleDefinition{
		ID:      "expander",
		Name:    "只读展开器",
		Type:    enums.RoleTypeFixed,
		Tools:   []string{"ReadFile", "SearchInFiles", "ListDir"},
		SystemPrompt: "你是只读展开器：按任务给出的摘要链与查询，用 ReadFile/SearchInFiles/ListDir " +
			"取回被压缩掉的细节原文。禁止写入、禁止派发。答案 ≤2000 字，给原文/摘录 + 出处。",
	}

	mem := d.memory
	if mem == nil {
		mem = agent.NopMemoryPipeline{}
	}
	// 工具适配器白名单=roleDef.Tools（call_sub_agent 等结构性不可见，派不出下级）。
	adapter := agent.NewToolRegistryAdapterForRole(d.tools, subID, roleDef.Tools, roleDef.ID, nil)
	sub := agent.NewReActAgent(subID, roleDef, provider, adapter).
		WithProviderFunc(func(callCtx context.Context) (agent.ModelProvider, error) {
			return d.providerForAgent(callCtx, roleID, parentID)
		}).
		WithMemory(mem).
		WithWorkDir(d.subAgentWorkDirFor(ctx)).
		WithLoopConfig(agent.LoopConfig{
			TokenBudget:        expandGrantTokens,
			MaxIterations:      expandMaxIter,
			ToolOutputMaxRunes: expandToolRuneCap,
		})

	res, err := sub.Run(ctx, task)
	// LimitReached（预算/轮数收口）有部分产出也算成功——截答语义。
	if res.Text != "" {
		return res.Text, nil
	}
	if err != nil {
		return agent.LastAssistantText(res.History), err
	}
	return agent.LastAssistantText(res.History), nil
}
