package memory

import (
	"testing"
)

func TestAllocateTokenBudget(t *testing.T) {
	budget := allocateTokenBudget(64000, nil)
	if budget.Total() > 64000 {
		t.Fatalf("total budget %d exceeds context window 64000", budget.Total())
	}
	if budget.SystemRole < 512 {
		t.Fatalf("SystemRole budget %d below minimum 512", budget.SystemRole)
	}
	if budget.SharedState < budget.TopicGlobal {
		t.Fatalf("SharedState should be larger than TopicGlobal, got %d vs %d", budget.SharedState, budget.TopicGlobal)
	}
	if budget.PrivateMemory != budget.SharedState {
		t.Fatalf("PrivateMemory and SharedState should be equal, got %d vs %d", budget.PrivateMemory, budget.SharedState)
	}
}

func TestAllocateTokenBudgetZeroFallback(t *testing.T) {
	budget := allocateTokenBudget(0, nil)
	if budget.Total() > 32000 {
		t.Fatalf("zero context window should fallback to 32k, got total %d", budget.Total())
	}
}
