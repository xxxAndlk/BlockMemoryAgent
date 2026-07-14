package textutil

import "regexp"

// 预编译敏感信息正则表达式，避免每次调用 RedactSensitive 时重新编译。
var (
	// bearerRe 匹配 Bearer Token 形式的 API 密钥：
	// 不区分大小写的 "bearer " 后跟至少 8 个合法 token 字符（字母/数字/_/-/.）。
	bearerRe = regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9_\-\.]{8,}\b`)
	// skRe 匹配以 "sk-" 开头的 OpenAI 风格密钥，后续至少 20 个字母或数字。
	skRe = regexp.MustCompile(`(?i)\b(sk-[a-z0-9]{20,})\b`)
	// akRe 匹配以 "ak-" 开头的访问密钥，后续至少 10 个字母或数字。
	akRe = regexp.MustCompile(`(?i)\b(ak-[a-z0-9]{10,})\b`)
)

// RedactSensitive 将 prompt/response 字符串中常见的 API 密钥模式脱敏。
//
// 参数: s 待脱敏的原始字符串。
// 返回: 脱敏后的字符串；未匹配到敏感模式时返回原串。
func RedactSensitive(s string) string {
	// 将 Bearer Token 的密钥部分替换为 ***，保留 "bearer " 前缀（通过 ${1} 引用）。
	s = bearerRe.ReplaceAllString(s, "${1}***")
	// 将整个 sk-xxx 密钥替换为 ***。
	s = skRe.ReplaceAllString(s, "***")
	// 将整个 ak-xxx 密钥替换为 ***。
	s = akRe.ReplaceAllString(s, "***")
	// 返回处理后的字符串。
	return s
}
