package subagent

// reuse_guard.go 实现新建 domain 派发的热驻复用守卫（实证 2026-08-26：jiujie 换端口
// 任务新建近义 domain jiujie-port80 而非复用热驻 Agent，冷启动重烧全部领域上下文）。
// 空闲清单注入与工具描述均为软提示，注意力被 spec 校验吸走时会被跳过；
// 本守卫在 dispatcher 代码层拦截（同 spec 强制校验/接力熔断先例）：
//   - 信号 1（命名包含）：新 domain 与某热驻槽 domain 全串包含（case-insensitive，
//     短侧 >= 2 rune）--jiujie-port80 ⊃ jiujie 命中；game-core 与 game-ui 互不包含。
//   - 信号 2（文件重叠）：本次派发 spec 的 files 与槽近期写入文件（lastWrites 追踪）
//     路径重叠（归一化后相等或互为目录后缀，容忍 abs/rel 差异）。
//   - 逃生口：task 含【新领域声明】标记放行（同接力熔断【接力理由】先例），
//     防误伤真新领域；最坏情况退化为现状（声明后照常新建）。
// 热驻未开启时零行为变化。

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// newDomainDeclareMarker 是确属新领域的逃生口标记（前缀匹配，兼容
// 【新领域声明】与【新领域声明：理由…】两种写法）：task 含该标记时守卫放行。
const newDomainDeclareMarker = "【新领域声明"

// checkIdleDomainReuse 新建 domain 派发的复用守卫：疑似与某热驻槽同目标时返回
// 拒绝文案（含 reuse_agent_id 指引）；无匹配/已声明/热驻未开启返回空串放行。
// 调用点：dispatchOne 的 roleID=="domain" 分支（reuse 分流已在更早处 return）。
func (d *Dispatcher) checkIdleDomainReuse(ctx context.Context, parentID, domain, task string) string {
	domain = strings.TrimSpace(domain)
	if !d.hotEnabled() || domain == "" || strings.Contains(task, newDomainDeclareMarker) {
		return ""
	}
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" {
		sid = sessionIDFromAgentID(parentID)
	}
	if sid == "" {
		return ""
	}
	specFiles := d.dispatchSpecFiles(ctx, parentID, domain)
	for _, s := range d.pool.slots(sid) {
		var signals []string
		if domainNameOverlap(domain, s.domain) {
			signals = append(signals, fmt.Sprintf("命名与热驻领域 %q 近似", s.domain))
		}
		if files := overlapFileList(specFiles, d.recentWrittenFiles(s.id, idleRosterFileWindow)); len(files) > 0 {
			if len(files) > 3 {
				files = files[:3]
			}
			signals = append(signals, "spec 文件与其近期写入重叠: "+strings.Join(files, ", "))
		}
		if len(signals) == 0 {
			continue
		}
		return reuseGuardMessage(s, domain, signals)
	}
	return ""
}

// reuseGuardMessage 渲染拒绝文案：给 reuse_agent_id 指引 + 新领域声明逃生口。
func reuseGuardMessage(s *domainSlot, domain string, signals []string) string {
	resp := ""
	if r := strings.TrimSpace(s.responsibility); r != "" {
		resp = "，职责: " + truncateRunes(r, 80)
	}
	return fmt.Sprintf(
		"复用守卫：新建 domain %q 与热驻领域 Agent 疑似同一目标（id=%s，领域=%q%s；信号: %s）。"+
			"若这是对该领域的后续修改（改端口/改配置/修 bug/加功能/调样式），必须复用而非新建："+
			"call_sub_agent(reuse_agent_id=%s, task=新任务)，保留其全部上下文与领域知识，禁止新建近义 domain。"+
			"确属全新领域时，在 task 末尾追加一行【新领域声明：与该领域无关，理由…】后重派即可放行。",
		domain, s.id, s.domain, resp, strings.Join(signals, "；"), s.id)
}

// domainNameOverlap 判断两个 domain 名是否疑似同目标：相等，或全串包含
//（case-insensitive，短侧 >= 2 rune 防单字符误命中）。token 前缀重叠不算
//（game-core vs game-ui 不命中）。
func domainNameOverlap(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if utf8.RuneCountInString(short) < 2 {
		return false
	}
	return strings.Contains(long, short)
}

// dispatchSpecFiles 取本次派发对应的 spec files（文件重叠信号源）：
// 依次试 domain 键、遗留单键、唯一 keyed spec——回退链与 hasFreshSpec 同口径
// （resolveSpecKeyCandidates 单源）。
// sharedMem 缺失/spec 解析失败返回 nil（信号静默降级，不阻塞派发）。
func (d *Dispatcher) dispatchSpecFiles(ctx context.Context, parentID, domain string) []string {
	if d.sharedMem == nil {
		return nil
	}
	for _, key := range d.resolveSpecKeyCandidates(ctx, parentID, domain) {
		if files := specFilesOfKey(ctx, d.sharedMem, key); len(files) > 0 {
			return files
		}
	}
	return nil
}

// specFilesOfKey 解析单个 spec 键的 files 字段（墓碑/解析失败返回 nil）。
func specFilesOfKey(ctx context.Context, store tool.SharedMemoryStore, key string) []string {
	val, err := store.Get(ctx, key)
	if err != nil || strings.TrimSpace(val) == "" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(val), tool.SpecTombstonePrefix) {
		return nil
	}
	fm, _, ok := tool.DecodeSharedMD(val)
	if !ok {
		return nil
	}
	if len(fm.FileList) > 0 {
		return fm.FileList
	}
	files := make([]string, 0, len(fm.Files))
	for p := range fm.Files {
		files = append(files, p)
	}
	return files
}

// normPathKey 归一化路径比较键：斜杠统一、去 ./ 前缀、lower（Windows 大小写不敏感）。
// 反斜杠统一不依赖平台：两侧路径来自 spec 文本与写盘记录，可能混用 Windows 形态，
// 比对要跨平台一致（filepath.ToSlash 在非 Windows 上原样保留反斜杠，会让同一文件判为不同）。
func normPathKey(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	p = strings.TrimPrefix(p, "./")
	return strings.ToLower(p)
}

// pathsOverlap 判断两路径是否指向同一文件：归一化后相等，或互为目录后缀
//（容忍 spec 写相对路径、lastWrites 记绝对路径的差异）。
func pathsOverlap(a, b string) bool {
	ka, kb := normPathKey(a), normPathKey(b)
	if ka == "" || kb == "" {
		return false
	}
	if ka == kb {
		return true
	}
	return strings.HasSuffix(ka, "/"+kb) || strings.HasSuffix(kb, "/"+ka)
}

// overlapFileList 返回 specFiles 中与 written 任一路径重叠的子集（保序）。
func overlapFileList(specFiles, written []string) []string {
	var out []string
	for _, sf := range specFiles {
		for _, wf := range written {
			if pathsOverlap(sf, wf) {
				out = append(out, sf)
				break
			}
		}
	}
	return out
}
