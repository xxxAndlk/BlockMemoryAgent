package config

// harness_modes_test.go 锁定 Harness 根治开关的默认值与非法值回落（计划 §5.4 必测项），
// 并断言默认档位符合灰度顺序：logical_task=shadow、action_transaction=off、
// semantic_edit=off、contract_verification=shadow——任何默认值变化都会让发布顺序失效。

import "testing"

func TestHarnessModeDefaults(t *testing.T) {
	c := &Config{}
	c.applyHarnessModeDefaults()
	if c.Agent.LogicalTaskMode != HarnessModeShadow {
		t.Fatalf("logical_task_mode default = %q, want shadow", c.Agent.LogicalTaskMode)
	}
	if c.Agent.ActionTransactionMode != HarnessModeOff {
		t.Fatalf("action_transaction_mode default = %q, want off", c.Agent.ActionTransactionMode)
	}
	if c.Agent.SemanticEditMode != HarnessModeOff {
		t.Fatalf("semantic_edit_mode default = %q, want off", c.Agent.SemanticEditMode)
	}
	if c.Agent.ContractVerificationMode != HarnessModeShadow {
		t.Fatalf("contract_verification_mode default = %q, want shadow", c.Agent.ContractVerificationMode)
	}
	if c.Agent.CheckpointMaxFiles != 20000 {
		t.Fatalf("checkpoint_max_files default = %d, want 20000", c.Agent.CheckpointMaxFiles)
	}
	if c.Agent.LogicalTaskContractMaxRunes != 2200 {
		t.Fatalf("logical_task_contract_max_runes default = %d, want 2200", c.Agent.LogicalTaskContractMaxRunes)
	}
}

func TestHarnessModeIllegalFallsBack(t *testing.T) {
	c := &Config{}
	c.Agent.LogicalTaskMode = "ENFORCE-NOW"
	c.Agent.ActionTransactionMode = "yes"
	c.Agent.SemanticEditMode = "maybe"
	c.Agent.ContractVerificationMode = "1"
	c.applyHarnessModeDefaults()
	if c.Agent.LogicalTaskMode != HarnessModeShadow {
		t.Fatalf("illegal logical_task_mode must fall back to shadow, got %q", c.Agent.LogicalTaskMode)
	}
	if c.Agent.ActionTransactionMode != HarnessModeOff {
		t.Fatalf("illegal action_transaction_mode must fall back to off, got %q", c.Agent.ActionTransactionMode)
	}
	if c.Agent.SemanticEditMode != HarnessModeOff {
		t.Fatalf("illegal semantic_edit_mode must fall back to off, got %q", c.Agent.SemanticEditMode)
	}
	if c.Agent.ContractVerificationMode != HarnessModeShadow {
		t.Fatalf("illegal contract_verification_mode must fall back to shadow, got %q", c.Agent.ContractVerificationMode)
	}
}

func TestHarnessModeExplicitValuesPreserved(t *testing.T) {
	c := &Config{}
	c.Agent.LogicalTaskMode = " Enforce "
	c.Agent.ActionTransactionMode = "SHADOW"
	c.Agent.SemanticEditMode = "enforce"
	c.Agent.ContractVerificationMode = "off"
	c.applyHarnessModeDefaults()
	if c.Agent.LogicalTaskMode != HarnessModeEnforce {
		t.Fatalf("logical_task_mode = %q", c.Agent.LogicalTaskMode)
	}
	if c.Agent.ActionTransactionMode != HarnessModeShadow {
		t.Fatalf("action_transaction_mode = %q", c.Agent.ActionTransactionMode)
	}
	if c.Agent.SemanticEditMode != HarnessModeEnforce {
		t.Fatalf("semantic_edit_mode = %q", c.Agent.SemanticEditMode)
	}
	if c.Agent.ContractVerificationMode != HarnessModeOff {
		t.Fatalf("contract_verification_mode = %q", c.Agent.ContractVerificationMode)
	}
	// 幂等：重复调用不改变已归一化的值。
	before := *c
	c.applyHarnessModeDefaults()
	if c.Agent.LogicalTaskMode != before.Agent.LogicalTaskMode ||
		c.Agent.ActionTransactionMode != before.Agent.ActionTransactionMode {
		t.Fatal("applyHarnessModeDefaults must be idempotent")
	}
}
