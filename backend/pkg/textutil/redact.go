package textutil

import "regexp"

var (
	bearerRe = regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9_\-\.]{8,}\b`)
	skRe     = regexp.MustCompile(`(?i)\b(sk-[a-z0-9]{20,})\b`)
	akRe     = regexp.MustCompile(`(?i)\b(ak-[a-z0-9]{10,})\b`)
)

// RedactSensitive masks common API-key patterns in prompt/response strings.
func RedactSensitive(s string) string {
	s = bearerRe.ReplaceAllString(s, "${1}***")
	s = skRe.ReplaceAllString(s, "***")
	s = akRe.ReplaceAllString(s, "***")
	return s
}
