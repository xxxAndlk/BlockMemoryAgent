package server

import "testing"

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
		got := redactSensitive(c.input)
		if got != c.want {
			t.Fatalf("redactSensitive(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
