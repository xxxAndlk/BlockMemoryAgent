package retriever

// chunker.go 实现 TODO #27 外部知识库摄入的文档切块：
// Markdown/纯文本按段落/标题切块，块间带重叠避免切碎语义。

import (
	"strings"
)

// Chunk 把文档文本切分为块。
//
// 策略（保持语义完整优先）：
//   - 先按行切分，优先在空行/标题（#/##/###）边界收块；
//   - 块达到 chunkRunes 上限时强制收块（避免超大段落撑爆单块）；
//   - 相邻块保留 overlapRunes 字符重叠（接续语义不丢）。
//
// 返回非空切片；空输入返回空切片。
func Chunk(text string, chunkRunes, overlapRunes int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if chunkRunes <= 0 {
		chunkRunes = 800
	}
	if overlapRunes < 0 {
		overlapRunes = 0
	}

	lines := splitLines(text)
	var chunks []string
	var buf []rune
	flush := func() {
		if len(buf) == 0 {
			return
		}
		chunks = append(chunks, string(buf))
		if len(buf) > overlapRunes {
			buf = append([]rune{}, buf[len(buf)-overlapRunes:]...)
		} else {
			buf = buf[:0]
		}
	}

	for _, line := range lines {
		lineRunes := []rune(line)
		if len(lineRunes) == 0 {
			// 空行：自然的块边界，尝试收块（仅当已有内容）。
			if len(buf) > 0 {
				flush()
			}
			continue
		}
		// 标题行：优先收块再开始新块（标题作为块首，利于检索定位）。
		if isHeading(line) && len(buf) > 0 {
			flush()
		}
		for len(lineRunes) > 0 {
			room := chunkRunes - len(buf)
			if room <= 0 {
				flush()
				continue
			}
			if len(lineRunes) <= room {
				buf = append(buf, lineRunes...)
				lineRunes = nil
			} else {
				// 单行超长：按 room 截断进当前块，剩余继续。
				buf = append(buf, lineRunes[:room]...)
				lineRunes = lineRunes[room:]
				flush()
			}
		}
	}
	if len(buf) > 0 {
		flush()
	}
	return chunks
}

// splitLines 按 \n 拆行。
func splitLines(text string) []string {
	raw := strings.Split(text, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, strings.TrimRight(l, "\r"))
	}
	return out
}

// isHeading 判断行是否为 Markdown 标题（# 开头 + 空白）。
func isHeading(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	// 至少一个 #，且其后是空格或行尾（排除 #### 更深层级按 ### 处理仍视为标题）。
	if len(trimmed) == 1 {
		return true
	}
	return trimmed[1] == ' ' || trimmed[1] == '#'
}
