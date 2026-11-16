//go:build windows

// 自动更新：商业桌面应用的常规做法——启动时（或用户点「检查更新」）拉一份远端
// manifest，发现新版本就下载安装包并静默运行，由安装器替换当前版本。
//
// 更新地址写在安装目录 .env 的 BMA_UPDATE_URL（可指向一个 JSON 文件）：
//
//	{ "version": "20261114-171028-eb60f8e",
//	  "url": "https://example.com/BlockMemoryAgent-Setup-20261114-171028.exe",
//	  "sha256": "…", "notes": "修复了什么" }
//
// 没配 BMA_UPDATE_URL 就不做任何检查（不打扰用户）；版本号用时间戳前缀，同格式下
// 字符串比较即等价于时间比较。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blockmemory/agent/backend/internal/buildinfo"
)

// updateManifest 远端更新清单。
type updateManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Notes   string `json:"notes"`
	SHA256  string `json:"sha256"`
}

// updateURL 更新地址（空=不启用自动更新）。
func updateURL(root string) string {
	return strings.TrimSpace(readEnvValue(filepath.Join(root, ".env"), "BMA_UPDATE_URL"))
}

// checkUpdate 拉取 manifest 并判断是否有新版本。
//
// 返回 (nil, false, nil) 表示没配置更新地址或已是最新——调用方据此静默跳过。
func checkUpdate(root string) (*updateManifest, bool, error) {
	url := updateURL(root)
	if url == "" {
		return nil, false, nil
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("更新清单返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	var m updateManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("更新清单解析失败: %w", err)
	}
	if m.Version == "" || m.URL == "" {
		return nil, false, fmt.Errorf("更新清单缺少 version/url")
	}
	cur := appVersion(root)
	if m.Version <= cur {
		return &m, false, nil
	}
	return &m, true, nil
}

// appVersion 当前安装的版本（读版本指针；读不到就用编译期 buildinfo）。
func appVersion(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "app", "current.json"))
	if err == nil {
		var cur struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &cur); err == nil && cur.Version != "" {
			return cur.Version
		}
	}
	return buildinfo.Version
}

// runUpdateCheck 检查更新并（用户同意时）下载安装。
//
// interactive=false 用于启动时的后台检查：没配更新地址就静默跳过，不打扰用户；
// interactive=true 用于托盘菜单手动触发，会把结果（已是最新/失败原因）说清楚。
func runUpdateCheck(root string, interactive bool) {
	m, hasNew, err := checkUpdate(root)
	if err != nil {
		if interactive {
			showError(appTitle, "检查更新失败："+err.Error())
		}
		return
	}
	if m == nil {
		if interactive {
			showError(appTitle, "未配置更新地址。\n\n在安装目录的 .env 里加一行即可启用：\nBMA_UPDATE_URL=https://…/latest.json\n\n"+
				"该 JSON 需包含 version 与 url 两个字段。")
		}
		return
	}
	if !hasNew {
		if interactive {
			showError(appTitle, "已是最新版本："+m.Version)
		}
		return
	}
	msg := "发现新版本 " + m.Version + "\n当前版本 " + appVersion(root)
	if m.Notes != "" {
		msg += "\n\n" + m.Notes
	}
	msg += "\n\n是否立即下载并安装？"
	if !showConfirm("发现新版本", msg) {
		return
	}
	if err := downloadAndRun(m); err != nil {
		showError(appTitle, "更新失败："+err.Error())
		return
	}
	// 安装器接管：先停后端与内置运行时，再退出本进程。
	stopBackend(root)
	stopEmbeddedRuntime(root)
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}

// downloadAndRun 下载安装包并静默运行（由安装器替换当前版本）。
//
// 流程：下载 → 校验 sha256（清单给了才校验）→ 静默启动安装器 → 退出本进程，
// 让安装器接管（安装器会用 Restart Manager 关掉还在跑的后端）。
func downloadAndRun(m *updateManifest) error {
	if m == nil || m.URL == "" {
		return fmt.Errorf("没有可安装的更新")
	}
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("BlockMemoryAgent-Setup-%s.exe", m.Version))
	if err := downloadFile(m.URL, dst); err != nil {
		return err
	}
	if m.SHA256 != "" {
		sum, err := fileSHA256(dst)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sum, m.SHA256) {
			os.Remove(dst)
			return fmt.Errorf("安装包校验失败（sha256 不匹配）")
		}
	}
	cmd := exec.Command(dst, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动安装包失败: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// downloadFile 下载文件到本地路径。
func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// fileSHA256 计算文件 sha256（十六进制小写）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
