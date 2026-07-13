package server_test

import (
	"testing"

	"github.com/blockmemory/agent/backend/pkg/textutil"
)

func TestRedactSensitive(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			input: "Authorization: Bearer sk-abc12345678901234567890",
			want:  "Authorization: Bearer ***",
		},
		{
			input: "key=sk-abcdefghijklmnopqrstuvwxyz123456",
			want:  "key=***",
		},
		{
			input: "ak-1234567890abcdef",
			want:  "***",
		},
		{
			input: "普通文本不含 key",
			want:  "普通文本不含 key",
		},
	}

	for _, c := range cases {
		got := textutil.RedactSensitive(c.input)
		if got != c.want {
			t.Fatalf("RedactSensitive(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestParseTokenUsage(t *testing.T) {
	cases := []struct {
		msg       string
		wantIn    int
		wantOut   int
		wantCalls int // 此字段本测试未使用，仅保留结构清晰
	}{
		{
			msg:     "[MetaAgent] Token 消耗: in=123 out=456 dur=789ms",
			wantIn:  123,
			wantOut: 456,
		},
		{
			msg:     "[助手[代码助手]] Token 消耗: in=10 out=20 dur=30ms",
			wantIn:  10,
			wantOut: 20,
		},
		{
			msg:     "[助手[t]] blades agent 完成 dur=30ms",
			wantIn:  0,
			wantOut: 0,
		},
		{
			msg:     "",
			wantIn:  0,
			wantOut: 0,
		},
	}

	for _, c := range cases {
		gotIn, gotOut := textutil.ParseTokenUsage(c.msg)
		if gotIn != c.wantIn || gotOut != c.wantOut {
			t.Fatalf("ParseTokenUsage(%q) = (%d, %d), want (%d, %d)",
				c.msg, gotIn, gotOut, c.wantIn, c.wantOut)
		}
	}
}
