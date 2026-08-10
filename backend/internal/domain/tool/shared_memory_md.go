package tool

// shared_memory_md.go 提供共享记忆的 MD 文件编解码。
//
// 替代旧的 SharedEntry JSON 格式：所有共享记忆/spec 落盘到 .bma/shared/ 下 MD 文件，
// frontmatter（YAML）存机器可读字段（agent/slot/files mtime/spec 结构化字段），
// body 存人读文本（spec 为结构化 MD，shared memory 为自由 content）。
//
// 设计权衡：
//   - frontmatter 用 YAML：go.mod 已依赖 gopkg.in/yaml.v3，无新依赖；map[string]int64
//     原生支持，files mtime 直接序列化；
//   - spec 结构化字段同时存 frontmatter 与 body：injectSpec/hasFreshSpec 读 frontmatter
//     不解析 body（O(1) 解码），body 仅供人读与 git diff；
//   - 单一 decodeSharedMD 函数供 dispatcher/registry 复用，避免多份解析逻辑漂移。

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// MDFrontmatter 是共享记忆 MD 文件的 YAML 头部结构。
// 所有 slot 共用：spec slot 额外填 Goal/Acceptance/Constraints，其余 slot 仅填 AgentID/Slot/Files。
// Version 供 CAS 乐观锁（SetIfVersion）使用：每次写入自增，冲突检测用。
type MDFrontmatter struct {
	AgentID string `yaml:"agent,omitempty"`
	Slot    string `yaml:"slot,omitempty"`
	Files   map[string]int64 `yaml:"files,omitempty"`
	Version int    `yaml:"version,omitempty"`
	// Spec 专属字段：仅 slot == SpecSlot 时填充。
	// 其余 slot 这些字段为空，解码时忽略。
	Goal        string   `yaml:"goal,omitempty"`
	Acceptance  []string `yaml:"acceptance,omitempty"`
	Constraints []string `yaml:"constraints,omitempty"`
}

// EncodeSharedMD 把 shared memory 槽位编码为 MD（frontmatter + body=content）。
// agentID/slot 进 frontmatter 供调试溯源；files mtime 进 frontmatter 供失效校验。
// 导出让 subagent 包测试可构造 MD fixture，避免重复实现编码逻辑。
func EncodeSharedMD(agentID, slot string, files map[string]int64, body string) string {
	fm := MDFrontmatter{
		AgentID: agentID,
		Slot:    slot,
		Files:   files,
	}
	return encodeMD(fm, body)
}

// encodeSharedMD 是 EncodeSharedMD 的包内别名，保留旧调用点不改。
func encodeSharedMD(agentID, slot string, files map[string]int64, body string) string {
	return EncodeSharedMD(agentID, slot, files, body)
}

// EncodeSpecMD 把 Spec 编码为 MD：frontmatter 含 goal/acceptance/constraints/files mtime，
// body 为人读结构化 MD（# 任务规范 / ## 目标 / ## 验收条件 / ## 约束 / ## 涉及文件）。
// frontmatter 字段供 injectSpec/hasFreshSpec 直接读取，body 仅供人读与 git diff。
// 导出让 subagent 包测试可构造 spec MD fixture。
func EncodeSpecMD(agentID string, spec Spec, files map[string]int64) string {
	fm := MDFrontmatter{
		AgentID:     agentID,
		Slot:        SpecSlot,
		Files:       files,
		Goal:        spec.Goal,
		Acceptance:  spec.Acceptance,
		Constraints: spec.Constraints,
	}
	return encodeMD(fm, renderSpecBody(spec))
}

// encodeSpecMD 是 EncodeSpecMD 的包内别名，保留旧调用点不改。
func encodeSpecMD(agentID string, spec Spec, files map[string]int64) string {
	return EncodeSpecMD(agentID, spec, files)
}

// renderSpecBody 渲染 spec 的人读 MD body。
// 与 dispatcher.renderSpecPrefix 不同：此处为人读视图（## 标题 + 列表），
// renderSpecPrefix 是注入子 Agent task 前缀的紧凑视图（目标:/验收:/约束:）。
func renderSpecBody(s Spec) string {
	var b strings.Builder
	b.WriteString("# 任务规范\n\n")

	b.WriteString("## 目标\n")
	b.WriteString(strings.TrimSpace(s.Goal))
	b.WriteString("\n\n")

	if len(s.Acceptance) > 0 {
		b.WriteString("## 验收条件\n")
		for _, a := range s.Acceptance {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(a))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(s.Constraints) > 0 {
		b.WriteString("## 约束\n")
		for _, c := range s.Constraints {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(c))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(s.Files) > 0 {
		b.WriteString("## 涉及文件\n")
		for _, f := range s.Files {
			b.WriteString("- `")
			b.WriteString(strings.TrimSpace(f))
			b.WriteString("`\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// encodeMD 写 frontmatter（YAML）+ body。
// frontmatter 用 yaml.Marshal 序列化；body 原样拼接。
func encodeMD(fm MDFrontmatter, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	if data, err := yaml.Marshal(&fm); err == nil {
		b.Write(data)
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// DecodeSharedMD 解析 MD 字符串，返回 frontmatter 与 body。
// 解析失败（无 frontmatter / YAML 错误）返回 ok=false。
// 供 dispatcher.injectSpec/injectKVMemory/hasFreshSpec 与 registry.invalidateSharedMemoryForPath 复用。
func DecodeSharedMD(raw string) (MDFrontmatter, string, bool) {
	var fm MDFrontmatter
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "---") {
		return fm, "", false
	}
	// 去掉首部 "---" 行
	rest := strings.TrimPrefix(s, "---")
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	// 找闭合 "---"
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, "", false
	}
	fmRaw := rest[:end]
	body := strings.TrimSpace(rest[end+len("\n---"):])
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return fm, "", false
	}
	return fm, body, true
}
