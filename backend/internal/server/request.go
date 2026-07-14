package server

import (
	"encoding/json" // JSON 解码
	"io"            // 读取请求体
	"net/http"      // HTTP 请求
	"unicode/utf8"  // UTF-8 校验
)

// errInvalidUTF8 是请求体包含非法 UTF-8 时的单例错误。
var errInvalidUTF8 = jsonInvalidUTF8Error{}

// jsonInvalidUTF8Error 表示请求体不是合法 UTF-8 的错误类型。
type jsonInvalidUTF8Error struct{}

// Error 实现 error 接口，返回可读错误信息。
func (jsonInvalidUTF8Error) Error() string { return "request body must be valid UTF-8" }

// DecodeBody 读取并解码 JSON 请求体到类型 T 的值。
// 如果请求体不是合法 UTF-8，会返回 errInvalidUTF8。
// 类型参数 T：目标类型。
// 参数 r：HTTP 请求。
// 返回值：解码后的 T 与错误。
func DecodeBody[T any](r *http.Request) (T, error) {
	var v T
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return v, err
	}
	if !utf8.Valid(raw) {
		return v, errInvalidUTF8
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}
