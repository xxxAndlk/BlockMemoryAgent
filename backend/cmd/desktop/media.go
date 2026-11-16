//go:build windows

// 原生媒体服务：包内自带 mediaservice.exe（媒体生成的上游适配），配合随包的
// Node + ui_design 桥（stdio 方式），让桌面版**免 Docker**也能生图/生视频。
//
// 与容器方案的分工：
//   * 容器方案（docker/open-design-daemon）：完整上游，能力全，但要求装 Docker。
//   * 原生方案（本文件 + cmd/mediaservice）：只实现桥实际用到的那几个端点，
//     纯文本生成（t2i/t2v）；参考图类操作明确报错而不是假装支持。
// 桌面版走原生方案；开发机仍可用容器方案（plugins.yaml 两种配置并存）。
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// mediaServiceName 原生媒体服务可执行文件名（版本目录内）。
const mediaServiceName = "mediaservice.exe"

// mediaServicePort 原生媒体服务端口（与容器 daemon 同端口，桥的默认地址就是它）。
const mediaServicePort = 7456

// ensureLocalMediaBridge 拉起随包的 MiniMax 适配桥（本地 Node 进程）。
//
// 仓库的 .env 媒体段指向容器服务名（minimax-media:8787）；桌面版没有 Docker 网络，
// 由这个本地进程顶替（零依赖单文件 Node 服务，随包带）。
func ensureLocalMediaBridge(root string, onStep func(string)) error {
	bridge := filepath.Join(root, runtimeDirName, "media", "minimax-media", "server.js")
	nodeExe := filepath.Join(root, runtimeDirName, "node", "node.exe")
	if !fileExists(bridge) || !fileExists(nodeExe) {
		return fmt.Errorf("安装包未包含本地媒体适配桥")
	}
	// 已经有人在听 8787（之前起过）就不重复起。
	if portOpen(8787) {
		return nil
	}
	if onStep != nil {
		onStep("启动本地媒体适配桥 ...")
	}
	logDir := filepath.Join(root, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "media_bridge.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(nodeExe, bridge)
	cmd.Dir = filepath.Dir(bridge)
	cmd.Env = os.Environ() // IMAGE_KEY / VIDEO_KEY / MINIMAX_* 已在 .env 里
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	_ = cmd.Process.Release()
	logFile.Close()
	// 等端口起来
	for i := 0; i < 40; i++ {
		if portOpen(8787) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("本地媒体适配桥未在超时内就绪（日志 %s）", logPath)
}

// portOpen 端口是否有人在听。
func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// stopLocalMediaBridge 停掉本地媒体适配桥（按安装目录下的 node.exe 路径匹配）。
func stopLocalMediaBridge(root string) {
	stopProcessByDir(filepath.Join(root, runtimeDirName, "node"), "node.exe")
}

// ensureMediaService 拉起原生媒体服务。
//
// 只在"装了原生媒体栈 + .env 配了媒体通道"时才启动；失败不阻断启动流程
// （媒体能力降级，主流程照常）。返回错误仅用于日志与状态展示。
func ensureMediaService(root string, onStep func(string)) error {
	exe := filepath.Join(root, "app", "versions", currentVersion(root), "bin", mediaServiceName)
	if !fileExists(exe) {
		return fmt.Errorf("安装包未包含原生媒体服务（%s）", mediaServiceName)
	}
	if !mediaConfigured() {
		return fmt.Errorf("未配置媒体通道（.env 的 IMAGE_PROVIDER / VIDEO_PROVIDER 等）")
	}
	// 先起本地媒体适配桥（.env 里指向容器服务名的通道靠它）。
	if err := ensureLocalMediaBridge(root, onStep); err != nil {
		logMediaNotice(root, err)
	}
	if onStep != nil {
		onStep("启动原生媒体服务 ...")
	}

	dataRoot, err := userDataDir()
	if err != nil {
		return err
	}
	dataDir := filepath.Join(dataRoot, "media-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logDir := filepath.Join(root, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	// 上一版遗留的实例还占着端口 → 先清掉（否则新实例 bind 失败退出）。
	if portOpen(mediaServicePort) {
		stopMediaService(root)
		for i := 0; i < 20 && portOpen(mediaServicePort); i++ {
			time.Sleep(250 * time.Millisecond)
		}
	}
	logPath := filepath.Join(logDir, "mediaservice.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	cmd := exec.Command(exe, "-addr", fmt.Sprintf("127.0.0.1:%d", mediaServicePort), "-data", dataDir)
	cmd.Dir = root
	cmd.Env = os.Environ() // .env 已由依赖自举阶段载入进程环境，媒体通道配置随之继承
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("启动媒体服务失败: %w", err)
	}
	_ = cmd.Process.Release()
	logFile.Close()

	// 等它就绪（桥第一次调用就会连；这里先确认端口起来了，避免用户等到调用才报错）
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if mediaServiceHealthy() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("媒体服务未在超时内就绪（日志 %s）", logPath)
}

// mediaConfigured 判断 .env 里是否声明了媒体通道。
func mediaConfigured() bool {
	for _, k := range []string{"IMAGE_PROVIDER", "IMAGE_URL", "IMAGE_MODEL", "VIDEO_PROVIDER", "VIDEO_URL", "VIDEO_MODEL"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return true
		}
	}
	return false
}

// mediaServiceHealthy 探测媒体服务健康。
func mediaServiceHealthy() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/media/config", mediaServicePort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// stopMediaService 停掉原生媒体服务（按安装目录路径精确匹配，不误杀别的实例）。
func stopMediaService(root string) {
	stopProcessByDir(filepath.Join(root, "app", "versions"), mediaServiceName)
}

// logMediaNotice 把媒体栈的降级原因写进日志（媒体不可用时主流程照常，只是这项能力没有）。
func logMediaNotice(root string, err error) {
	logDir := filepath.Join(root, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	f, err2 := os.OpenFile(filepath.Join(logDir, "media_notice.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err2 != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s 媒体栈降级：%v\n", time.Now().Format(time.RFC3339), err)
}
