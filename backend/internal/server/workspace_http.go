package server

// workspace_http.go 会话工作区文件服务（对话栏媒体渲染的读侧）：
// 把工作目录内的文件按真实 Content-Type 流式返回，供 <img>/<video>/<audio>/<iframe>
// 直接引用——这是"Agent 产出的图/视频/HTML 能在对话栏里看"的最后一环。
//
// 为什么必须是 path 型路由（/workspace/*path）而不是 ?path= 查询参数：
// HTML 产物里的**相对引用**（如 pages/index.html 里写 assets/x.png）会以本路由为基准
// 解析，天然可用；用查询参数则相对路径全部 404。

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// sessionWorkDirProvider 是"按会话解析有效工作目录"能力的鸭子类型接口
// （同 HandleSessionTopic 的 topicSwitcher 模式：只依赖一个方法，不动 agent.Agent 门面
// 与各处测试 mock）。由 *agent.ReactService 实现。
type sessionWorkDirProvider interface {
	SessionWorkDir(sessionID string) string
}

// HandleSessionWorkspace 处理 GET /api/sessions/{id}/workspace/*path。
//
// 安全边界（比既有 /api/files/content 严格——后者无任何路径约束，属遗留项）：
//   - 只服务**该会话工作目录内**的普通文件，越界（绝对路径、..、工作目录外）一律 400；
//   - 响应带 X-Content-Type-Options: nosniff 与 Referrer-Policy: no-referrer；
//   - 用 http.ServeContent 流式返回：自带 Range/Last-Modified/ETag 语义，
//     视频拖动进度条与音频分段加载依赖它，且绝不把二进制读进内存或事件流。
func (m *SessionManager) HandleSessionWorkspace(c *gin.Context) {
	id := c.Param("id")
	wp, ok := m.agent.(sessionWorkDirProvider)
	if !ok {
		c.String(http.StatusNotImplemented, "工作区文件服务未接线")
		return
	}
	root := wp.SessionWorkDir(id)
	if root == "" {
		// 会话不存在（或既无会话目录也无进程默认目录）。
		c.String(http.StatusNotFound, "会话或工作目录不存在")
		return
	}

	// gin 的通配参数含前导 "/"；统一转成路径分隔符后清洗。
	rel := strings.TrimPrefix(c.Param("path"), "/")
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || rel == "." {
		c.String(http.StatusBadRequest, "缺少文件路径")
		return
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		c.String(http.StatusBadRequest, "非法路径")
		return
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		c.String(http.StatusInternalServerError, "工作目录无效")
		return
	}
	abs := filepath.Join(rootAbs, rel)
	// 越界二次校验：Join 后必须仍位于工作目录内（防 "a/../../x"、"C:\\x" 等写法）。
	if inside, err := filepath.Rel(rootAbs, abs); err != nil || inside == ".." ||
		strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		c.String(http.StatusBadRequest, "非法路径")
		return
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		c.String(http.StatusNotFound, "文件不存在")
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		c.String(http.StatusNotFound, "文件不存在")
		return
	}
	defer f.Close()

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	// ServeContent 自行按扩展名推断 Content-Type 并处理 Range；
	// 未知扩展名回落 application/octet-stream（浏览器会下载而非执行）。
	http.ServeContent(c.Writer, c.Request, filepath.Base(abs), modTimeOf(info), f)
}

// modTimeOf 取文件修改时间（失败回落零值，ServeContent 会跳过 Last-Modified）。
func modTimeOf(info os.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	return info.ModTime()
}
