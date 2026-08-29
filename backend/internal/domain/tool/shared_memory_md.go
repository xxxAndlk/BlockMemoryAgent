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
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// MDFrontmatter 是共享记忆 MD 文件的 YAML 头部结构。
// 所有 slot 共用：spec slot 额外填 Goal/Acceptance/Constraints，其余 slot 仅填 AgentID/Slot/Files。
// Version 供 CAS 乐观锁（SetIfVersion）使用：每次写入自增，冲突检测用。
type MDFrontmatter struct {
	AgentID string           `yaml:"agent,omitempty"`
	Slot    string           `yaml:"slot,omitempty"`
	Files   map[string]int64 `yaml:"files,omitempty"`
	Version int              `yaml:"version,omitempty"`
	// InvalidatedAt 失效标记（RFC3339 时间戳）：普通共享记忆槽在所涉文件被
	// WriteFile/EditFile 修改后由 registry.invalidateSharedMemoryForPath 打上，
	// 不物理删除、body/Files 原样保留；dispatcher 注入侧据此判 stale，
	// 注入内容并附行号漂移警告（实证：物理删除导致下次派发从零重读同一批文件）。
	// spec 槽不走此字段（写墓碑 specTombstonePrefix，语义不变）。WriteSharedMemory
	// 重写时编码新 frontmatter，该标记自然清除。
	InvalidatedAt string `yaml:"invalidated_at,omitempty"`
	// FileList 是 spec.files 的完整原始清单（含当时不存在的待创建文件）。
	// Files map 只收录 stat 成功的文件（mtime 索引），创建类任务的待创建文件
	// 不在其中；dispatcher 冒烟检查（TODO #56）需要全量清单，读 FileList 兜底。
	FileList []string `yaml:"file_list,omitempty"`
	// Spec 专属字段：仅 slot == SpecSlot 时填充。
	// 其余 slot 这些字段为空，解码时忽略。
	Goal        string   `yaml:"goal,omitempty"`
	Acceptance  []string `yaml:"acceptance,omitempty"`
	Constraints []string `yaml:"constraints,omitempty"`
	// VerifyLevels 验收层级（TODO #59）：existence/static/integration/runtime/visual 子集。
	VerifyLevels []string `yaml:"verify_levels,omitempty"`
	// Contract 跨域契约（TODO #57），仅 spec slot 填充；nil 等价于未填。
	Contract *Contract `yaml:"contract,omitempty"`
	// Probes 运行时探针声明（TODO #67 runtime 层），仅 spec slot 填充。
	Probes []string `yaml:"probes,omitempty"`
	// Scenes 场景化截图清单（TODO #69 visual 层），仅 spec slot 填充。
	Scenes []string `yaml:"scenes,omitempty"`
	// Baseline 对标基线产物清单（TODO #75），仅 spec slot 填充。
	Baseline []string `yaml:"baseline,omitempty"`
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
		AgentID:      agentID,
		Slot:         SpecSlot,
		Files:        files,
		FileList:     spec.Files,
		Goal:         spec.Goal,
		Acceptance:   spec.Acceptance,
		Constraints:  spec.Constraints,
		VerifyLevels: spec.VerifyLevels,
		Contract:     spec.Contract,
		Probes:       spec.Probes,
		Scenes:       spec.Scenes,
		Baseline:     spec.Baseline,
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
		b.WriteByte('\n')
	}
	if len(s.VerifyLevels) > 0 {
		b.WriteString("## 验收层级\n")
		b.WriteString(strings.Join(s.VerifyLevels, " / "))
		b.WriteString("\n\n")
	}
	if len(s.Probes) > 0 {
		b.WriteString("## 运行时探针\n")
		for _, p := range s.Probes {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(p))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(s.Scenes) > 0 {
		b.WriteString("## 场景截图清单\n")
		for _, sc := range s.Scenes {
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(sc))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if len(s.Baseline) > 0 {
		b.WriteString("## 对标基线\n")
		for _, bl := range s.Baseline {
			b.WriteString("- `")
			b.WriteString(strings.TrimSpace(bl))
			b.WriteString("`\n")
		}
		b.WriteByte('\n')
	}
	if s.Contract != nil && !s.Contract.Empty() {
		b.WriteString("## 跨域契约\n")
		for _, sy := range s.Contract.Symbols {
			stubNote := ""
			if sy.Stub {
				stubNote = fmt.Sprintf(" 【占位桩,责任方: %s,须实装】", strings.TrimSpace(sy.Owner))
			}
			if len(sy.Refs) > 0 {
				fmt.Fprintf(&b, "- 符号 `%s`%s 声明于 `%s`，引用方: `%s`\n", sy.Symbol, stubNote, sy.File, strings.Join(sy.Refs, "`、`"))
			} else {
				fmt.Fprintf(&b, "- 符号 `%s`%s 声明于 `%s`\n", sy.Symbol, stubNote, sy.File)
			}
		}
		for _, id := range s.Contract.DOMIDs {
			fmt.Fprintf(&b, "- DOM id `%s` 声明于 `%s`\n", id.ID, id.File)
		}
		for i, sc := range s.Contract.Scripts {
			fmt.Fprintf(&b, "- script 顺序 %d: `%s`\n", i+1, sc.File)
		}
		for _, sg := range s.Contract.Signatures {
			fmt.Fprintf(&b, "- 签名 `%s`（%s）声明于 `%s`\n", sg.Signature, sg.Symbol, sg.File)
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
