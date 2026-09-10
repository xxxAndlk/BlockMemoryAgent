package role

// model_tools_test.go 覆盖 list_models / set_role_model：
// 正常路径、meta/lightweight 拒绝、未知角色拒绝、空 reason 拒绝、
// 探活（SwitchModel）错误原样透传、45s 子 ctx 上限、注册表可 Dispatch。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeSwitcher 是 ModelSwitcher 的测试替身。
type fakeSwitcher struct {
	entries   []types.ModelEntry
	roles     []string
	info      map[string]model.ModelStatus
	switchErr error
	// 记录最后一次 SwitchModel 调用。
	lastRole, lastModel, lastThinking string
	lastDeadline                      time.Time
}

func (f *fakeSwitcher) RegistryModels() []types.ModelEntry { return f.entries }
func (f *fakeSwitcher) SwitchableRoles() []string          { return f.roles }
func (f *fakeSwitcher) CurrentModelInfo(roleID string) (model.ModelStatus, error) {
	st, ok := f.info[roleID]
	if !ok {
		return model.ModelStatus{}, errors.New("unknown role")
	}
	return st, nil
}
func (f *fakeSwitcher) SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	f.lastRole, f.lastModel, f.lastThinking = roleID, modelID, thinking
	if dl, ok := ctx.Deadline(); ok {
		f.lastDeadline = dl
	}
	if f.switchErr != nil {
		return types.AgentModelConfig{}, f.switchErr
	}
	return types.AgentModelConfig{Provider: "p", Model: "m"}, nil
}

func newFakeSwitcher() *fakeSwitcher {
	return &fakeSwitcher{
		entries: []types.ModelEntry{
			{ID: "fast", Name: "快档", Provider: "p", Model: "m1", Description: "高频小修", MaxOutputTokens: 32768},
			{ID: "strong", Name: "强档", Provider: "p", Model: "m2", Description: "长程推理"},
		},
		roles: []string{"meta", "domain", "lightweight", "code_assistant", "scout"},
		info: map[string]model.ModelStatus{
			"domain":         {Provider: "p", Model: "m1", ModelID: "fast", Thinking: "medium", Bound: true},
			"code_assistant": {Provider: "p", Model: "m1", ModelID: "fast"},
		},
	}
}

func TestListModelsTool_ListsEntriesAndTargets(t *testing.T) {
	fs := newFakeSwitcher()
	res := (&listModelsTool{switcher: fs}).Execute(context.Background(), nil)
	if !res.Success {
		t.Fatalf("list_models failed: %s", res.Error)
	}
	var parsed struct {
		Models []map[string]any `json:"models"`
		Roles  []map[string]any `json:"switchable_roles"`
	}
	if err := json.Unmarshal([]byte(res.Output), &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, res.Output)
	}
	if len(parsed.Models) != 2 {
		t.Fatalf("models len = %d, want 2", len(parsed.Models))
	}
	if parsed.Models[0]["id"] != "fast" || parsed.Models[0]["description"] != "高频小修" ||
		parsed.Models[0]["max_output_tokens"].(float64) != 32768 {
		t.Errorf("entry 内容错误: %+v", parsed.Models[0])
	}
	// meta/lightweight 应被排除；domain/code_assistant/scout 在列。
	got := map[string]bool{}
	for _, r := range parsed.Roles {
		got[r["role_id"].(string)] = true
	}
	if got["meta"] || got["lightweight"] {
		t.Errorf("meta/lightweight 不应出现在可切换角色: %+v", parsed.Roles)
	}
	if !got["domain"] || !got["code_assistant"] || !got["scout"] {
		t.Errorf("可切换角色缺失: %+v", parsed.Roles)
	}
	for _, r := range parsed.Roles {
		if r["role_id"] == "domain" && r["model_id"] != "fast" {
			t.Errorf("domain 当前绑定错误: %+v", r)
		}
	}
}

func TestSetRoleModelTool_HappyPath(t *testing.T) {
	fs := newFakeSwitcher()
	res := (&setRoleModelTool{switcher: fs}).Execute(context.Background(), map[string]any{
		"role_id": "domain", "model_id": "strong", "reason": "长程推理任务需要更强模型",
	})
	if !res.Success {
		t.Fatalf("set_role_model failed: %s", res.Error)
	}
	if fs.lastRole != "domain" || fs.lastModel != "strong" || fs.lastThinking != "" {
		t.Errorf("SwitchModel 参数错误: %s/%s/%q", fs.lastRole, fs.lastModel, fs.lastThinking)
	}
	if !strings.Contains(res.Output, "domain") || !strings.Contains(res.Output, "strong") ||
		!strings.Contains(res.Output, "长程推理任务需要更强模型") {
		t.Errorf("output 应含角色/模型/reason: %s", res.Output)
	}
	// 45s 子 ctx 上限（含余量断言）。
	if fs.lastDeadline.IsZero() || time.Until(fs.lastDeadline) > modelSwitchTimeout {
		t.Errorf("SwitchModel 应在 %v 子 ctx 内调用", modelSwitchTimeout)
	}
}

func TestSetRoleModelTool_RejectsDisallowedAndInvalid(t *testing.T) {
	fs := newFakeSwitcher()
	cases := []struct {
		name    string
		args    map[string]any
		wantSub string
	}{
		{"meta 拒绝", map[string]any{"role_id": "meta", "model_id": "strong", "reason": "r"}, "不允许"},
		{"lightweight 拒绝", map[string]any{"role_id": "lightweight", "model_id": "strong", "reason": "r"}, "不允许"},
		{"未知角色拒绝", map[string]any{"role_id": "ghost", "model_id": "strong", "reason": "r"}, "不允许"},
		{"空 reason", map[string]any{"role_id": "domain", "model_id": "strong"}, "reason 必填"},
		{"空 role_id", map[string]any{"model_id": "strong", "reason": "r"}, "必填"},
		{"空 model_id", map[string]any{"role_id": "domain", "reason": "r"}, "必填"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := (&setRoleModelTool{switcher: fs}).Execute(context.Background(), c.args)
			if res.Success || res.Error == "" {
				t.Fatalf("应拒绝，got Success=%v Error=%q", res.Success, res.Error)
			}
			if !strings.Contains(res.Error, c.wantSub) {
				t.Errorf("错误信息应含 %q: %s", c.wantSub, res.Error)
			}
			if res.Category != tool.ResultCategoryValidationRejected {
				t.Errorf("拒绝应标 validation_rejected, got %q", res.Category)
			}
			if fs.lastRole != "" {
				t.Errorf("拒绝路径不应调用 SwitchModel")
			}
		})
	}
}

func TestSetRoleModelTool_ProbeErrorPassthrough(t *testing.T) {
	fs := newFakeSwitcher()
	fs.switchErr = errors.New(`模型 "strong" 连通性探测失败，保持原模型: 401 unauthorized`)
	res := (&setRoleModelTool{switcher: fs}).Execute(context.Background(), map[string]any{
		"role_id": "domain", "model_id": "strong", "reason": "r",
	})
	if res.Success {
		t.Fatal("探测失败应返回错误")
	}
	if !strings.Contains(res.Error, "连通性探测失败") || !strings.Contains(res.Error, "401") {
		t.Errorf("错误应原样透传: %s", res.Error)
	}
	if res.Category != tool.ResultCategoryExecutionFailed {
		t.Errorf("执行失败应标 execution_failed, got %q", res.Category)
	}
}

func TestModelTools_NilSwitcher(t *testing.T) {
	res := (&listModelsTool{}).Execute(context.Background(), nil)
	if res.Success || !strings.Contains(res.Error, "not configured") {
		t.Fatalf("nil switcher 应报未配置: %+v", res)
	}
	res = (&setRoleModelTool{}).Execute(context.Background(), map[string]any{
		"role_id": "domain", "model_id": "x", "reason": "r",
	})
	if res.Success || !strings.Contains(res.Error, "not configured") {
		t.Fatalf("nil switcher 应报未配置: %+v", res)
	}
}

func TestRegisterModelTools_Dispatch(t *testing.T) {
	tr := tool.NewBuiltinRegistry("", nil, nil)
	RegisterModelTools(tr, newFakeSwitcher())
	res, _ := tr.Dispatch(context.Background(), "list_models", nil)
	if !res.Success {
		t.Fatalf("list_models dispatch failed: %s", res.Error)
	}
	res, _ = tr.Dispatch(context.Background(), "set_role_model", map[string]any{
		"role_id": "domain", "model_id": "strong", "reason": "test",
	})
	if !res.Success {
		t.Fatalf("set_role_model dispatch failed: %s", res.Error)
	}
}
