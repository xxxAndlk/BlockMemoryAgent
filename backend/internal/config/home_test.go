package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 造一个"像 home"的目录(含 config/config.yaml)
func mkHome(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "config.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveHome_EnvWins(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "envhome")
	got, err := resolveHome(home, filepath.Join(base, "nowhere", "bin", "tui.exe"), base)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_EnvInvalid(t *testing.T) {
	base := t.TempDir() // 不含 config/config.yaml
	if _, err := resolveHome(base, "", ""); err == nil {
		t.Fatal("want error for BMA_HOME without config/config.yaml")
	}
}

func TestResolveHome_ExeBinParent(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "bma") // exe 在 <home>/bin/tui.exe
	got, err := resolveHome("", filepath.Join(home, "bin", "tui.exe"), base)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_ExeBackendDevLayout(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "repo") // 开发态:exe 在 <repo>/backend/tui.exe
	got, err := resolveHome("", filepath.Join(home, "backend", "tui.exe"), filepath.Join(base, "other"))
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_CwdFallback(t *testing.T) {
	base := t.TempDir()
	home := mkHome(t, base, "cwdhome")
	got, err := resolveHome("", filepath.Join(base, "random", "x.exe"), home)
	if err != nil || got != home {
		t.Fatalf("got %q err %v, want %q", got, err, home)
	}
}

func TestResolveHome_NotFound(t *testing.T) {
	base := t.TempDir()
	if _, err := resolveHome("", filepath.Join(base, "a", "x.exe"), base); err == nil {
		t.Fatal("want error when nothing looks like home")
	}
}
