package textutil

import (
	"strconv"
	"strings"
	"time"
)

// ParseTokenUsage extracts input/output token counts from a token-usage message.
// Expected markers: "in=<n>" and "out=<n>".
func ParseTokenUsage(msg string) (in, out int) {
	in = extractIntAfter(msg, "in=")
	out = extractIntAfter(msg, "out=")
	return
}

// ParseDurationFromTokenUsage extracts a duration from a token-usage message.
// Expected marker: "dur=<duration>".
func ParseDurationFromTokenUsage(msg string) time.Duration {
	idx := strings.Index(msg, "dur=")
	if idx < 0 {
		return 0
	}
	start := idx + len("dur=")
	end := start
	for end < len(msg) && msg[end] != ' ' && msg[end] != '\t' {
		end++
	}
	if end <= start {
		return 0
	}
	if dur, err := time.ParseDuration(msg[start:end]); err == nil {
		return dur
	}
	return 0
}

func extractIntAfter(s, marker string) int {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return 0
	}
	start := idx + len(marker)
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	n := 0
	signed := false
	if start < len(s) && s[start] == '-' {
		signed = true
		start++
	}
	for start < len(s) && s[start] >= '0' && s[start] <= '9' {
		n = n*10 + int(s[start]-'0')
		start++
	}
	if signed {
		n = -n
	}
	return n
}

// ParseInt is a thin wrapper around strconv.Atoi that returns 0 on error.
func ParseInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
