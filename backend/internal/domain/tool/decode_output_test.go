package tool

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TestDecodeCommandOutput_UTF8Passthrough 验证合法 UTF-8 输出原样返回（不触发 GBK 回退）。
func TestDecodeCommandOutput_UTF8Passthrough(t *testing.T) {
	in := "56:     this._rafId = null; // requestAnimationFrame 句柄（stop 时 cancel）"
	if got := DecodeCommandOutput([]byte(in)); got != in {
		t.Fatalf("UTF-8 输出应原样返回，got %q", got)
	}
}

// TestDecodeCommandOutput_GBKFallback 验证中文 Windows 控制台的 GBK 输出被正确解码为 UTF-8，
// 不再出现 � 替换字符（实证：Select-String/findstr 在代码页 936 下输出 GBK 字节）。
func TestDecodeCommandOutput_GBKFallback(t *testing.T) {
	want := "句柄（stop 时 cancel）"
	gbk, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(want))
	if err != nil {
		t.Fatalf("GBK 编码构造失败: %v", err)
	}
	got := DecodeCommandOutput(gbk)
	if got != want {
		t.Fatalf("GBK 输出应解码回原文，want %q got %q", want, got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("解码结果不应包含替换字符: %q", got)
	}
}

// TestDecodeCommandOutput_Empty 验证空输出安全返回。
func TestDecodeCommandOutput_Empty(t *testing.T) {
	if got := DecodeCommandOutput(nil); got != "" {
		t.Fatalf("空输入应返回空串，got %q", got)
	}
}
