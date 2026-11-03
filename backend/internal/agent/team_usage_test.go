package agent

// team_usage_test.go B-04/B-10：会话累计 token 用量与预算闸门。

import "testing"

func TestTeamTokenBudget(t *testing.T) {
	s := &ReactService{}
	// 未设预算：永不超限，但用量仍累加。
	s.recordSessionTokens("s1", 50, 40)
	if in, out := s.TeamTokenUsage("s1"); in != 50 || out != 40 {
		t.Fatalf("usage = (%d,%d), want (50,40)", in, out)
	}
	if s.TeamBudgetExceeded("s1") {
		t.Fatal("0 budget must never be exceeded")
	}

	s.SetTeamTokenBudget(100)
	if s.TeamBudgetExceeded("s1") {
		t.Fatal("90 < 100 must not be exceeded")
	}
	s.recordSessionTokens("s1", 10, 0)
	if !s.TeamBudgetExceeded("s1") {
		t.Fatal("100 >= 100 must be exceeded")
	}
	// 其他会话互不影响。
	if s.TeamBudgetExceeded("s2") {
		t.Fatal("other session must not share usage")
	}
	s.ResetTeamTokenUsage("s1")
	if in, out := s.TeamTokenUsage("s1"); in != 0 || out != 0 {
		t.Fatalf("reset usage = (%d,%d), want (0,0)", in, out)
	}
}
