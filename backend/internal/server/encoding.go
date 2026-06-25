package server

import (
	"encoding/json"
	"io"
	"unicode/utf8"
)

// DecodeJSONRequest 解析 JSON 请求体，强制 UTF-8。
// 设计意图：项目统一 UTF-8，客户端必须以 UTF-8 发送 body。非 UTF-8 字节
// 视为非法请求，直接拒绝（400）。
//
// 参数：r - HTTP 请求体；v - 解析目标指针。
// 返回：解析错误（IO / 编码 / JSON 语法）。
func DecodeJSONRequest(r io.Reader, v any) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		return errInvalidUTF8
	}
	return json.Unmarshal(raw, v)
}

var errInvalidUTF8 = jsonInvalidUTF8Error{}

type jsonInvalidUTF8Error struct{}

func (jsonInvalidUTF8Error) Error() string { return "request body must be valid UTF-8" }
