package main

// providers.go 媒体生成的上游协议适配（原生媒体服务的"渲染器"部分）。
//
// 槽位与协议一一对应（与容器 daemon 的渲染器命名保持一致）：
//   custom-image  OpenAI 兼容 /images/generations（聚合网关 / 方舟 / MiniMax 桥）
//   nanobanana    Gemini  :generateContent
//   aihubmix      OpenAI /videos（异步任务）
//   volcengine    方舟 /contents/generations/tasks（异步任务）
//   grok          POST {base}/videos/generations（同步形状，返回 video_url）

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// generateReq 生成请求（与 main.go 的匿名结构一致）。
type generateReq struct {
	Surface  string
	Model    string
	Prompt   string
	Aspect   string
	Image    string
	Seed     *int
	Negative string
}

// runTask 执行生成任务：调上游 → 落盘 → 更新任务状态。
func (s *server) runTask(projID string, t *mediaTask, req generateReq) {
	s.setTask(t, func(x *mediaTask) { x.Status = "running" })

	slot, ok := s.slotFor(req.Surface, req.Model)
	if !ok {
		s.failTask(t, "没有可用通道（.env 媒体段未配置，或所选槽位的 model 为空）")
		return
	}

	var (
		data []byte
		ext  string
		err  error
	)
	switch req.Surface {
	case "image":
		data, ext, err = s.generateImage(slot, req, projID)
	case "video":
		data, ext, err = s.generateVideo(slot, req)
	default:
		err = fmt.Errorf("未知的生成类型：%s", req.Surface)
	}
	if err != nil {
		s.failTask(t, err.Error())
		return
	}

	name := fmt.Sprintf("%s.%s", t.TaskID, ext)
	path := filepath.Join(s.dataDir, projID, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		s.failTask(t, "产物落盘失败："+err.Error())
		return
	}
	s.setTask(t, func(x *mediaTask) {
		x.Status = "done"
		x.File = &taskFile{FileName: name, Size: int64(len(data))}
	})
}

// slotFor 选通道：先按模型 id 精确匹配，再按类型兜底。
func (s *server) slotFor(surface, model string) (providerSlot, bool) {
	if model != "" {
		for _, slot := range s.cfg.Providers {
			if slot.Model == model {
				return slot, true
			}
		}
	}
	// 兜底顺序：图像 custom-image → nanobanana；视频 volcengine → aihubmix → grok
	var order []string
	if surface == "video" {
		order = []string{"volcengine", "aihubmix", "grok"}
	} else {
		order = []string{"custom-image", "nanobanana"}
	}
	for _, name := range order {
		if slot, ok := s.cfg.Providers[name]; ok && slot.Model != "" {
			return slot, true
		}
	}
	return providerSlot{}, false
}

// setTask 改任务状态（加锁）。
func (s *server) setTask(t *mediaTask, fn func(*mediaTask)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(t)
}

// failTask 标记任务失败。
func (s *server) failTask(t *mediaTask, msg string) {
	s.setTask(t, func(x *mediaTask) {
		x.Status = "failed"
		x.Error = &taskErr{Message: msg}
	})
}

// generateImage 生图（custom-image / nanobanana）。
//
// 带参考图（req.Image 非空）时走 OpenAI 兼容的 /images/edits（multipart），
// 对应 subject_reference / image_edit 这类"以图为条件"的通道。
func (s *server) generateImage(slot providerSlot, req generateReq, projID string) ([]byte, string, error) {
	if slot.BaseURL == "" {
		return nil, "", errors.New("图像通道未配置接口地址（.env 的 IMAGE_URL）")
	}
	if req.Image != "" {
		return s.generateImageEdit(slot, req, projID)
	}
	// nanobanana 槽位名以 nanobanana 为准（配置里只有它是 Gemini 协议）。
	if s.isNanoSlot(slot) {
		return s.generateImageNano(slot, req)
	}
	return s.generateImageOpenAI(slot, req)
}

// generateImageEdit 参考图编辑（i2i / subject_reference）。
//
// 形状对齐容器 daemon 的补丁：**同一个 generations 端点 + JSON**，参考图以 data URL
// 放在 image 字段（上游桥按这个形状解析）。不用 multipart —— 上游（MiniMax 适配桥、
// 方舟 i2i）都收 JSON + data URL。
func (s *server) generateImageEdit(slot providerSlot, req generateReq, projID string) ([]byte, string, error) {
	refPath := filepath.Join(s.dataDir, projID, filepath.Base(req.Image))
	ref, err := os.ReadFile(refPath)
	if err != nil {
		return nil, "", fmt.Errorf("参考图不存在：%s（%v）", req.Image, err)
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(req.Image)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(ref))

	body := map[string]any{
		"model":           slot.Model,
		"prompt":          req.Prompt,
		"size":            sizeForAspect(req.Aspect),
		"n":               1,
		"response_format": "b64_json",
		"image":           dataURL,
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.Negative != "" {
		body["negative_prompt"] = req.Negative
	}
	raw, err := s.postJSON(imagesEndpoint(slot.BaseURL), slot.apiKey, body)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	if len(resp.Data) == 0 {
		return nil, "", fmt.Errorf("上游未返回图像数据（%s）", truncate(string(raw), 300))
	}
	if resp.Data[0].B64JSON != "" {
		b, err := base64.StdEncoding.DecodeString(resp.Data[0].B64JSON)
		if err != nil {
			return nil, "", fmt.Errorf("图像 base64 解码失败: %w", err)
		}
		return b, "png", nil
	}
	if resp.Data[0].URL != "" {
		b, err := s.fetchBytes(resp.Data[0].URL)
		if err != nil {
			return nil, "", err
		}
		return b, extFromURL(resp.Data[0].URL), nil
	}
	return nil, "", errors.New("上游返回的图像既没有 b64_json 也没有 url")
}

// isNanoSlot 判断是不是 Gemini 通道（按 baseUrl 形态 + 模型名兜底）。
func (s *server) isNanoSlot(slot providerSlot) bool {
	if strings.Contains(slot.BaseURL, "generativelanguage.googleapis.com") {
		return true
	}
	if slot.BaseURL != "" && !strings.Contains(slot.BaseURL, "/v1") && !strings.Contains(slot.BaseURL, "http://127.0.0.1") {
		// 非 OpenAI 兼容形态的网关根（没带 /v1）→ 多半是 Gemini 网关
		return true
	}
	return false
}

// generateImageOpenAI OpenAI 兼容 /images/generations。
func (s *server) generateImageOpenAI(slot providerSlot, req generateReq) ([]byte, string, error) {
	url := imagesEndpoint(slot.BaseURL)
	body := map[string]any{
		"model":           slot.Model,
		"prompt":          req.Prompt,
		"size":            sizeForAspect(req.Aspect),
		"n":               1,
		"response_format": "b64_json",
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if req.Negative != "" {
		body["negative_prompt"] = req.Negative
	}
	raw, err := s.postJSON(url, slot.apiKey, body)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	if len(resp.Data) == 0 {
		return nil, "", fmt.Errorf("上游未返回图像数据（%s）", truncate(string(raw), 300))
	}
	if resp.Data[0].B64JSON != "" {
		b, err := base64.StdEncoding.DecodeString(resp.Data[0].B64JSON)
		if err != nil {
			return nil, "", fmt.Errorf("图像 base64 解码失败: %w", err)
		}
		return b, "png", nil
	}
	if resp.Data[0].URL != "" {
		b, err := s.fetchBytes(resp.Data[0].URL)
		if err != nil {
			return nil, "", err
		}
		return b, extFromURL(resp.Data[0].URL), nil
	}
	return nil, "", errors.New("上游返回的图像既没有 b64_json 也没有 url")
}

// generateImageNano Gemini :generateContent。
func (s *server) generateImageNano(slot providerSlot, req generateReq) ([]byte, string, error) {
	root := strings.TrimRight(slot.BaseURL, "/")
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", root, slot.Model)
	body := map[string]any{
		"contents": []any{map[string]any{
			"parts": []any{map[string]any{"text": req.Prompt}},
		}},
	}
	raw, err := s.postJSON(url, slot.apiKey, body)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, "", errors.New(resp.Error.Message)
	}
	for _, c := range resp.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData.Data != "" {
				b, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
				if err != nil {
					return nil, "", fmt.Errorf("图像 base64 解码失败: %w", err)
				}
				ext := "png"
				if strings.Contains(p.InlineData.MimeType, "jpeg") {
					ext = "jpg"
				}
				return b, ext, nil
			}
		}
	}
	return nil, "", fmt.Errorf("上游未返回图像数据（%s）", truncate(string(raw), 300))
}

// generateVideo 生视频（volcengine / aihubmix / grok）。
func (s *server) generateVideo(slot providerSlot, req generateReq) ([]byte, string, error) {
	if slot.BaseURL == "" {
		return nil, "", errors.New("视频通道未配置接口地址（.env 的 VIDEO_URL）")
	}
	root := strings.TrimRight(slot.BaseURL, "/")

	switch {
	case slotLooksGrok(slot):
		return s.generateVideoGrok(slot, req, root)
	case slotLooksVolcengine(slot):
		return s.generateVideoVolcengine(slot, req, root)
	default:
		return s.generateVideoAIHubMix(slot, req, root)
	}
}

// slotLooksGrok 判断是不是 grok 线（同步 video_url 形状）。
func slotLooksGrok(slot providerSlot) bool {
	return strings.Contains(strings.ToLower(slot.BaseURL), "8090") ||
		strings.Contains(strings.ToLower(slot.BaseURL), "grok")
}

// slotLooksVolcengine 判断是不是方舟 tasks 协议。
func slotLooksVolcengine(slot providerSlot) bool {
	return strings.Contains(strings.ToLower(slot.BaseURL), "ark.cn-beijing") ||
		strings.Contains(strings.ToLower(slot.BaseURL), "volces")
}

// generateVideoGrok POST {base}/videos/generations（同步形状）。
func (s *server) generateVideoGrok(slot providerSlot, req generateReq, root string) ([]byte, string, error) {
	raw, err := s.postJSON(root+"/videos/generations", slot.apiKey, map[string]any{
		"model":  slot.Model,
		"prompt": req.Prompt,
	})
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		VideoURL string `json:"video_url"`
		Data     []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	url := resp.VideoURL
	if url == "" && len(resp.Data) > 0 {
		url = resp.Data[0].URL
	}
	if url == "" {
		return nil, "", fmt.Errorf("上游未返回视频地址（%s）", truncate(string(raw), 300))
	}
	b, err := s.fetchBytes(url)
	if err != nil {
		return nil, "", err
	}
	return b, extFromURL(url), nil
}

// generateVideoVolcengine 方舟 tasks 协议（提交 → 轮询）。
func (s *server) generateVideoVolcengine(slot providerSlot, req generateReq, root string) ([]byte, string, error) {
	raw, err := s.postJSON(root+"/contents/generations/tasks", slot.apiKey, map[string]any{
		"model":   slot.Model,
		"content": []any{map[string]any{"type": "text", "text": req.Prompt}},
	})
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		ID     string `json:"id"`
		Error  *struct{ Message string `json:"message"` } `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return nil, "", errors.New(resp.Error.Message)
	}
	if resp.ID == "" {
		return nil, "", fmt.Errorf("上游未返回任务 id（%s）", truncate(string(raw), 300))
	}
	url, err := s.pollTask(root+"/contents/generations/tasks/"+resp.ID, slot.apiKey,
		func(body []byte) (string, bool, string) {
			var r struct {
				Status  string `json:"status"`
				Content struct {
					VideoURL string `json:"video_url"`
				} `json:"content"`
				Error *struct{ Message string `json:"message"` } `json:"error"`
			}
			_ = json.Unmarshal(body, &r)
			switch r.Status {
			case "succeeded", "success", "completed":
				return r.Content.VideoURL, true, ""
			case "failed", "error", "cancelled":
				msg := r.Error.Message
				if msg == "" {
					msg = "上游返回 " + r.Status
				}
				return "", true, msg
			default:
				return "", false, ""
			}
		})
	if err != nil {
		return nil, "", err
	}
	b, err := s.fetchBytes(url)
	if err != nil {
		return nil, "", err
	}
	return b, extFromURL(url), nil
}

// generateVideoAIHubMix OpenAI /videos 协议（提交 → 轮询）。
func (s *server) generateVideoAIHubMix(slot providerSlot, req generateReq, root string) ([]byte, string, error) {
	raw, err := s.postJSON(root+"/videos", slot.apiKey, map[string]any{
		"model":  slot.Model,
		"prompt": req.Prompt,
	})
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		ID      string `json:"id"`
		VideoURL string `json:"video_url"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, "", fmt.Errorf("上游响应解析失败: %v（%s）", err, truncate(string(raw), 300))
	}
	if resp.VideoURL != "" {
		b, err := s.fetchBytes(resp.VideoURL)
		if err != nil {
			return nil, "", err
		}
		return b, extFromURL(resp.VideoURL), nil
	}
	if resp.ID == "" {
		return nil, "", fmt.Errorf("上游未返回任务 id（%s）", truncate(string(raw), 300))
	}
	url, err := s.pollTask(root+"/videos/"+resp.ID, slot.apiKey,
		func(body []byte) (string, bool, string) {
			var r struct {
				Status string `json:"status"`
				URL    string `json:"url"`
				Data   []struct {
					URL string `json:"url"`
				} `json:"data"`
				Error *struct{ Message string `json:"message"` } `json:"error"`
			}
			_ = json.Unmarshal(body, &r)
			switch r.Status {
			case "completed", "succeeded", "success":
				u := r.URL
				if u == "" && len(r.Data) > 0 {
					u = r.Data[0].URL
				}
				return u, true, ""
			case "failed", "error", "cancelled":
				msg := r.Error.Message
				if msg == "" {
					msg = "上游返回 " + r.Status
				}
				return "", true, msg
			default:
				return "", false, ""
			}
		})
	if err != nil {
		return nil, "", err
	}
	b, err := s.fetchBytes(url)
	if err != nil {
		return nil, "", err
	}
	return b, extFromURL(url), nil
}

// pollTask 轮询异步任务直到出结果。
func (s *server) pollTask(url, apiKey string, parse func([]byte) (string, bool, string)) (string, error) {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		raw, err := s.getJSON(url, apiKey)
		if err != nil {
			return "", err
		}
		result, done, failMsg := parse(raw)
		if done {
			if failMsg != "" {
				return "", errors.New(failMsg)
			}
			if result == "" {
				return "", errors.New("上游任务完成但未返回产物地址")
			}
			return result, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", errors.New("等待上游视频任务超时")
}

// postJSON POST JSON 并返回原始响应体。
func (s *server) postJSON(url, apiKey string, body any) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return s.doRequest(req)
}

// getJSON GET 并返回原始响应体。
func (s *server) getJSON(url, apiKey string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return s.doRequest(req)
}

// doRequest 执行请求并处理非 2xx。
func (s *server) doRequest(req *http.Request) ([]byte, error) {
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游返回 %d：%s", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
}

// fetchBytes 下载二进制（产物）。
func (s *server) fetchBytes(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return s.doRequest(req)
}

// imagesEndpoint 归一生图端点：配置里给 API 根就补 /images/generations，
// 已经给了全路径就直接用（两种写法在 .env 里都出现过）。
func imagesEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/images/generations") {
		return base
	}
	return base + "/images/generations"
}

// sizeForAspect 画幅 → 像素尺寸（与容器补丁同一套映射）。
func sizeForAspect(aspect string) string {
	switch aspect {
	case "16:9":
		return "2048x1152"
	case "9:16":
		return "1152x2048"
	case "4:3":
		return "2048x1536"
	case "3:4":
		return "1536x2048"
	default:
		return "2048x2048"
	}
}

// extFromURL 从 URL 猜扩展名。
func extFromURL(url string) string {
	base := url
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	ext := strings.ToLower(filepath.Ext(base))
	switch ext {
	case ".mp4", ".webm", ".mov", ".mkv":
		return ext[1:]
	case ".png", ".jpg", ".jpeg", ".webp":
		return ext[1:]
	default:
		return "mp4"
	}
}

// truncate 截断长文本（错误信息用）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
