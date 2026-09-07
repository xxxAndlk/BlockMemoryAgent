// imageutil.go 实现工具图片的统一降采样（TODO 第9项④）：
// 全部工具图片的唯一汇流点是 toolRegistryAdapter.Dispatch（截图/截图类 MCP 工具的
// ResultImage 都经此映射）。png/jpeg 长边超过上限时等比缩小（不放大小图），原图落盘
// <workDir>/.bma/images/ 供追溯；gif/webp 等其他格式与解码/缩放失败一律原图直通
//（截图主流格式是 png/jpeg，覆盖面足够；缩放失败不应阻断工具结果回传）。
//
// 缩放为确定性纯函数（整数 box-filter，同源同参必得同字节），保证
// visualEvidenceCheck 的指纹去重语义不变：同一张截图前后两次缩放结果逐字节一致。
package agent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// imageMaxEdgeCfg 工具图片长边降采样上限（像素）；<=0 关闭降采样。
// 包级旋钮而非构造参数：工具注册表适配器在多个装配点构造（meta/子 Agent/测试），
// 改构造签名侵入面过大；该值进程生命周期内不变，bootstrap 启动时注入一次。
var imageMaxEdgeCfg atomic.Int64

// SetImageMaxEdge 注入工具图片长边降采样上限（像素）；<=0 关闭。由 bootstrap 调用。
func SetImageMaxEdge(px int) {
	imageMaxEdgeCfg.Store(int64(px))
}

// downsampleResultImages 对工具结果携带的图片做降采样与原图落盘（TODO 第9项④）。
// 逐张处理，单张失败不影响其余图片与结果本体；关闭（maxEdge<=0）时零行为。
// 原图落盘成功时在 Output 追加「原图已落盘」行，让模型/用户知道盘上有原图可查。
func downsampleResultImages(ctx context.Context, toolName string, result *ToolResult, maxEdge int) {
	if maxEdge <= 0 || len(result.Images) == 0 {
		return
	}
	for i := range result.Images {
		img := &result.Images[i]
		raw, err := base64.StdEncoding.DecodeString(string(img.Data))
		if err != nil || len(raw) == 0 {
			continue // 链路下游（ToBladesMessages）同样按解码失败跳过，语义一致
		}
		scaled, format := scaleImageLongEdge(raw, maxEdge)
		if scaled == nil {
			continue // 非缩放格式（gif/webp 等）/小图/坏图：原图直通
		}
		// 原图落盘（best-effort）：workDir 取 ctx 会话目录；无目录时跳过落盘仍降采样。
		if wd := tool.WorkDirFromContext(ctx); wd != "" {
			if path, err := saveOriginalImage(wd, raw, format); err == nil {
				result.Output += "\n\n原图已落盘: " + path
			} else {
				log.Printf("[react] image original save failed: tool=%s err=%v", toolName, err)
			}
		}
		img.Data = []byte(base64.StdEncoding.EncodeToString(scaled))
		img.MIMEType = formatMIME(format)
	}
}

// scaleImageLongEdge 解码图片并按需缩放：png/jpeg 且长边 > maxEdge 时等比缩到长边
// = maxEdge（不放大小图），返回缩放后字节与格式名（"png"/"jpeg"）；无需缩放/无法
// 解码返回 nil（调用方原图直通）。
func scaleImageLongEdge(raw []byte, maxEdge int) ([]byte, string) {
	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ""
	}
	if format != "png" && format != "jpeg" {
		return nil, "" // gif/webp 等直通（image/gif/webp 未注册解码器时 decode 已失败）
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := max(w, h)
	if long <= maxEdge || long == 0 {
		return nil, "" // 长边已达上限：不动原图，保持指纹/字节稳定
	}
	ratio := float64(maxEdge) / float64(long)
	dw := max(1, int(math.Round(float64(w)*ratio)))
	dh := max(1, int(math.Round(float64(h)*ratio)))
	dst := boxFilter(src, dw, dh)
	var out bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 90}); err != nil {
			return nil, ""
		}
	} else {
		if err := png.Encode(&out, dst); err != nil {
			return nil, ""
		}
	}
	return out.Bytes(), format
}

// boxFilter 整数盒式滤波缩放（确定性）：目标像素取源图对应矩形区域的均值。
// RGBA() 返回 16bit 分量，累加均值后折算 8bit。同源同参产出逐字节一致的图像，
// 保证 visualEvidenceCheck 指纹去重不因缩放引入抖动。
func boxFilter(src image.Image, dw, dh int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0 := y * sh / dh
		sy1 := (y + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		if sy1 > sh {
			sy1 = sh
		}
		for x := 0; x < dw; x++ {
			sx0 := x * sw / dw
			sx1 := (x + 1) * sw / dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sx1 > sw {
				sx1 = sw
			}
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(bl / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}

// saveOriginalImage 把原图写入 <workDir>/.bma/images/<unix>-<sha1前8>.<ext>，返回绝对路径。
// 文件名带内容摘要：同图多次落盘幂等可追溯，异图不覆盖。
func saveOriginalImage(workDir string, raw []byte, format string) (string, error) {
	ext := ".png"
	if format == "jpeg" {
		ext = ".jpg"
	}
	dir := filepath.Join(workDir, ".bma", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := fmt.Sprintf("%x", sha1.Sum(raw))[:8]
	path := filepath.Join(dir, fmt.Sprintf("%d-%s%s", time.Now().Unix(), sum, ext))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// formatMIME 把 image.Decode 的格式名映射回 MIMEType。
func formatMIME(format string) string {
	if format == "jpeg" {
		return "image/jpeg"
	}
	return "image/png"
}
