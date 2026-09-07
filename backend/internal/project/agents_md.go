// agents_md.go 实现 workDir 根部项目自述文档的自动注入（TODO 第10项⑦，对标 AGENTS.md 冷启动）：
// 检测 AGENTS.md（优先）与 CLAUDE.md，注入【项目自述】段。两处消费方：
//   - agent 包 buildEnvBlock（meta/会话级系统提示词，per-agent 实例冻结）；
//   - dispatcher 派发前缀首位（子 Agent 任务前缀，会话内最稳的段）。
//
// mtime+size 缓存：文件未变不重读（派发高频路径，每次 stat 即可）；变化即失效重读。
// 文件缺失零开销（一次 stat）。maxRunes 截断防膨胀（AGENTS.md 可能塞进整份编码规范）。
package project

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// briefCacheEntry 是单路径的缓存条目：mtime+size 指纹 + 渲染后内容。
type briefCacheEntry struct {
	mtimeSec  int64
	mtimeNsec int64
	size      int64
	content   string
}

var (
	briefCacheMu sync.RWMutex
	briefCache   = map[string]briefCacheEntry{} // key: 绝对路径
)

// LoadProjectBrief 读取 workDir 根部的项目自述文档：AGENTS.md 优先、其次 CLAUDE.md。
// 返回截断至 maxRunes 的正文（调用方自行加段标题）；文件缺失/不可读/关闭（maxRunes<=0）
// 返回空串，调用方零注入。进程级 mtime 缓存：同一文件未变不重读，修改后自动失效。
func LoadProjectBrief(workDir string, maxRunes int) string {
	if workDir == "" || maxRunes <= 0 {
		return ""
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		p := filepath.Join(workDir, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		key, err := filepath.Abs(p)
		if err != nil {
			key = p
		}
		mtime := fi.ModTime()
		briefCacheMu.RLock()
		cached, ok := briefCache[key]
		briefCacheMu.RUnlock()
		if ok && cached.mtimeSec == mtime.Unix() && cached.mtimeNsec == int64(mtime.Nanosecond()) && cached.size == fi.Size() {
			return cached.content
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "" // 存在但读不了：不回退下一个候选（AGENTS.md 优先语义），零注入
		}
		content := truncateBriefRunes(string(data), maxRunes)
		briefCacheMu.Lock()
		briefCache[key] = briefCacheEntry{
			mtimeSec:  mtime.Unix(),
			mtimeNsec: int64(mtime.Nanosecond()),
			size:      fi.Size(),
			content:   content,
		}
		briefCacheMu.Unlock()
		return content
	}
	return ""
}

// truncateBriefRunes 按 rune 数截断，超限附截断标记（与画像/偏好注入同口径）。
func truncateBriefRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimRight(string(runes[:maxRunes]), " \t\n") + "\n…（已截断）"
}
