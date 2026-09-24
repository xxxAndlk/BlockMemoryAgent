package bootstrap

// compaction_hooks.go 压缩生命周期钩子装配（TODO #20②+#21）。
// 两件配套：preCompactSnapshot 是"记录"（压掉了什么、账本当时什么状态——随压缩事件
// 落 agent_events 底账可 SQL 查回）；hotReinjectProvider 是"解药"（压缩后按最近改动
// 清单重读回灌，未提交热改动不随压缩蒸发——对齐 CC context-window "What survives
// compaction"：≤5 个文件、单文件过大只回路径引用；CC 社区 anthropics/claude-code#34674
// 压缩回退未提交 Edit 事故的同类防护）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
)

const (
	// compactionSnapshotMaxRunes 压缩前快照上限：指针级摘要，不存全文防 agent_events 体积通胀。
	compactionSnapshotMaxRunes = 800
	// hotReinjectMaxFiles 热改动回灌文件数上限（CC 口径 ≤5，最近修改优先）。
	hotReinjectMaxFiles = 5
	// hotReinjectFileTokenCap 单文件 token 上限：超出只回路径引用（内容不进上下文）。
	hotReinjectFileTokenCap = 5000
	// hotReinjectTotalTokenCap 回灌总预算（第三方源码分析 CC POST_COMPACT_TOKEN_BUDGET=50K 同款）。
	hotReinjectTotalTokenCap = 50000
)

// newCompactionSnapshot 构造压缩前状态快照（任务账本 + 拓扑名册 + 未读工具链指针级摘要）。
// 会话级账本按 agentID 前缀（顶层句柄即 sessionID）归位；全部 best-effort，失败段静默缺席。
func newCompactionSnapshot(d *subagent.Dispatcher, mb *mailbox.Mailbox) func(agentID string) string {
	return func(agentID string) string {
		if agentID == "" {
			return ""
		}
		sessionID := agentID
		if i := strings.Index(agentID, "/"); i > 0 {
			sessionID = agentID[:i]
		}
		var sb strings.Builder
		if brief := d.TaskLedgerBrief(sessionID); brief != "" {
			sb.WriteString("【任务账本】\n" + brief + "\n")
		}
		if topo := d.TopologyBrief(agentID); topo != "" {
			sb.WriteString("【拓扑名册】\n" + topo + "\n")
		}
		if mb != nil {
			if n := mb.Count(agentID); n > 0 {
				fmt.Fprintf(&sb, "【未读工具链】未读邮箱 %d 封（细节查 agent_events type=mailbox）\n", n)
			}
		}
		out := strings.TrimSpace(sb.String())
		if r := []rune(out); len(r) > compactionSnapshotMaxRunes {
			out = string(r[:compactionSnapshotMaxRunes]) + "…(快照截断)"
		}
		return out
	}
}

// newHotReinjectProvider 构造压缩后热改动重读回灌器：files（FilesModifiedFromHistory
// 产出，出现序）逆序取 ≤5 个最近改动文件从盘重读；单文件 >5K tokens 只回路径引用；
// 总量超 50K tokens 后余量同样只回路径。项目自述（CLAUDE.md/AGENTS.md）从盘重注入
// （CC "What survives compaction" 文档化项：项目级文件与记忆索引压缩后存活）。
func newHotReinjectProvider(workDir string) func(agentID string, files []string) string {
	return func(_ string, files []string) string {
		var sb strings.Builder
		total := 0
		// 逆序 = 最近修改优先（history 只追加，末尾是最新写入）。
		for i := len(files) - 1; i >= 0 && hotReinjectCount(&sb) < hotReinjectMaxFiles; i-- {
			p := files[i]
			body, err := readHotFile(workDir, p)
			if err != nil {
				// 文件已删/不可读：只回路径引用（删除本身也是信息）。
				fmt.Fprintf(&sb, "- %s（已不可读，保持路径引用）\n", p)
				continue
			}
			if total+model.EstimateTokens(body) > hotReinjectTotalTokenCap || model.EstimateTokens(body) > hotReinjectFileTokenCap {
				fmt.Fprintf(&sb, "- %s（内容过大，只回路径引用）\n", p)
				continue
			}
			total += model.EstimateTokens(body)
			fmt.Fprintf(&sb, "- %s（当前盘上内容）：\n%s\n", p, body)
		}
		// 项目自述从盘重注入（压缩不吞项目级约束）。
		for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
			body, err := readHotFile(workDir, name)
			if err != nil {
				continue
			}
			fmt.Fprintf(&sb, "- %s（项目自述，盘上重读）：\n%s\n", name, body)
		}
		out := strings.TrimSpace(sb.String())
		if out == "" {
			return ""
		}
		return "【热改动回灌】（压缩后从盘重读的文件现状，以盘上为准；被压缩段落的细节查 agent_events / session_events）\n" + out
	}
}

// hotReinjectCount 数已渲染的文件条目（"- " 开头行）。
func hotReinjectCount(sb *strings.Builder) int {
	return strings.Count(sb.String(), "\n- ") + strings.Count(sb.String(), "- ")
}

// readHotFile 读 workDir 相对（或绝对）路径的文件内容，超长预截断防单文件爆预算。
func readHotFile(workDir, path string) (string, error) {
	full := path
	if !filepath.IsAbs(path) && workDir != "" {
		full = filepath.Join(workDir, path)
	}
	body, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	s := string(body)
	// 预截断：8K runes 之外先裁，token 估算才有意义。
	if r := []rune(s); len(r) > 8000 {
		s = string(r[:8000]) + "\n…(截断)"
	}
	return s, nil
}
