package main

import (
	"encoding/json"
	"strings"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func stringsReplaceAll(s string) string { return strings.ReplaceAll(s, "\n", " ⏎ ") }

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func stringsCount(s, sub string) int {
	c := 0
	idx := 0
	for {
		i := stringsIndex(s[idx:], sub)
		if i < 0 {
			break
		}
		c++
		idx += i + len(sub)
	}
	return c
}
