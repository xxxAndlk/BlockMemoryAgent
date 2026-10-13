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
	// 临时/备份目录无残留。
	entries, _ := os.ReadDir(m.Dir())
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") {
			t.Fatalf("原子写残留临时目录: %s", de.Name())
		}
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

	if err := m.Delete("card"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := m.Get("card"); ok {
		t.Fatal("删除后 Get 应未命中")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), "card")); !os.IsNotExist(err) {
		t.Fatal("删除后目录应不存在")
	}
	// 幂等：再删一次不报错；非法 id 报错。
	if err := m.Delete("card"); err != nil {
		t.Fatal("重复删除应幂等")
	}
	if err := m.Delete("../x"); err == nil {
		t.Fatal("非法 id 应拒绝")
	}
}
