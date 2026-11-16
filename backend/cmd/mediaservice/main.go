// mediaservice 原生媒体服务：替代 Docker 里的 open_design daemon，
// 为随包的 ui_design 桥（Node stdio MCP）提供它实际用到的那几个 HTTP 端点。
//
// 为什么要有它：桌面版要做到免装 Docker，而媒体生成是核心能力。daemon 上游是
// 容器镜像（编译后的完整应用），vendor 不进安装包；桥只用到 8 个端点，这里按同样
// 的协议实现一遍，**配置来源仍是 .env 的媒体段**（与容器 entrypoint 的映射一致，
// 槽位名、能力声明、URL 归一规则都照抄，避免"两套配置语义"）。
//
// 端点（桥用到的子集）：
//
//	GET  /api/media/config                     通道槽位（桥据此探测能力与模型）
//	GET  /api/media/models                     模型目录（可选，桥容错）
//	POST /api/projects                         建项目
//	GET  /api/projects                         项目列表
//	POST /api/projects/{id}/files              上传（multipart）
//	GET  /api/projects/{id}/files/{name}       取文件（?download=1）
//	GET  /api/projects/{id}/raw/{name}         取文件（旧路由，桥会依次尝试）
//	POST /api/projects/{id}/media/generate     受理生成任务 → {taskId}
//	GET  /api/projects/{id}/media/tasks        任务列表（?includeDone=1）
//
// 与 daemon 的差异（有意为之）：
//   * 只支持"纯文本生成"（t2i / t2v）。参考图类操作（subject_reference /
//     image_edit / i2v）明确报错而不是假装支持——猜错会给用户一张错的图。
//   * 只监听 127.0.0.1，不做鉴权（仅本机可达）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// providerSlot 一个媒体通道槽位（与 daemon 的 media-config.json 同构）。
type providerSlot struct {
	BaseURL      string   `json:"baseUrl,omitempty"`
	Model        string   `json:"model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`

	apiKey string // 不出现在 HTTP 响应里
}

// mediaConfig 对应 GET /api/media/config 的响应体。
type mediaConfig struct {
	Providers map[string]providerSlot `json:"providers"`
}

// taskErr 任务错误（桥读 error.message）。
type taskErr struct {
	Message string `json:"message"`
}

// taskFile 产物文件元数据（桥读 file.fileName / file.name）。
type taskFile struct {
	FileName string `json:"fileName"`
	Size     int64  `json:"size,omitempty"`
}

// mediaTask 一个生成任务。
type mediaTask struct {
	TaskID    string    `json:"taskId"`
	Surface   string    `json:"surface"`
	Model     string    `json:"model"`
	Prompt    string    `json:"prompt,omitempty"`
	Status    string    `json:"status"` // queued | running | done | failed | interrupted
	Error     *taskErr  `json:"error,omitempty"`
	File      *taskFile `json:"file,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// project 一个项目（桥用一个固定 id）。
type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// server 媒体服务。
type server struct {
	mu       sync.Mutex
	cfg      mediaConfig
	projects map[string]*project
	tasks    map[string]*mediaTask
	// dataDir 项目文件落盘目录（每个项目一个子目录）。
	dataDir string
	// httpClient 调上游用（生图/生视频可能很慢，超时给足）。
	httpClient *http.Client
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7456", "监听地址")
	dataDir := flag.String("data", "", "数据目录（项目产物落盘处）")
	flag.Parse()

	if *dataDir == "" {
		d, err := os.Getwd()
		if err != nil {
			log.Fatalf("无法确定工作目录: %v", err)
		}
		*dataDir = filepath.Join(d, "media-data")
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	s := &server{
		cfg:        loadMediaConfig(),
		projects:   map[string]*project{},
		tasks:      map[string]*mediaTask{},
		dataDir:    *dataDir,
		httpClient: &http.Client{Timeout: 20 * time.Minute},
	}
	s.logProviders()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/media/config", s.handleMediaConfig)
	mux.HandleFunc("/api/media/models", s.handleMediaModels)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/projects/", s.handleProjectPath)

	log.Printf("媒体服务已启动：http://%s（数据目录 %s）", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("媒体服务退出: %v", err)
	}
}

// loadMediaConfig 从环境变量构造通道槽位。
//
// 与容器 entrypoint.sh 同一套映射：IMAGE_PROVIDER / IMAGE_URL / IMAGE_MODEL /
// IMAGE_KEY / IMAGE_CAPABILITIES 与 VIDEO_* 一组；未选中的槽位必须清掉 model
// （桥以"providers.<槽>.model 非空"判定用户选了哪个通道）。
func loadMediaConfig() mediaConfig {
	cfg := mediaConfig{Providers: map[string]providerSlot{}}

	imageProvider := strings.ToLower(envOr("IMAGE_PROVIDER", "custom-image"))
	videoProvider := strings.ToLower(envOr("VIDEO_PROVIDER", "volcengine"))
	imageDeclared := hasEnv("IMAGE_PROVIDER", "IMAGE_URL", "IMAGE_MODEL")
	videoDeclared := hasEnv("VIDEO_PROVIDER", "VIDEO_URL", "VIDEOS_URL", "VIDEO_MODEL", "VIDEOS_MODEL")

	imageCaps := capsList(os.Getenv("IMAGE_CAPABILITIES"))
	videoCaps := capsList(os.Getenv("VIDEO_CAPABILITIES"))
	imageURL := toNanoRoot(os.Getenv("IMAGE_URL"))
	videoURL := toApiRoot(firstEnv("VIDEO_URL", "VIDEOS_URL"))
	// 桌面版没有 Docker 网络："minimax-media"（容器服务名）指向随包自带的本地适配桥。
	imageURL = localBridgeHost(imageURL)
	videoURL = localBridgeHost(videoURL)
	imageModel := os.Getenv("IMAGE_MODEL")
	videoModel := firstEnv("VIDEO_MODEL", "VIDEOS_MODEL")
	imageKey := os.Getenv("IMAGE_KEY")
	videoKey := os.Getenv("VIDEO_KEY")

	if imageDeclared {
		switch imageProvider {
		case "nanobanana":
			cfg.Providers["nanobanana"] = providerSlot{
				BaseURL: imageURL, Model: imageModel, Capabilities: imageCaps,
				apiKey: firstEnvOr(imageKey, "OD_NANOBANANA_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY"),
			}
		default: // custom-image：OpenAI 兼容 /images/generations
			cfg.Providers["custom-image"] = providerSlot{
				BaseURL: imageURL, Model: imageModel, Capabilities: imageCaps,
				apiKey: firstEnvOr(imageKey, "OD_CUSTOM_IMAGE_API_KEY"),
			}
		}
	}
	if videoDeclared {
		switch videoProvider {
		case "openai": // aihubmix 槽位：OpenAI /videos 协议
			cfg.Providers["aihubmix"] = providerSlot{
				BaseURL: videoURL, Model: videoModel, Capabilities: videoCaps,
				apiKey: firstEnvOr(videoKey, "OD_AIHUBMIX_API_KEY"),
			}
		case "grok": // grok 线：POST {base}/videos/generations（同步形状）
			cfg.Providers["grok"] = providerSlot{
				BaseURL: videoURL, Model: videoModel, Capabilities: videoCaps,
				apiKey: firstEnvOr(videoKey, "OD_GROK_API_KEY"),
			}
		default: // volcengine：方舟 tasks 协议
			cfg.Providers["volcengine"] = providerSlot{
				BaseURL: videoURL, Model: videoModel, Capabilities: videoCaps,
				apiKey: firstEnvOr(videoKey, "ARK_API_KEY"),
			}
		}
	}
	return cfg
}

// logProviders 打印生效摘要（不含密钥）。
func (s *server) logProviders() {
	for name, slot := range s.cfg.Providers {
		log.Printf("媒体通道 %s：model=%s baseUrl=%s 能力=%v 密钥=%s",
			name, slot.Model, slot.BaseURL, slot.Capabilities, keyState(slot.apiKey))
	}
}

// keyState 密钥状态（只说有没有，不说内容）。
func keyState(k string) string {
	if k == "" {
		return "缺失"
	}
	return fmt.Sprintf("已配置(%d 字符)", len(k))
}

// handleMediaConfig 返回通道槽位（不含密钥）。
func (s *server) handleMediaConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg)
}

// handleMediaModels 返回模型目录（桥容错，失败也不致命）。
func (s *server) handleMediaModels(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"image": []any{}, "video": []any{}}
	for name, slot := range s.cfg.Providers {
		if slot.Model == "" {
			continue
		}
		entry := map[string]any{"id": slot.Model, "provider": name, "caps": slot.Capabilities}
		if strings.Contains(name, "custom-image") || name == "nanobanana" {
			out["image"] = append(out["image"].([]any), entry)
		} else {
			out["video"] = append(out["video"].([]any), entry)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProjects 建项目 / 列项目。
func (s *server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == "" {
			req.ID = "bma-agent-artifacts"
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.projects[req.ID]; exists {
			// 已存在：daemon 侧是 409，桥会回查列表确认
			writeJSON(w, http.StatusConflict, map[string]string{"error": "project already exists"})
			return
		}
		s.projects[req.ID] = &project{ID: req.ID, Name: req.Name}
		if err := os.MkdirAll(filepath.Join(s.dataDir, req.ID), 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.projects[req.ID])
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		list := make([]*project, 0, len(s.projects))
		for _, p := range s.projects {
			list = append(list, p)
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": list})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleProjectPath 分发项目子路径。
func (s *server) handleProjectPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	projID := parts[0]
	tail := parts[1:]

	switch {
	case len(tail) == 1 && tail[0] == "media" && r.Method == http.MethodGet:
		// /api/projects/{id}/media（daemon 有这个路由，这里返回空目录）
		writeJSON(w, http.StatusOK, map[string]any{"projects": []any{}})
	case len(tail) == 2 && tail[0] == "media" && tail[1] == "generate" && r.Method == http.MethodPost:
		s.handleGenerate(w, r, projID)
	case len(tail) == 2 && tail[0] == "media" && tail[1] == "tasks" && r.Method == http.MethodGet:
		s.handleTasks(w, r, projID)
	case len(tail) == 1 && tail[0] == "files" && r.Method == http.MethodPost:
		s.handleUpload(w, r, projID)
	case len(tail) >= 2 && tail[0] == "files" && r.Method == http.MethodGet:
		s.handleDownload(w, r, projID, strings.Join(tail[1:], "/"))
	case len(tail) >= 2 && tail[0] == "raw" && r.Method == http.MethodGet:
		s.handleDownload(w, r, projID, strings.Join(tail[1:], "/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleGenerate 受理生成任务（异步执行，立即返回 taskId）。
func (s *server) handleGenerate(w http.ResponseWriter, r *http.Request, projID string) {
	var req struct {
		Surface  string `json:"surface"`
		Model    string `json:"model"`
		Prompt   string `json:"prompt"`
		Aspect   string `json:"aspect"`
		Image    string `json:"image"`
		Seed     *int   `json:"seed"`
		Negative string `json:"negative_prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	t := &mediaTask{
		TaskID:    fmt.Sprintf("bmt_%d", time.Now().UnixNano()),
		Surface:   req.Surface,
		Model:     req.Model,
		Prompt:    req.Prompt,
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	s.tasks[t.TaskID] = t
	s.mu.Unlock()

	go s.runTask(projID, t, generateReq{
		Surface:  req.Surface,
		Model:    req.Model,
		Prompt:   req.Prompt,
		Aspect:   req.Aspect,
		Image:    req.Image,
		Seed:     req.Seed,
		Negative: req.Negative,
	})

	writeJSON(w, http.StatusOK, map[string]string{"taskId": t.TaskID})
}

// handleTasks 任务列表（includeDone=1 时含终态）。
func (s *server) handleTasks(w http.ResponseWriter, r *http.Request, projID string) {
	includeDone := r.URL.Query().Get("includeDone") == "1"
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []*mediaTask{}
	for _, t := range s.tasks {
		if !includeDone && (t.Status == "done" || t.Status == "failed" || t.Status == "interrupted") {
			continue
		}
		list = append(list, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": list})
}

// handleUpload 接收项目文件（multipart）。
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request, projID string) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 file 字段"})
		return
	}
	defer file.Close()
	name := header.Filename
	if name == "" {
		name = "upload.bin"
	}
	path := filepath.Join(s.dataDir, projID, filepath.Base(name))
	out, err := os.Create(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": map[string]string{"name": filepath.Base(name)}})
}

// handleDownload 返回项目文件字节。
func (s *server) handleDownload(w http.ResponseWriter, r *http.Request, projID, name string) {
	path := filepath.Join(s.dataDir, projID, filepath.Base(name))
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// envOr 取环境变量（空则用默认值）。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// hasEnv 任一环境变量有值。
func hasEnv(keys ...string) bool {
	for _, k := range keys {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

// firstEnv 取第一个有值的环境变量（参数是**变量名**）。
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// firstEnvOr 取第一个有值的环境变量；都没有则用 def（def 是**值**，不是变量名）。
func firstEnvOr(def string, keys ...string) string {
	if v := firstEnv(keys...); v != "" {
		return v
	}
	return def
}

// capsList 解析能力声明（逗号/空格分隔）。
func capsList(raw string) []string {
	out := []string{}
	for _, s := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// localBridgeHost 把容器服务名换成回环地址（桌面版自带本地适配桥）。
func localBridgeHost(u string) string {
	if u == "" {
		return u
	}
	for _, host := range []string{"minimax-media", "open_design"} {
		u = strings.Replace(u, "//"+host+":", "//127.0.0.1:", 1)
		u = strings.Replace(u, "//"+host+"/", "//127.0.0.1/", 1)
	}
	return u
}

// toApiRoot 归一视频 API 根（剥掉协议后缀，避免拼出双路径 404）。
func toApiRoot(u string) string {
	if u == "" {
		return u
	}
	for _, suffix := range []string{"/contents/generations/tasks", "/videos/generations", "/videos"} {
		if strings.HasSuffix(strings.TrimRight(u, "/"), suffix) {
			return strings.TrimRight(u, "/")[:len(strings.TrimRight(u, "/"))-len(suffix)]
		}
	}
	return u
}

// toNanoRoot 归一 Gemini 网关根（剥掉 generateContent 全路径）。
func toNanoRoot(u string) string {
	if u == "" {
		return u
	}
	if i := strings.Index(u, "/v1beta/models/"); i >= 0 {
		return u[:i]
	}
	return u
}
