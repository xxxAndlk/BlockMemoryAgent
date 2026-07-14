package tool

// 导入所需标准库：bytes 用于无 HTML 转义的 JSON 序列化；context 用于进度回调上下文；
// encoding/json 用于 JSON 编码；strings 用于 UTF-8 修复；unicode/utf8 用于判断字节序列是否为合法 UTF-8。
import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Result 表示一次工具调用的结果。
type Result struct {
	// Tool 是被调用工具的名称。
	Tool string `json:"tool"`
	// Success 标记本次工具调用是否成功。
	Success bool `json:"success"`
	// Output 记录工具调用产生的标准输出或返回内容。
	Output string `json:"output"`
	// Error 记录工具调用失败时的错误信息；无错误时为空。
	Error string `json:"error,omitempty"`
	// Path 记录工具操作所针对的文件或目录路径。
	Path string `json:"path,omitempty"`
	// ArgsJSON 记录工具调用参数的 JSON 序列化形式。
	ArgsJSON string `json:"args_json,omitempty"`
	// SessionID 记录本次调用所属的会话标识。
	SessionID string `json:"session_id,omitempty"`
	// IsTemporary 标记本次调用是否使用了临时目录或临时资源。
	IsTemporary bool `json:"is_temporary,omitempty"`
	// TempDir 记录本次调用使用的临时目录路径。
	TempDir string `json:"temp_dir,omitempty"`
}

// ProgressEvent 表示代理执行过程中的单个进度事件，用于通知观察者（如 UI、日志等）。
type ProgressEvent struct {
	// SessionID 记录产生该事件的会话标识。
	SessionID string
	// Kind 表示事件的类型或阶段。
	Kind string
	// Agent 表示产生该事件的代理名称。
	Agent string
	// Tool 表示当前事件关联的工具名称。
	Tool string
	// Message 记录事件的主要提示信息。
	Message string
	// Detail 记录事件的补充细节信息。
	Detail string
}

// ProgressCallback 是接收进度事件的回调函数类型。
// ctx 用于传递上下文和取消信号。
// ev 为要处理的进度事件。
type ProgressCallback func(ctx context.Context, ev ProgressEvent)

// marshalNoHTMLEscape 将 v 序列化为 JSON，且禁用 HTML 转义。
// v 为待序列化的任意值。
// 返回序列化后的字节切片；如果序列化失败则返回错误。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	// buf 用于接收编码器写入的 JSON 字节流。
	var buf bytes.Buffer
	// enc 是基于 buf 的 JSON 编码器。
	enc := json.NewEncoder(&buf)
	// 禁用 HTML 转义，避免将 <、>、& 等字符转义为 Unicode 实体。
	enc.SetEscapeHTML(false)
	// 将 v 编码到 buf 中；若编码失败，立即返回 nil 和错误。
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// out 取出 buf 中已编码的字节序列。
	out := buf.Bytes()
	// 如果序列化结果末尾存在换行符，则将其截断，保证返回紧凑的 JSON。
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	// 返回处理后的 JSON 字节切片和 nil 错误。
	return out, nil
}

// SanitizeBytes 将字节切片转换为合法的 UTF-8 字符串。
// b 为待检查的原始字节切片。
// 返回有效的 UTF-8 字符串；若原字节序列不合法，则用替换字符处理非法部分。
func SanitizeBytes(b []byte) string {
	// 如果 b 已经是合法的 UTF-8，直接转换为字符串返回，避免额外拷贝。
	if utf8.Valid(b) {
		return string(b)
	}
	// 否则将字节切片转为字符串，并用 "�" 替换所有非法 UTF-8 序列。
	return strings.ToValidUTF8(string(b), "�")
}
