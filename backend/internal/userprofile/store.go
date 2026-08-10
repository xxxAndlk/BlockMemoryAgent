// Package userprofile 实现记忆体系第四层：用户画像（TODO #28）。
//
// 与既有层的关系（防重合）：
//   - 外部知识库（#27）= 预置参考知识；块记忆 = 任务经验流；LLM wiki = 策展沉淀；
//     用户画像 = 「人」的结构化偏好档案。画像不进向量库、不作检索语料（小体量结构化偏好），
//     不写 wiki（非项目知识）、不写块记忆（非任务经验）。
//
// 存储：单文件 config/user_profile.md（与 soul.md 对称），人可直接编辑、git 可追踪。
// 注入：仅 MetaAgent system prompt（带 token 上限截断），不下发子 Agent。
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

// Store 用户画像存储：单文件，读多写少。
//
// 并发安全：current 用 atomic.Pointer 实现 RCU 风格读；写路径（Load/Append/Save）
// 通过 mutex 串行化磁盘写与原子发布。
type Store struct {
	mu      sync.Mutex            // 写路径串行化
	path    string                // user_profile.md 路径
	current atomic.Pointer[Profile]
}

// NewStore 创建画像存储。仅记录路径，不触发 IO；调用方需显式 Load（缺失文件容忍）。
func NewStore(path string) *Store {
	return &Store{path: path}
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

// Append 结构化追加一条画像记录：在指定小节（## 段）末尾追加带时间戳的行；
// 文件缺失/内容为空时先写入模板头。返回错误时不做任何修改。
//
// 双路写入共用此方法：
//   - 用户显式陈述（remember_preference 工具 / "记住我偏好 X"）→ 小节=偏好；
//   - 会话完成轻量模型提取的偏好增量 → 小节=反馈记录。
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
		content = "# 用户画像\n\n> 结构化偏好档案：与 soul.md（人格）对称。人可直接编辑；程序经 remember_preference 工具或会话结束提取追加。\n\n"
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

	if err := s.writeLocked(content); err != nil {
		return err
	}
	return nil
}

// Save 全量覆盖画像（HTTP PUT 用户手动编辑）。内容为空时写入模板头。
func (s *Store) Save(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(content) == "" {
		content = "# 用户画像\n"
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
