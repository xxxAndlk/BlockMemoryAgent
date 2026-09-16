// capability_err.go 识别"模型不支持图片输入"类 provider 错误。
// 背景（2026-09-16 实证）：图片经 ReadMedia / 插件截图透传塞进对话后，下一轮请求
// 带 anthropic image block，无视觉模型（ark 网关 glm/deepseek 系）回 400
// {"error":{"code":"InvalidParameter","message":"Model do not support image input..."}}。
// 该错误是环境性能力缺失、非瞬时故障：既不该重试，也不该把整个 run 判死
// （react_agent 侧据此剥图降级继续；retry 侧据此快速失败）。
package model

import "strings"

// IsImageInputUnsupported 判定错误是否为"当前模型不支持图片输入"。
// 匹配 provider 错误文本（各 provider 均把状态码与原始响应体带进错误串）：
//   - 含 "not support image"（覆盖 "do not support image input" / "does not support image"）；
//   - 或同时含 "support image input" 与 "400"，兜住措辞变体。
func IsImageInputUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not support image") {
		return true
	}
	return strings.Contains(msg, "support image input") && strings.Contains(msg, "400")
}
