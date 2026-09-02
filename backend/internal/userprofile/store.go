// Package userprofile 实现记忆体系第四层：用户画像（TODO #28）。
//
// 与既有层的关系（防重合）：
//   - 外部知识库（#27）= 预置参考知识；块记忆 = 任务经验流；LLM wiki = 策略沉淀；
//     用户画像 = 「人」的结构化偏好档案。画像不进向量库、不作检索语料（小体量结构化偏好），
//     不写 wiki（非项目知识）、不写块记忆（非任务经验）。
//
// 存储：单文件（默认 config/user_profile.md，与 soul.md 对称），人可直接编辑、git 可追踪。
// 注入：仅 MetaAgent system prompt（带 token 上限截断），不下发子 Agent。
//
// 泛化（2026-09-02 偏好与自进化期 1）：Store 不再绑定用户画像一种文件——
// 模板头/归档小节经构造函数注入，用户画像与项目偏好（.bma/project_preferences.md）
// 各持一个实例，同一套 Append/Save/Merge 语义，不复制代码。
package userprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Profile 用户画像快照（原子读）。
type Profile struct {
	Path      string    // 文件路径
	Content   string    // 全文
	UpdatedAt time.Time // 最近加载时间
}

// DefaultTemplate 用户画像空文件首次写入的模板头。
const DefaultTemplate = "# 用户画像\n\n> 结构化偏好档案：与 soul.md（人格）对称。人可直接编辑；程序经 remember_preference 工具或会话结束提取追加。\n\n"

// ProjectTemplate 项目偏好（.bma/project_preferences.md）空文件首次写入的模板头。
// 注意：模板文本不得包含 "## " 前缀字样（Append 按该子串定位小节，会被引言行误命中）。
const ProjectTemplate = "# 项目偏好\n\n> 本项目约定与经验档案。小节「项目约定」人工维护（无时间戳的行程序永不改写）；小节「项目经验」会话结束自动沉淀。\n\n"

// DefaultArchiveSection 用户画像的归档小节名（被替换的旧自动行移入此处，可审计）。
const DefaultArchiveSection = "反馈记录"

// ProjectArchiveSection 项目偏好的归档小节名。
const ProjectArchiveSection = "经验归档"

// defaultArchiveCap 归档小节保留的最大条数（超出裁最旧，原始审计线索限量）。
const defaultArchiveCap = 20

// Store 结构化偏好文件存储：单文件，读多写少。
//
// 并发安全：current 用 atomic.Pointer 实现 RCU 风格读；写路径（Load/Append/Save/ApplyMerge）
// 通过 mutex 串行化磁盘写与原子发布。
type Store struct {
	mu           sync.Mutex // 写路径串行化
	path         string     // 文件路径
	template     string     // 空文件首次写入的模板头
	archiveSec   string     // Merge 归档小节名
	archiveCap   int        // 归档小节保留上限
	current      atomic.Pointer[Profile]
}

// NewStore 创建用户画像存储。仅记录路径，不触发 IO；调用方需显式 Load（缺失文件容忍）。
func NewStore(path string) *Store {
	return newStore(path, DefaultTemplate, DefaultArchiveSection, defaultArchiveCap)
}

// NewProjectStore 创建项目偏好存储（per workDir .bma/project_preferences.md）。
// 语义与用户画像 Store 完全一致，仅模板头与归档小节名不同。
func NewProjectStore(path string) *Store {
	return newStore(path, ProjectTemplate, ProjectArchiveSection, defaultArchiveCap)
}

func newStore(path, template, archiveSec string, archiveCap int) *Store {
	if strings.TrimSpace(template) == "" {
		template = DefaultTemplate
	}
	if archiveSec == "" {
		archiveSec = DefaultArchiveSection
	}
	if archiveCap <= 0 {
		archiveCap = defaultArchiveCap
	}
	return &Store{path: path, template: template, archiveSec: archiveSec, archiveCap: archiveCap}
}

// Load 读取磁盘画像并原子发布。文件缺失时发布空画像（不报错，服务可启动）。
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.current.Store(&Profile{Path: s.path})
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.Store(&Profile{Path: s.path, Content: string(data), UpdatedAt: time.Now()})
	return nil
}

// Reload 显式重载（用户编辑后热更新）。
func (s *Store) Reload() error { return s.Load() }

// Current 返回当前画像快照；未加载返回空画像（非 nil）。
func (s *Store) Current() *Profile {
	if p := s.current.Load(); p != nil {
		return p
	}
	return &Profile{Path: s.path}
}

// Path 返回画像文件路径。
func (s *Store) Path() string { return s.path }

// ArchiveSection 返回归档小节名。
func (s *Store) ArchiveSection() string { return s.archiveSec }

// Append 结构化追加一条画像记录：在指定小节（## 段）末尾追加带时间戳的行；
// 文件缺失/内容为空时先写入模板头。返回错误时不做任何修改。
//
// 双路写入共用此方法：
//   - 用户显式陈述（remember_preference 工具 / "记住我偏好 X"）→ 小节=偏好；
//   - 会话完成轻量模型提取的偏好增量（Merge 不可用时降级直写）→ 归档小节。
func (s *Store) Append(section, text string) error {
	text = strings.TrimSpace(text)
	section = strings.TrimSpace(section)
	if text == "" || section == "" {
		return fmt.Errorf("section and text are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	content := ""
	if cur := s.current.Load(); cur != nil {
		content = cur.Content
	}
	if strings.TrimSpace(content) == "" {
		content = s.template
	}

	// 定位小节（## <section>）；无该小节则文件末尾追加新小节。
	line := fmt.Sprintf("- %s（%s）", text, time.Now().Format("2006-01-02 15:04"))
	idx := strings.Index(content, "## "+section)
	if idx < 0 {
		content = strings.TrimRight(content, "\n") + "\n\n## " + section + "\n" + line + "\n"
	} else {
		// 小节范围：idx 到下一个 "\n## "（下一小节）或文件尾；在小节内容末尾插入。
		next := strings.Index(content[idx+3:], "\n## ")
		if next < 0 {
			content = strings.TrimRight(content, "\n") + "\n" + line + "\n"
		} else {
			insertAt := idx + 3 + next
			content = content[:insertAt] + line + "\n" + content[insertAt:]
		}
	}

	return s.writeLocked(content)
}

// Save 全量覆盖画像（HTTP PUT 用户手动编辑）。内容为空时写入模板头首行。
func (s *Store) Save(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(content) == "" {
		content = strings.SplitN(s.template, "\n", 2)[0] + "\n"
	}
	return s.writeLocked(content)
}

// writeLocked 落盘并原子发布（mu 已持有时调用）。
func (s *Store) writeLocked(content string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(s.path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	s.current.Store(&Profile{Path: s.path, Content: content, UpdatedAt: time.Now()})
	return nil
}
