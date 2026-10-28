package agent

// memory_import.go —— Markdown 长期记忆迁移（doc/计划_长期记忆撤回与污染修复.md §8）。
//
// 目标：把 config/user_profile.md 与 .bma/project_preferences.md 里的自然语言行
// **一次性导入**为有条目的 items（之后注入只读 items 的有效视图），并且：
//   - 归档小节（反馈记录/经验归档）只导入为 tombstone（retracted），不进入 active；
//   - 无时间戳的行 = 人工维护（origin=human，可 active）；
//   - 带时间戳的行 = 自动沉淀（origin=legacy_import；项目经验不因"原来在现行区"
//     就自动获得 verified，按计划保持 unverified）；
//   - 迁移只在目标作用域为空时执行一次（幂等，不重复导入、不覆盖已有条目）。

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/store"
)

// memoryLineTimestampRe 匹配自动沉淀行的行尾时间戳/来源标记：
// "（2026-09-07 17:09）" 与 "（2026-10-22 23:42 · 会话#65）"。
var memoryLineTimestampRe = regexp.MustCompile(`\s*（\s*20\d{2}-\d{2}-\d{2}[^）]*）\s*$`)

// memoryArchiveSections 归档小节名（内容只作 tombstone）。
var memoryArchiveSections = []string{"反馈记录", "经验归档", "已归档"}

// ImportMarkdown 把一份 Markdown 记忆文件导入 items（幂等：作用域已有条目时跳过）。
// scope=user 时 projectKey 必须为空；scope=project 时必须给 projectKey。
// 返回导入条目数（跳过时为 0）与被跳过原因。
func (lc *MemoryLifecycle) ImportMarkdown(ctx context.Context, owner, scope, projectKey, markdown string) (int, string) {
	if lc == nil || lc.repo == nil {
		return 0, "lifecycle not wired"
	}
	if scope != store.MemoryScopeUser && scope != store.MemoryScopeProject {
		return 0, "invalid scope"
	}
	pk := ""
	if scope == store.MemoryScopeProject {
		pk = store.NormalizeProjectKey(projectKey)
		if pk == "" {
			return 0, "project_key required for project scope"
		}
	}
	existing, err := lc.repo.ListItems(ctx, store.MemoryQuery{
		Owner: owner, Scope: scope, ProjectKey: pk, Limit: 1,
	})
	if err != nil {
		return 0, "list existing items: " + err.Error()
	}
	if len(existing) > 0 {
		return 0, "already imported"
	}
	parsed := parseMemoryMarkdown(markdown)
	if len(parsed) == 0 {
		return 0, "no importable lines"
	}
	now := time.Now()
	imported := 0
	for _, p := range parsed {
		status := store.MemoryStatusActive
		origin := store.MemoryOriginHuman
		verification := store.MemoryVerificationUnverified
		if p.archived {
			// 归档区只作 tombstone：防止后续自动沉淀换措辞把旧结论写回现行区。
			status = store.MemoryStatusRetracted
			origin = store.MemoryOriginLegacyImport
		} else if p.auto {
			origin = store.MemoryOriginLegacyImport
		}
		if !p.archived && !p.auto {
			// 人工维护的约定视为已验证（计划 §8.2 第 6 条）。
			verification = store.MemoryVerificationVerified
		}
		key := p.memoryKey
		if key == "" {
			keys := classifyMemoryKeys(p.content)
			if len(keys) > 0 {
				key = keys[0]
			} else {
				key = "lesson.general"
			}
		}
		// 同名 key 的后续行改挂到 lesson.* 派生键，避免 active 唯一索引冲突。
		if status == store.MemoryStatusActive {
			list, _ := lc.repo.ListItems(ctx, store.MemoryQuery{
				Owner: owner, Scope: scope, ProjectKey: pk,
				Statuses: []string{store.MemoryStatusActive}, MemoryKeys: []string{key}, Limit: 1,
			})
			if len(list) > 0 {
				key = derivedMemoryKey(key, p.content)
			}
		}
		item := store.MemoryItem{
			Owner: owner, Scope: scope, ProjectKey: pk, MemoryKey: key,
			Content: p.content, Status: status, Origin: origin, Verification: verification,
			ContentHash: store.MemoryContentHash(p.content),
			CreatedAt:   now, UpdatedAt: now,
		}
		created, err := lc.repo.InsertItem(ctx, item)
		if err != nil {
			log.Printf("[memory-lifecycle] import item failed (skip): key=%s err=%v", key, err)
			continue
		}
		imported++
		eventType := store.MemoryEventImported
		if p.archived {
			eventType = store.MemoryEventRetracted
		}
		_ = lc.repo.AppendEvent(ctx, store.MemoryEvent{
			Owner: owner, MemoryID: created.ID, EventType: eventType, Actor: store.MemoryActorSystem,
			Reason:  fmt.Sprintf("从 Markdown 迁移导入（section=%s auto=%v archived=%v）", p.section, p.auto, p.archived),
			Payload: fmt.Sprintf(`{"section":%q,"auto":%v,"archived":%v}`, p.section, p.auto, p.archived),
		})
	}
	lc.bumpRevision()
	log.Printf("[memory-lifecycle] imported %d memory item(s) scope=%s project=%s", imported, scope, pk)
	return imported, ""
}

// parsedMemoryLine 一行记忆的解析结果。
type parsedMemoryLine struct {
	section   string
	content   string
	auto      bool // 行尾带时间戳 = 自动沉淀
	archived  bool // 位于归档小节
	memoryKey string
}

// parseMemoryMarkdown 解析 Markdown 记忆文件：
//   - "## 小节" 切换当前小节；
//   - "- 内容" 为条目行；同一条自动行里用 " | " 合并的多条偏好按分隔拆开；
//   - 行尾时间戳/来源标记剥离并记为自动行；
//   - 归档小节全部标记 archived；"项目约定"小节视为人工约定。
func parseMemoryMarkdown(md string) []parsedMemoryLine {
	var out []parsedMemoryLine
	section := ""
	for _, raw := range strings.Split(md, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(strings.TrimLeft(line, "#"))
			continue
		}
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") {
			continue
		}
		body := strings.TrimSpace(line[2:])
		if body == "" || strings.HasPrefix(body, ">") {
			continue
		}
		archived := false
		for _, s := range memoryArchiveSections {
			if strings.Contains(section, s) {
				archived = true
				break
			}
		}
		auto := false
		if loc := memoryLineTimestampRe.FindStringIndex(body); loc != nil {
			auto = true
			body = strings.TrimSpace(body[:loc[0]])
		}
		// 自动行里的 " | " 是历次 Merge 合并多条的痕迹：拆成独立条目，避免一条
		// 巨行既难撤回又难注入。
		parts := []string{body}
		if auto && strings.Contains(body, " | ") {
			parts = strings.Split(body, " | ")
		}
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" || strings.HasPrefix(p, ">") {
				continue
			}
			out = append(out, parsedMemoryLine{
				section: section, content: p, auto: auto, archived: archived,
			})
		}
	}
	return out
}

// derivedMemoryKey 让同 key 的第二条起落到 lesson.<主题> 派生键（保持"每条一个稳定键"）。
func derivedMemoryKey(key, content string) string {
	base := strings.TrimPrefix(key, "lesson.")
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	sum := store.MemoryContentHash(content)
	short := sum
	if len(short) > 6 {
		short = short[:6]
	}
	return "lesson." + base + "." + short
}

// SyncHumanEdit 把人工全量编辑过的 Markdown 记忆同步进结构化条目（PUT /api/profile、
// 项目偏好 PUT 的路径）：新增行落 active（origin=human）；原 active 条目在新内容里
// 已不存在的标 retracted（reason=human_edit）。不物理删除，事件留痕。
// 归档小节的行仍按 tombstone 处理（不激活）。
func (lc *MemoryLifecycle) SyncHumanEdit(ctx context.Context, owner, scope, projectKey, markdown string) (added, removed int, err error) {
	if lc == nil || lc.repo == nil {
		return 0, 0, nil
	}
	pk := ""
	if scope == store.MemoryScopeProject {
		pk = store.NormalizeProjectKey(projectKey)
	}
	parsed := parseMemoryMarkdown(markdown)
	want := map[string]bool{}
	for _, p := range parsed {
		if p.archived {
			continue
		}
		want[store.MemoryContentHash(p.content)] = true
	}
	existing, lerr := lc.repo.ListItems(ctx, store.MemoryQuery{
		Owner: owner, Scope: scope, ProjectKey: pk, Statuses: []string{store.MemoryStatusActive},
	})
	if lerr != nil {
		return 0, 0, lerr
	}
	sc := MemoryScopeContext{Owner: owner, ProjectKey: pk}
	for _, it := range existing {
		// 只处理人工/显式来源的行：自动沉淀（evolver）由 evolver 自己的路径管理。
		if it.Origin != store.MemoryOriginHuman && it.Origin != store.MemoryOriginExplicitTool {
			continue
		}
		if want[it.ContentHash] {
			continue
		}
		if terr := lc.transition(ctx, sc, it, store.MemoryStatusRetracted, store.MemoryActorAdmin,
			"人工编辑已移除该行（human_edit）", ""); terr == nil {
			removed++
		}
	}
	for _, p := range parsed {
		if p.archived || want["used:"+p.content] {
			continue
		}
		hash := store.MemoryContentHash(p.content)
		// 已 active 的同内容行跳过（避免重复）。
		dup := false
		for _, it := range existing {
			if it.ContentHash == hash && it.Status == store.MemoryStatusActive {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		key := p.memoryKey
		if key == "" {
			if keys := classifyMemoryKeys(p.content); len(keys) > 0 {
				key = keys[0]
			} else {
				key = "lesson.general"
			}
		}
		// active 唯一键：同 key 已被占用则派生。
		list, _ := lc.repo.ListItems(ctx, store.MemoryQuery{
			Owner: owner, Scope: scope, ProjectKey: pk,
			Statuses: []string{store.MemoryStatusActive}, MemoryKeys: []string{key}, Limit: 1,
		})
		if len(list) > 0 {
			key = derivedMemoryKey(key, p.content)
		}
		created, blocked, reason := lc.insertChecked(ctx, store.MemoryItem{
			Owner: owner, Scope: scope, ProjectKey: pk, MemoryKey: key,
			Content: p.content, Status: store.MemoryStatusActive, Origin: store.MemoryOriginHuman,
			Verification: store.MemoryVerificationVerified, ContentHash: hash,
		})
		if blocked {
			log.Printf("[memory-lifecycle] human edit line blocked by tombstone (kept candidate): %s", reason)
			continue
		}
		if created.ID != "" {
			added++
		}
	}
	lc.bumpRevision()
	return added, removed, nil
}
