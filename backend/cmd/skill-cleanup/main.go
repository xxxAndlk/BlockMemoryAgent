package main

// cmd/skill-cleanup 是一次性存量清洗工具（TODO 25 阶段 A2）：
// 扫描经验技能目录（config/skills_learned/*.md），对 步骤/坑点 段落中
// 渲染层叠加造成的双层行首序号做保守剥离（"1. 1. xxx" → "1. xxx"）。
//
// 保守策略：仅当一行匹配 doubleMarkerRe（行首 "N." 后紧跟又一层 "M." 序号
// 或 -/•/* 符号）时才剥离第二层；正文里合法的数字开头（如 "5. 已经带.的点"、
// "2024.09 数据"）不会误伤。生成层修复靠 stripLeadingMarker（bootstrap/evolver.go），
// 本工具只清存量文件。
//
// 用法：
//
//	go run ./cmd/skill-cleanup -dir ../config/skills_learned -dry-run

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// doubleMarkerRe 双层行首序号：行首空白 + "N." 后紧跟又一层序号（"M." / "M、" / "M)" 等）
// 或列表符号（-/•/*）。捕获组：①行首空白 ②第一层 "N. " ③正文起点。
var doubleMarkerRe = regexp.MustCompile(`^(\s*)(\d+\.\s+)(?:\d+\s*[.、．!)]|[-•*])\s*`)

// cleanChange 一处清洗记录（1-based 行号）。
type cleanChange struct {
	line   int
	before string
	after  string
}

// cleanContent 清洗单个文件内容：只在 步骤/坑点 段落内处理，返回新内容与变更清单。
// 无变更时返回原内容（保持字节级稳定，不写盘）。
func cleanContent(data []byte) ([]byte, []cleanChange) {
	lines := strings.Split(string(data), "\n")
	inSection := false
	var changes []cleanChange
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// "## " 标题行判定段落归属：进入/离开 步骤、坑点 段。
		if strings.HasPrefix(trimmed, "## ") {
			inSection = trimmed == "## 步骤" || trimmed == "## 坑点"
			continue
		}
		if !inSection {
			continue
		}
		m := doubleMarkerRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// 保留第一层 "N. " 与行首缩进，剥掉第二层序号/符号及其空白。
		after := m[1] + m[2] + strings.TrimSpace(line[len(m[0]):])
		lines[i] = after
		changes = append(changes, cleanChange{line: i + 1, before: line, after: after})
	}
	if len(changes) == 0 {
		return data, nil
	}
	return []byte(strings.Join(lines, "\n")), changes
}

// run 扫描 dir 下所有 .md 文件并清洗；dryRun 只报告不写盘。返回处理文件数与总变更数。
func run(dir string, dryRun bool, out io.Writer) (int, int, error) {
	var files, total int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fixed, changes := cleanContent(data)
		if len(changes) == 0 {
			return nil
		}
		files++
		fmt.Fprintf(out, "%s（%d 处）\n", path, len(changes))
		for _, c := range changes {
			fmt.Fprintf(out, "  行 %d: %q -> %q\n", c.line, c.before, c.after)
		}
		if dryRun {
			total += len(changes)
			return nil
		}
		if err := os.WriteFile(path, fixed, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		total += len(changes)
		return nil
	})
	return files, total, err
}

func main() {
	dir := flag.String("dir", "config/skills_learned", "经验技能目录（含 .md 技能文件）")
	dryRun := flag.Bool("dry-run", false, "只报告清洗内容，不写盘")
	flag.Parse()

	files, total, err := run(*dir, *dryRun, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	mode := "已清洗"
	if *dryRun {
		mode = "dry-run（未写盘）"
	}
	fmt.Printf("完成：%s，处理 %d 个文件，共 %d 处双层序号\n", mode, files, total)
}
