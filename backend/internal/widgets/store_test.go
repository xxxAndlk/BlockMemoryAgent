package widgets

// store_test.go 钉死 widgets.Manager 的契约：扫描/热载（mtime 变化重读）、
// Save 校验（坏 id / 越界路径 / 超尺寸 / 缺 entry）、更新保留 enabled、
// SetEnabled 原子改写、Delete 幂等。这条链路是"Agent 自写界面能不能被看到"的源头。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManager 在临时目录建一个 Manager。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// writeManifest 手工落一个部件包（模拟手改场景）。
func writeManifest(t *testing.T, m *Manager, id, manifestJSON string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(m.Dir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManager_ScanAndGet(t *testing.T) {
	m := newTestManager(t)
	writeManifest(t, m, "wechat-qr",
		`{"name":"微信二维码","version":"1.0","description":"扫码画布","entry":"index.html","enabled":true}`,
		map[string]string{"index.html": "<html></html>", "app.js": "// js", "css/a.css": "/* css */"})

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("应扫到 1 个部件, got %d", len(list))
	}
	info, ok := m.Get("wechat-qr")
	if !ok {
		t.Fatal("Get 应命中")
	}
	if info.Name != "微信二维码" || info.Version != "1.0" || info.Description != "扫码画布" ||
		info.Entry != "index.html" || !info.Enabled || info.UpdatedAt.IsZero() {
		t.Fatalf("Info 字段不符: %+v", info)
	}
	// 文件列表：不含清单，含子目录文件，正斜杠。
	want := []string{"app.js", "css/a.css", "index.html"}
	if strings.Join(info.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("文件列表不符: %v", info.Files)
	}
	// 无清单的目录不参与管理；异形 id 不参与管理。
	if err := os.MkdirAll(filepath.Join(m.Dir(), "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.Dir(), "Bad_ID"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("无清单/异形目录不应入列, got %d", n)
	}
	if _, ok := m.Get("missing"); ok {
		t.Fatal("不存在部件 Get 应返回 false")
	}
}

func TestManager_HotReloadOnMtimeChange(t *testing.T) {
	m := newTestManager(t)
	writeManifest(t, m, "card", `{"name":"v1","entry":"index.html","enabled":true}`, map[string]string{"index.html": "a"})

	info, ok := m.Get("card")
	if !ok || info.Name != "v1" || !info.Enabled {
		t.Fatalf("首读不符: %+v ok=%v", info, ok)
	}
	// 手改清单（等 20ms 保证 mtime 变化——Windows 低精度时钟可能同刻）。
	time.Sleep(20 * time.Millisecond)
	writeManifest(t, m, "card", `{"name":"v2","entry":"index.html","enabled":false}`, map[string]string{"index.html": "a"})
	info, ok = m.Get("card")
	if !ok || info.Name != "v2" || info.Enabled {
		t.Fatalf("mtime 变化应热载新清单: %+v ok=%v", info, ok)
	}
	// 清单改坏：Get 未命中但 List 不拖垮。
	time.Sleep(20 * time.Millisecond)
	writeManifest(t, m, "card", `{bad json`, map[string]string{"index.html": "a"})
	if _, ok := m.Get("card"); ok {
		t.Fatal("损坏清单 Get 应返回 false")
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("损坏清单应跳过, got %d", n)
	}
}

func TestManager_SaveValidation(t *testing.T) {
	m := newTestManager(t)
	files := []FileInput{{Path: "index.html", Content: "<html></html>"}}

	if _, err := m.Save("Bad_ID", Manifest{}, files); err == nil {
		t.Fatal("非法 id 应拒绝")
	}
	if _, err := m.Save("ok-id", Manifest{}, nil); err == nil {
		t.Fatal("空文件列表应拒绝")
	}
	if _, err := m.Save("ok-id", Manifest{Entry: "main.html"}, files); err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("entry 不在文件列表应拒绝, got %v", err)
	}
	for _, bad := range []string{"../x.js", "/abs/x.js", "a/../../x.js", ".hidden.js"} {
		if _, err := m.Save("ok-id", Manifest{}, []FileInput{{Path: "index.html"}, {Path: bad}}); err == nil {
			t.Fatalf("越界/隐藏路径 %q 应拒绝", bad)
		}
	}
	// 文件数上限。
	tooMany := make([]FileInput, 0, maxWidgetFiles+1)
	tooMany = append(tooMany, FileInput{Path: "index.html"})
	for i := 0; i < maxWidgetFiles; i++ {
		tooMany = append(tooMany, FileInput{Path: fmt.Sprintf("f%d.js", i)})
	}
	if _, err := m.Save("ok-id", Manifest{}, tooMany); err == nil {
		t.Fatal("超文件数上限应拒绝")
	}
	// 总大小上限。
	big := FileInput{Path: "big.txt", Content: strings.Repeat("x", maxWidgetTotalBytes+1)}
	if _, err := m.Save("ok-id", Manifest{}, []FileInput{{Path: "index.html"}, big}); err == nil {
		t.Fatal("超总大小上限应拒绝")
	}
	// 清单名冲突。
	if _, err := m.Save("ok-id", Manifest{}, []FileInput{{Path: "index.html"}, {Path: manifestFile}}); err == nil {
		t.Fatal("与清单同名文件应拒绝")
	}
}

func TestManager_SavePreservesEnabledAndFilesOnDisk(t *testing.T) {
	m := newTestManager(t)
	info, err := m.Save("wechat-qr",
		Manifest{Name: "微信二维码", Description: "扫码画布"},
		[]FileInput{{Path: "index.html", Content: "<html>v1</html>"}, {Path: "app.js", Content: "// v1"}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if info.Enabled {
		t.Fatal("新部件默认应停用（enabled=false），由用户显式启用")
	}
	// 启用后再保存（改代码）：enabled 必须保留。
	if _, err := m.SetEnabled("wechat-qr", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	info, err = m.Save("wechat-qr",
		Manifest{Name: "微信二维码", Description: "扫码画布"},
		[]FileInput{{Path: "index.html", Content: "<html>v2</html>"}})
	if err != nil {
		t.Fatalf("第二次 Save: %v", err)
	}
	if !info.Enabled {
		t.Fatal("更新应保留原 enabled=true")
	}
	// 磁盘内容是新版本；旧文件 app.js 已被整目录替换清掉。
	data, err := os.ReadFile(filepath.Join(m.Dir(), "wechat-qr", "index.html"))
	if err != nil || string(data) != "<html>v2</html>" {
		t.Fatalf("磁盘内容不符: %q err=%v", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), "wechat-qr", "app.js")); !os.IsNotExist(err) {
		t.Fatal("整目录替换后旧文件应不存在")
	}
	// 临时/备份目录无残留（.trash 回收站是设计内常驻目录，不算残留；其下应有本次覆写快照）。
	entries, _ := os.ReadDir(m.Dir())
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") && de.Name() != ".trash" {
			t.Fatalf("原子写残留临时目录: %s", de.Name())
		}
	}
	snapEntries, _ := os.ReadDir(filepath.Join(m.Dir(), ".trash"))
	if len(snapEntries) != 1 || !strings.HasPrefix(snapEntries[0].Name(), "wechat-qr-snap-") {
		t.Fatalf("覆写前旧目录应快照进 .trash/<id>-snap-<ts>: %v", snapEntries)
	}
}

func TestManager_SetEnabledAtomic(t *testing.T) {
	m := newTestManager(t)
	writeManifest(t, m, "card", `{"name":"卡片","entry":"index.html","enabled":true,"description":"d"}`, map[string]string{"index.html": "a"})

	info, err := m.SetEnabled("card", false)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if info.Enabled {
		t.Fatal("应已停用")
	}
	// 其余字段原样保留。
	if info.Name != "卡片" || info.Description != "d" || info.Entry != "index.html" {
		t.Fatalf("SetEnabled 不应改其他字段: %+v", info)
	}
	// 读回（跨缓存）确认落盘。
	m2, err := NewManager(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m2.Get("card")
	if !ok || got.Enabled {
		t.Fatalf("新实例读回应为停用: %+v ok=%v", got, ok)
	}
	if _, err := m.SetEnabled("missing", true); err == nil {
		t.Fatal("不存在部件 SetEnabled 应报错")
	}
}

func TestManager_Delete(t *testing.T) {
	m := newTestManager(t)
	writeManifest(t, m, "card", `{"entry":"index.html","enabled":true}`, map[string]string{"index.html": "a"})

	if _, err := m.Delete("card"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.Get("card"); ok {
		t.Fatal("删除后 Get 应未命中")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), "card")); !os.IsNotExist(err) {
		t.Fatal("删除后目录应不存在")
	}
	// 幂等：再删一次不报错；非法 id 报错。
	if _, err := m.Delete("card"); err != nil {
		t.Fatal("重复删除应幂等")
	}
	if _, err := m.Delete("../x"); err == nil {
		t.Fatal("非法 id 应拒绝")
	}
}

// TestManager_SoftDeleteTrashAndRestore 钉死软删回收站契约：Delete 把部件目录移入
// .trash/<id>-del-<ts>/（不物理删）、缓存失效、TrashList 可见（del 类型）、Restore 还原
// 到 widgets/<id>/ 且条目消失；恢复时现存同名部件先入回收站不覆盖丢失。
func TestManager_SoftDeleteTrashAndRestore(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Save("card", Manifest{Name: "卡片", Entry: "index.html"},
		[]FileInput{{Path: "index.html", Content: "v1"}}); err != nil {
		t.Fatal(err)
	}
	entry, err := m.Delete("card")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if entry == "" || !strings.HasPrefix(entry, "card-del-") {
		t.Fatalf("应返回 card-del-* 条目名: %q", entry)
	}
	if _, ok := m.Get("card"); ok {
		t.Fatal("软删后缓存应失效")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), ".trash", entry)); err != nil {
		t.Fatalf("回收站目录应存在: %v", err)
	}
	// 恢复路径不存在：报错。
	if _, err := m.Restore("card-del-9999999999"); err == nil {
		t.Fatal("恢复不存在的条目应报错")
	}
	// 条目名非法：报错。
	if _, err := m.Restore("../evil"); err == nil {
		t.Fatal("非法条目名应拒绝")
	}

	// 重建同名部件后恢复：现存版本先入回收站。
	if _, err := m.Save("card", Manifest{Name: "卡片", Entry: "index.html"},
		[]FileInput{{Path: "index.html", Content: "v2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(entry); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	info, ok := m.Get("card")
	if !ok || info.Entry != "index.html" {
		t.Fatalf("恢复后应可见: %+v ok=%v", info, ok)
	}
	data, _ := os.ReadFile(filepath.Join(m.Dir(), "card", "index.html"))
	if string(data) != "v1" {
		t.Fatalf("恢复应是旧版本内容: %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), ".trash", entry)); !os.IsNotExist(err) {
		t.Fatal("恢复后条目应消失")
	}
	// 现存 v2 已被挪进回收站。
	if len(m.TrashList()) != 1 {
		t.Fatalf("现存版本应入回收站: %+v", m.TrashList())
	}
}

// TestManager_TrashListAndSnapshot 钉死回收站列表与覆写快照：TrashList 只认
// <id>-del/snap-<ts> 形态目录并按时间倒序；Save 覆写把旧目录快照进
// .trash/<id>-snap-<ts>/（可经 Restore 回滚到旧版本），异形目录不进列表。
func TestManager_TrashListAndSnapshot(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Save("card", Manifest{Name: "c", Entry: "index.html"},
		[]FileInput{{Path: "index.html", Content: "v1"}}); err != nil {
		t.Fatal(err)
	}
	// 异形目录（人工放的杂物）不进列表。
	_ = os.MkdirAll(filepath.Join(m.Dir(), trashDirName, "junk-dir"), 0o755)
	// 覆写 → 快照。
	if _, err := m.Save("card", Manifest{Name: "c", Entry: "index.html"},
		[]FileInput{{Path: "index.html", Content: "v2"}}); err != nil {
		t.Fatal(err)
	}
	list := m.TrashList()
	if len(list) != 1 || list[0].WidgetID != "card" || list[0].Kind != "snap" ||
		!strings.HasPrefix(list[0].Name, "card-snap-") || len(list[0].Files) != 1 {
		t.Fatalf("TrashList 应含 card-snap-* 快照: %+v", list)
	}
	if list[0].DeletedAt.IsZero() {
		t.Fatal("deleted_at 应有时间戳")
	}
	// 回滚到快照版本。
	if _, err := m.Restore(list[0].Name); err != nil {
		t.Fatalf("Restore snap: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(m.Dir(), "card", "index.html"))
	if string(data) != "v1" {
		t.Fatalf("快照恢复应回到 v1: %q", string(data))
	}
}

// TestManager_SaveIncremental 钉死增量保存契约：upsert 只改列出文件（其余保持）、
// delete 删列出的文件（不存在报错）、保留 enabled/owner、与 files 语义同款校验
//（超清单/超大/entry 缺失）、新部件不允许增量。
func TestManager_SaveIncremental(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Save("card", Manifest{Name: "c", Entry: "index.html", Owner: "u1"},
		[]FileInput{{Path: "index.html", Content: "v1"}, {Path: "app.js", Content: "js1"}, {Path: "lib/util.js", Content: "u"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetEnabled("card", true); err != nil {
		t.Fatal(err)
	}

	info, err := m.SaveIncremental("card", Manifest{Name: "c2"},
		[]FileInput{{Path: "app.js", Content: "js2"}, {Path: "extra.css", Content: "x"}}, []string{"lib/util.js"})
	if err != nil {
		t.Fatalf("SaveIncremental: %v", err)
	}
	// enabled/owner 保留；name/entry 沿用原清单；文件三存一改一增一删。
	if !info.Enabled || info.Owner != "u1" || info.Name != "c2" || info.Entry != "index.html" {
		t.Fatalf("清单保留语义不符: %+v", info)
	}
	got := map[string]bool{}
	for _, f := range info.Files {
		got[f] = true
	}
	if !got["index.html"] || !got["app.js"] || !got["extra.css"] || got["lib/util.js"] {
		t.Fatalf("文件集合不符: %v", info.Files)
	}
	if data, _ := os.ReadFile(filepath.Join(m.Dir(), "card", "index.html")); string(data) != "v1" {
		t.Fatalf("未列出的文件应保持原样: %q", string(data))
	}
	// 覆盖前快照进回收站。
	if len(m.TrashList()) != 1 {
		t.Fatalf("增量覆写应先快照: %+v", m.TrashList())
	}

	// 不存在的删除路径报错；未知部件报错；空参数报错。
	if _, err := m.SaveIncremental("card", Manifest{}, nil, []string{"nope.js"}); err == nil {
		t.Fatal("删除不存在文件应报错")
	}
	if _, err := m.SaveIncremental("ghost", Manifest{}, []FileInput{{Path: "a", Content: "b"}}, nil); err == nil {
		t.Fatal("未知部件增量应报错")
	}
	if _, err := m.SaveIncremental("card", Manifest{}, nil, nil); err == nil {
		t.Fatal("空 upsert/delete 应报错")
	}
}

// TestManager_OwnerPropagation 钉死 owner 归属：Save 带.owner 落盘并在 Info/JSON 透出；
// 更新未带 owner 时保留原值；未接线留空。
func TestManager_OwnerPropagation(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Save("card", Manifest{Name: "c", Entry: "index.html", Owner: "user-9"},
		[]FileInput{{Path: "index.html", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	info, _ := m.Get("card")
	if info.Owner != "user-9" {
		t.Fatalf("Info 应透出 owner: %+v", info)
	}
	if _, err := m.Save("card", Manifest{Name: "c", Entry: "index.html"},
		[]FileInput{{Path: "index.html", Content: "y"}}); err != nil {
		t.Fatal(err)
	}
	if info, _ = m.Get("card"); info.Owner != "user-9" {
		t.Fatalf("更新应保留原 owner: %+v", info)
	}
}
