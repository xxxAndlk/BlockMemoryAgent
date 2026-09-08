package tool

import (
	"os"
	"testing"
)

func fakeExists(paths ...string) func(string) bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

// 类 Unix 恒为 sh -c。
func TestResolveShell_Unix(t *testing.T) {
	sc := unixShell()
	if sc.Exe != "sh" || len(sc.Args) != 1 || sc.Args[0] != "-c" {
		t.Fatalf("unix shell 错误: %+v", sc)
	}
}

// PATH 无 bash 但 git.exe 可见时，从 git.exe 同级推断出 Git Bash。
func TestResolveShell_Windows_BashViaGitSibling(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "git" {
			return `C:\Program Files\Git\cmd\git.exe`, nil
		}
		return "", os.ErrNotExist
	}
	exists := fakeExists(`C:\Program Files\Git\bin\bash.exe`)
	sc := windowsShell(look, exists)
	if sc.Exe != `C:\Program Files\Git\bin\bash.exe` {
		t.Fatalf("未从 git 同级推断 bash: %+v", sc)
	}
	if len(sc.Args) != 1 || sc.Args[0] != "-c" || sc.ForceUTF8 {
		t.Fatalf("bash 调用形状错误: %+v", sc)
	}
	if len(sc.ExtraEnv) == 0 || sc.ExtraEnv[0] != "MSYS_NO_PATHCONV=1" {
		t.Fatalf("bash 缺 MSYS_NO_PATHCONV: %+v", sc.ExtraEnv)
	}
}

// PATH 中的 WSL stub（System32\bash.exe）必须排除，继续探测后回落 PowerShell。
func TestResolveShell_Windows_WSLStubExcluded(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "bash" {
			return `C:\Windows\System32\bash.exe`, nil
		}
		return "", os.ErrNotExist
	}
	sc := windowsShell(look, fakeExists())
	if sc.Exe != "powershell" || !sc.ForceUTF8 {
		t.Fatalf("WSL stub 未排除: %+v", sc)
	}
}

// bash/git 均不可得时回落 PowerShell（带 -NoLogo -NoProfile -Command 与 UTF-8 前缀标记）。
func TestResolveShell_Windows_PowerShellFallback(t *testing.T) {
	sc := windowsShell(func(string) (string, error) { return "", os.ErrNotExist }, fakeExists())
	if sc.Exe != "powershell" || !sc.ForceUTF8 {
		t.Fatalf("PowerShell 兜底错误: %+v", sc)
	}
	if len(sc.Args) != 3 || sc.Args[2] != "-Command" {
		t.Fatalf("PowerShell 参数错误: %+v", sc.Args)
	}
}

// PATH 直接命中非 WSL bash 时优先使用。
func TestResolveShell_Windows_BashOnPath(t *testing.T) {
	look := func(name string) (string, error) {
		if name == "bash" {
			return `D:\Git\bin\bash.exe`, nil
		}
		return "", os.ErrNotExist
	}
	sc := windowsShell(look, fakeExists())
	if sc.Exe != `D:\Git\bin\bash.exe` {
		t.Fatalf("PATH bash 未命中: %+v", sc)
	}
}
