package bootstrap

// skill_tools.go 经验技能"技能携带工具"（TODO 25 阶段 C3 沉淀链路 + C2 加载注入辅助）。
//
// C3（Voyager 式"验证过才入库"）：evolver 产出的 tools [{filename, language, code, desc}]
// 固化为技能目录 scripts/ 前必须先过冒烟验证——filename 规范校验、code ≤60 行、
// 有运行时（py→python、sh→bash、js/ts→node）且冒烟执行成功（10s 超时）。
// 任一不满足 → 不建 scripts/ 目录，code 降级为正文末尾"附带脚本（未通过冒烟验证）"代码块，
// has_tools=false。通过的脚本写 <name>/scripts/<filename>（0644），frontmatter 补 tools 清单。
//
// C2（加载注入）：textutil.SkillToolSection 把 tools 渲染成「配套工具」段追加进
// types.Skill.Content——load_skill 取全文时 agent 可知每个脚本的相对路径/运行命令/用途。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// skillToolMaxLines code 行数上限（prompt 明示；超长只沉淀步骤+复现指引，不写 code）。
const skillToolMaxLines = 60

// skillToolMaxCount 单技能固化脚本数上限（防模型输出失控）。
const skillToolMaxCount = 3

// skillSmokeTimeout 冒烟执行预算（写入 scripts/ 前的一次真实执行）。
const skillSmokeTimeout = 10 * time.Second

// skillToolFileRe filename 规范：scripts/ 下小写字母数字连字符 + 点扩展名（py/sh/js/ts）。
var skillToolFileRe = regexp.MustCompile(`^scripts/[a-z0-9][a-z0-9-]*\.(py|sh|js|ts)$`)

// skillToolLanguage 语言小写归一（容忍模型返回 python/shell 等别名）。
func skillToolLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "py", "python", "python3":
		return "py"
	case "sh", "bash", "shell":
		return "sh"
	case "js", "javascript", "node":
		return "js"
	case "ts", "typescript":
		return "ts"
	}
	return ""
}

// skillToolRuntime 语言 → 冒烟执行器（第一个可用者）：
// py → python（回落 python3）；sh → bash（TODO C2：Windows 下 .sh 走 git-bash）；
// js/ts → node。无可用运行时返回空串（冒烟降级）。
var skillToolLookPath = exec.LookPath

func skillToolRuntime(language string) string {
	candidates := map[string][]string{
		"py": {"python", "python3"},
		"sh": {"bash", "sh"},
		"js": {"node"},
		"ts": {"node"},
	}[language]
	for _, c := range candidates {
		if p, err := skillToolLookPath(c); err == nil && p != "" {
			return c
		}
	}
	return ""
}

// skillSmokeRun 冒烟执行（可注入单测替身）：解释器跑一遍临时脚本，非零退出/超时返回 error。
var skillSmokeRun = func(ctx context.Context, exe, scriptPath string) error {
	cmd := exec.CommandContext(ctx, exe, scriptPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if se := stderr.String(); se != "" {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(se))
		}
		return err
	}
	return nil
}

// solidSkillTool 通过冒烟验证、可固化为 scripts/ 的脚本（= frontmatter tools 清单项）。
type solidSkillTool struct {
	meta textutil.SkillTool // {path, desc, run}
	code string             // 已验证的脚本全文（写盘用）
}

// failedSkillTool 未通过固化的脚本（正文附录代码块用）。
type failedSkillTool struct {
	filename string
	language string
	code     string
	desc     string
	reason   string // 降级原因（冒烟失败/无运行时/超 60 行/文件名不合法）
}

// solidifySkillTools C3 固化判定：逐条校验 + 冒烟，拆成可固化与降级两组 + 审计备注。
// 规则：仅本次会话实际用过且成功的脚本由模型产出（prompt 纪律）；这里做硬校验——
// filename 规范、code ≤60 行、有运行时、冒烟执行成功（10s）。全失败 → 两组皆空。
func solidifySkillTools(tools []agent.EvolvedSkillTool) (solid []solidSkillTool, failed []failedSkillTool, notes []string) {
	for _, t := range tools {
		if len(solid) >= skillToolMaxCount {
			notes = append(notes, fmt.Sprintf("超出单技能 %d 个上限，忽略 %s", skillToolMaxCount, t.Filename))
			break
		}
		filename := strings.TrimSpace(t.Filename)
		language := skillToolLanguage(t.Language)
		if language == "" || !skillToolFileRe.MatchString(filename) {
			failed = append(failed, failedSkillTool{filename, language, t.Code, t.Desc,
				fmt.Sprintf("文件名/语言不合法（%s/%s）", t.Filename, t.Language)})
			notes = append(notes, fmt.Sprintf("%s 文件名/语言不合法，不固化", filename))
			continue
		}
		if n := len(strings.Split(strings.TrimRight(t.Code, "\n"), "\n")); n > skillToolMaxLines {
			// 超长：只沉淀步骤+复现指引，不写 code（不进附录代码块）。
			notes = append(notes, fmt.Sprintf("%s 超 %d 行（%d 行），不固化不写 code", filename, skillToolMaxLines, n))
			continue
		}
		runtime := skillToolRuntime(language)
		if runtime == "" {
			failed = append(failed, failedSkillTool{filename, language, t.Code, t.Desc, "无可用运行时"})
			notes = append(notes, fmt.Sprintf("%s 无可用运行时，降级正文", filename))
			continue
		}
		smokeErr := runSkillSmoke(runtime, filename, t.Code)
		if smokeErr != nil {
			failed = append(failed, failedSkillTool{filename, language, t.Code, t.Desc, "冒烟失败：" + smokeErr.Error()})
			notes = append(notes, fmt.Sprintf("%s 冒烟失败，降级正文：%v", filename, smokeErr))
			continue
		}
		solid = append(solid, solidSkillTool{
			meta: textutil.SkillTool{Path: filename, Desc: strings.TrimSpace(t.Desc), Run: runtime + " " + filename},
			code: t.Code,
		})
		notes = append(notes, fmt.Sprintf("%s 冒烟通过（%s）", filename, runtime))
	}
	return solid, failed, notes
}

// runSkillSmoke 冒烟执行：code 写临时文件（按扩展名）→ 解释器执行一次（10s 超时）→ 清理。
// 有运行时且冒烟成功才允许调用方写 scripts/。
func runSkillSmoke(runtime, filename, code string) error {
	dir, err := os.MkdirTemp("", "bma-skill-smoke-*")
	if err != nil {
		return fmt.Errorf("mk smoke temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	scriptPath := filepath.Join(dir, filepath.Base(filename))
	if err := os.WriteFile(scriptPath, []byte(code), 0o644); err != nil {
		return fmt.Errorf("write smoke script: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), skillSmokeTimeout)
	defer cancel()
	return skillSmokeRun(ctx, runtime, scriptPath)
}

// writeSkillScripts 把通过冒烟的脚本写进 <技能目录>/scripts/（0644；执行只读即可，不设特殊权限）。
func writeSkillScripts(skillDir string, solid []solidSkillTool) error {
	if len(solid) == 0 {
		return nil
	}
	scriptsDir := filepath.Join(skillDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir scripts: %w", err)
	}
	for _, t := range solid {
		if err := os.WriteFile(filepath.Join(skillDir, t.meta.Path), []byte(t.code), 0o644); err != nil {
			return fmt.Errorf("write script %s: %w", t.meta.Path, err)
		}
	}
	return nil
}

// renderFailedToolAppendix 渲染正文末尾附录：未通过冒烟验证的脚本降级为代码块（仅供参考）。
// 无失败脚本返回空串。
func renderFailedToolAppendix(failed []failedSkillTool) string {
	if len(failed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 附带脚本（未通过冒烟验证，仅供参考）\n")
	for _, f := range failed {
		fmt.Fprintf(&b, "\n### %s — %s\n", f.filename, f.reason)
		if desc := strings.TrimSpace(f.desc); desc != "" {
			fmt.Fprintf(&b, "用途：%s\n\n", desc)
		}
		fmt.Fprintf(&b, "```%s\n%s\n```\n", f.language, strings.TrimRight(f.code, "\n"))
	}
	return b.String()
}

// solidToolMetas 提取固化脚本的 frontmatter 清单（{path, desc, run}）。
func solidToolMetas(solid []solidSkillTool) []textutil.SkillTool {
	out := make([]textutil.SkillTool, 0, len(solid))
	for _, t := range solid {
		out = append(out, t.meta)
	}
	return out
}
