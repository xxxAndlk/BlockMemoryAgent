// map_dispatch.go 提供 map_sub_agents 工具（TODO 第七项⑤）：同构批量派发——
// 单一 role_id + task_template（{{item}} 占位）+ items[] 一次性派 N 个同角色子 Agent，
// 并发限流排队；aggregate=true 时 N 个子 Agent 完成汇成一条父邮箱消息（每项两行）。
// 与 call_sub_agents（异构多领域同波）互补：map 模式面向"同一动作吃 N 个输入"的批量
// 侦察/批量转换场景（scout 类 32 项、重型 6 项），spec 门按波校验一次。
// 克制原则：不做跨角色混批、不做失败自动重试编排（失败项在聚合消息中显式标败）。
package subagent

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/google/jsonschema-go/jsonschema"
)

const (
	// mapConcurrentLimit 批量派发并发上限（worker pool 宽度）。
	mapConcurrentLimit = 6
	// mapItemsHeavyLimit 重型角色（非 spec_exempt）单波 item 上限，对齐 call_sub_agents maxBatch。
	mapItemsHeavyLimit = 6
	// mapItemsScoutLimit 侦察类（spec_exempt，如 scout）单波 item 上限：
	// 侦察无状态、单任务 ≤5 分钟、只读不写，批量收益最大且互不干扰。
	mapItemsScoutLimit = 32
)

// mapAggEntry 是聚合模式下单个子 Agent 的登记项；once 保证 notify 拦截与
// goroutine 兜底收口两路竞争时本项只记一次。
type mapAggEntry struct {
	agg  *mapAggregation
	idx  int
	item string
	once sync.Once
}

// mapAggItemResult 是聚合消息中单项的回传（保持派发顺序）。
type mapAggItemResult struct {
	Idx     int    `json:"idx"`
	Item    string `json:"item"`
	Summary string `json:"summary"`
	OK      bool   `json:"ok"`
}

// mapAggregation 收口同波全部项后回调 onDone（只触发一次，done 后 onDone 置 nil 防重放）。
type mapAggregation struct {
	mu       sync.Mutex
	total    int
	results  []mapAggItemResult
	finished int
	onDone   func(results []mapAggItemResult)
}

// record 记入单项结果；全部项收口时触发一次 onDone。
func (a *mapAggregation) record(idx int, item, summary string, ok bool) {
	a.mu.Lock()
	a.results = append(a.results, mapAggItemResult{Idx: idx, Item: item, Summary: summary, OK: ok})
	a.finished++
	done := a.finished >= a.total
	cb := a.onDone
	if done {
		a.onDone = nil
	}
	res := append([]mapAggItemResult(nil), a.results...)
	a.mu.Unlock()
	if done && cb != nil {
		// 按派发顺序排列（并发完成顺序不定，聚合消息保持 items 原序）。
		sort.Slice(res, func(i, j int) bool { return res[i].Idx < res[j].Idx })
		cb(res)
	}
}

// mapSubAgentsTool 是 map_sub_agents 工具的封装。
type mapSubAgentsTool struct {
	dispatcher *Dispatcher
}

// Name 返回工具名称。
func (t *mapSubAgentsTool) Name() string { return "map_sub_agents" }

// Aliases 返回工具别名列表，当前无别名。
func (t *mapSubAgentsTool) Aliases() []string { return nil }

// Description 返回工具的 LLM 可见描述。
func (t *mapSubAgentsTool) Description() string {
	return "同构批量派发：一个 role_id + 任务模板（{{item}} 占位）+ items 列表，" +
		"逐项替换后并发派出（限流排队），全部完成后各子 Agent 独立回传父邮箱" +
		"（aggregate=true 时汇成一条消息：每项两行 item→结论/状态）。\n" +
		"用途：同一动作吃 N 个输入的批量场景——批量定位 N 个符号、批量读 N 个文件、批量查证 N 个疑点；" +
		"典型 role_id=scout（侦察）。异构多领域拆分请用 call_sub_agents。\n" +
		"参数：role_id 必填；task_template 必填（须含 {{item}} 占位）；items 必填（字符串数组）；" +
		"context 可选（每项任务前附加的共享上下文）；wall_clock_min 可选（单项目标墙钟，分钟）；" +
		"aggregate 可选（默认 false）。返回派发清单；aggregate 模式汇总稍后经邮箱到达。"
}

// InputSchema 返回入参 JSON Schema（SchemaSource 动态回退桥接，无需显式 NewFunc）。
func (t *mapSubAgentsTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"role_id":       {Type: "string", Description: "统一目标角色 id（如 scout）"},
			"task_template": {Type: "string", Description: "任务模板，必须含 {{item}} 占位，逐项替换后派发"},
			"items":         {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "字符串列表，每项替换进模板"},
			"context":       {Type: "string", Description: "可选：每项任务前附加的共享上下文（结论/契约/路径前缀）"},
			"wall_clock_min": {Type: "number", Description: "可选：单项墙钟预算（分钟），缺省由角色决定（scout=5）"},
			"aggregate":     {Type: "boolean", Description: "可选：true 时 N 项完成汇成一条邮箱消息，默认 false 逐项回传"},
		},
		Required: []string{"role_id", "task_template", "items"},
	}
}

// Execute 执行 map_sub_agents：逐项模板替换 → 限流并发 dispatchOne → 可选聚合回传。
// spec 门按波校验一次（spec_exempt 角色整波跳过）；单项失败不影响其他项。
func (t *mapSubAgentsTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	d := t.dispatcher
	res := &tool.Result{Tool: "map_sub_agents"}

	roleID, _ := args["role_id"].(string)
	roleID = strings.TrimSpace(roleID)
	tmpl, _ := args["task_template"].(string)
	contextStr, _ := args["context"].(string)
	aggregate, _ := args["aggregate"].(bool)
	rawItems, _ := args["items"].([]any)

	parentID := agent.AgentIDFromContext(ctx)
	if parentID == "" {
		res.Error = "missing parent agent context"
		return res
	}
	if roleID == "" || strings.TrimSpace(tmpl) == "" || len(rawItems) == 0 {
		res.Error = "role_id、task_template（须含 {{item}}）、items 均必填"
		res.Category = tool.ResultCategoryValidationRejected
		return res
	}
	if !strings.Contains(tmpl, "{{item}}") {
		res.Error = `task_template 必须含 {{item}} 占位符`
		res.Category = tool.ResultCategoryValidationRejected
		return res
	}

	roleDef := d.registry.Get(roleID)
	if roleDef == nil {
		res.Error = fmt.Sprintf("unknown role: %s", roleID)
		res.Category = tool.ResultCategoryValidationRejected
		return res
	}
	maxItems := mapItemsHeavyLimit
	if roleDef.SpecExempt {
		maxItems = mapItemsScoutLimit
	}
	if len(rawItems) > maxItems {
		res.Error = fmt.Sprintf("items 过多：%d 项（role %s 上限 %d）。超过请分多波派发", len(rawItems), roleID, maxItems)
		res.Category = tool.ResultCategoryValidationRejected
		return res
	}
	if !d.registry.CanCall(roleIDFromAgentID(parentID), roleID) {
		res.Error = fmt.Sprintf("role %s cannot be called by %s", roleID, parentID)
		res.Category = tool.ResultCategoryValidationRejected
		return res
	}
	// spec 门按波校验一次：map 模式共用一份规格（单键），spec_exempt 角色整波跳过。
	if !roleDef.SpecExempt {
		if msg, _ := d.checkSpecBeforeDispatch(ctx, parentID, ""); msg != "" {
			res.Error = msg
			res.Category = tool.ResultCategoryValidationRejected
			return res
		}
	}

	wallClock := d.wallClockArg(args)
	items := make([]string, 0, len(rawItems))
	for i, r := range rawItems {
		s, ok := r.(string)
		if !ok || strings.TrimSpace(s) == "" {
			res.Error = fmt.Sprintf("items[%d] 必须是非空字符串", i)
			res.Category = tool.ResultCategoryValidationRejected
			return res
		}
		items = append(items, s)
	}

	// 聚合模式：构建聚合器，全部项收口后经 notify 单条送达父邮箱。
	var agg *mapAggregation
	batchID := ""
	if aggregate {
		agg = &mapAggregation{total: len(items)}
		batchID = fmt.Sprintf("%s/map-%d", parentID, d.seq.Add(1))
		agg.onDone = func(results []mapAggItemResult) {
			var b strings.Builder
			okCount := 0
			for _, r := range results {
				status := "失败"
				if r.OK {
					status = "完成"
					okCount++
				}
				b.WriteString(fmt.Sprintf("%s → [%s] %s\n", truncateRunes(r.Item, 120), status, truncateRunes(firstLine(r.Summary), 300)))
			}
			b.WriteString(fmt.Sprintf("\n共 %d 项：完成 %d / 失败 %d。需要全文按台账/回传子 Agent 路径取。", len(results), okCount, len(results)-okCount))
			d.notify(parentID, batchID, "【map_sub_agents 聚合回传】\n"+b.String(), nil)
		}
	}

	// 限流 worker pool：sem 宽度 6，逐项 dispatchOne（内部异步启动子 Agent）。
	sem := make(chan struct{}, mapConcurrentLimit)
	var wg sync.WaitGroup
	var okIDs, errs []string
	var errsMu sync.Mutex
	for i, item := range items {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, it string) {
			defer wg.Done()
			defer func() { <-sem }()
			task := strings.ReplaceAll(tmpl, "{{item}}", it)
			if contextStr != "" {
				task = contextStr + "\n\n" + task
			}
			var callOpts []*dispatchOpts
			if agg != nil {
				callOpts = append(callOpts, &dispatchOpts{aggregate: agg, aggItem: it, aggIdx: idx})
			}
			subAgentID, errRes := d.dispatchOne(ctx, roleID, "", task, "", "", "", nil, nil, wallClock, "", "", callOpts...)
			if errRes != nil {
				errsMu.Lock()
				errs = append(errs, fmt.Sprintf("items[%d](%s): %s", idx, truncateRunes(it, 60), errRes.Error))
				errsMu.Unlock()
				// dispatchOne 未启动 goroutine（立即拒绝）：聚合器此处兜底收口，防悬挂。
				if agg != nil {
					agg.record(idx, it, "派发失败："+errRes.Error, false)
				}
				return
			}
			errsMu.Lock()
			okIDs = append(okIDs, subAgentID)
			errsMu.Unlock()
		}(i, item)
	}
	wg.Wait()

	log.Printf("[subagent] map dispatch: parent=%s role=%s items=%d aggregate=%v ok=%d fail=%d", parentID, roleID, len(items), aggregate, len(okIDs), len(errs))

	res.Success = true
	out := fmt.Sprintf("已派出 %d 项（role=%s 并发=%d aggregate=%v）", len(items), roleID, mapConcurrentLimit, aggregate)
	if batchID != "" {
		out += fmt.Sprintf("，聚合批次 %s：全部完成后单条汇总送达", batchID)
	}
	if len(errs) > 0 {
		out += "\n失败项:\n" + strings.Join(errs, "\n")
	}
	res.Output = out
	return res
}

// firstLine 取文本首行（聚合消息单项摘要用）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
