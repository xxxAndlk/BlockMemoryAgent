// show_artifact.go 提供 ShowArtifact 工具：把 Agent 产出的**可视成果**（效果图/视频/
// 音频/HTML 原型/界面部件）显式"拿给用户看"——对话栏据此渲染媒体卡片，而不是让用户对着
// 一串路径自行想象。
//
// 与 ReadMedia 的分工：ReadMedia 是"把媒体读进**模型**上下文"（base64 喂多模态），
// ShowArtifact 是"把媒体呈现给**用户**"（只传相对路径 + 标题，零二进制进事件流）。
//
// 内联 HTML（content 参数）统一落盘到 <工作目录>/.bma/artifacts/ 再展示：
// 走同一条"路径引用"通道，历史回放/新窗口打开/分享都自然可用。
//
// kind=widget 是独立分支：引用 widgets/ 目录包里 Agent 自写的界面部件（widget_save
// 工具创建），工件 path 为 widget://<id>，对话栏渲染成 sandbox iframe 卡片；
// 不校验扩展名、不要求工作区文件，但要求部件存在且已启用（经 Registry 注入的
// widgetLookup 校验，未接线时拒绝并提示）。
package tool

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// artifactExts 各展示类型允许的扩展名（小写，含点）。
var artifactExts = map[string]map[string]string{
	"image": {".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml"},
	"video": {".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".mkv": "video/x-matroska"},
	"audio": {".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4", ".ogg": "audio/ogg", ".flac": "audio/flac"},
	"html":  {".html": "text/html", ".htm": "text/html"},
}

// artifactInlineHTMLMaxBytes 是 content 内联 HTML 的上限：防把整份数据当 HTML 贴进来。
const artifactInlineHTMLMaxBytes = 2 << 20

// artifactWidgetDataMaxBytes 是 kind=widget 的 data 初始数据上限（64KB）：
// 与 widget_data 瞬时通道同口径——初始值走 detail_json 落库，不能无界膨胀历史。
const artifactWidgetDataMaxBytes = 64 << 10

// showArtifactTool 是 ShowArtifact 工具的封装。
type showArtifactTool struct {
	exec *Executor
	// widgetLookup 校验 kind=widget 引用的部件存在且已启用（bootstrap 注入，
	// 接 widgets.Manager.Get）；nil 时 kind=widget 一律拒绝并提示未接线。
	widgetLookup func(id string) (name string, enabled bool, ok bool)
}

func (t *showArtifactTool) Name() string      { return "ShowArtifact" }
func (t *showArtifactTool) Aliases() []string { return []string{"show_artifact"} }

func (t *showArtifactTool) Description() string {
	return "把产出的可视成果直接展示给用户：对话栏会渲染成图片/视频/音频/HTML 预览卡片或界面部件卡片。" +
		"产出效果图、截图、录屏、UI 原型（HTML）后调用本工具让用户直接看到效果，" +
		"不要只在终答里贴文件路径。参数：kind=image|video|audio|html|widget；" +
		"path=工作区内文件路径（图片/视频/音频/已有 HTML 文件）；" +
		"kind=html 时也可用 content 直接给 HTML 源码（自动落盘到 .bma/artifacts/ 再展示）；" +
		"kind=widget 时用 widget_id 引用已创建的界面部件（widget_save 创建，须已启用），" +
		"可选 data 给初始数据（JSON 字符串）。" +
		"一次一个成果，多个成果多次调用；title/caption 是卡片标题与说明。"
}

func (t *showArtifactTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"kind": {
				Type:        "string",
				Enum:        []any{"image", "video", "audio", "html", "widget"},
				Description: "展示类型",
			},
			"path": {
				Type:        "string",
				Description: "工作区内文件路径（相对工作目录；kind=html 时也可省略改用 content）",
			},
			"content": {
				Type:        "string",
				Description: "kind=html 时的 HTML 源码（与 path 二选一；自动落盘到 .bma/artifacts/）",
			},
			"widget_id": {
				Type:        "string",
				Description: "kind=widget 时的部件 id（widget_save 创建；须已启用）",
			},
			"data": {
				Type:        "string",
				Description: "kind=widget 时给部件的初始数据（JSON 字符串，可选，≤64KB）",
			},
			"title":   {Type: "string", Description: "卡片标题（可选）"},
			"caption": {Type: "string", Description: "一句话说明（可选，如“主界面效果图 v2”）"},
		},
		Required: []string{"kind"},
	}
}

// Execute 校验并登记一个可视成果。
// 失败一律返回 validation_rejected（参数问题，修正即可），不抛错中断 ReAct 循环。
func (t *showArtifactTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "ShowArtifact"}
	kind := strings.ToLower(strings.TrimSpace(strArg(args, "kind")))
	// kind=widget 走独立分支：引用 widgets/ 目录包而非工作区文件，
	// 不校验扩展名、不解析工作目录。
	if kind == "widget" {
		return t.executeWidget(args)
	}
	exts, ok := artifactExts[kind]
	if !ok {
		res.Error = fmt.Sprintf("kind 非法 %q（可选 image/video/audio/html/widget）", kind)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	pathArg := strings.TrimSpace(strArg(args, "path"))
	content := strArg(args, "content")
	title := strings.TrimSpace(strArg(args, "title"))
	caption := strings.TrimSpace(strArg(args, "caption"))

	workDir := t.workDirFor(ctx)
	if workDir == "" {
		res.Error = "工作目录未注入，无法解析成果路径"
		res.Category = ResultCategoryValidationRejected
		return res
	}

	var abs string
	switch {
	case kind == "html" && content != "" && pathArg == "":
		// 内联 HTML：落盘后再展示（统一走路径引用通道，可回放/可新窗口打开）。
		if len(content) > artifactInlineHTMLMaxBytes {
			res.Error = fmt.Sprintf("HTML 内容 %d 字节超过上限 %d", len(content), artifactInlineHTMLMaxBytes)
			res.Category = ResultCategoryValidationRejected
			return res
		}
		var err error
		abs, err = t.writeInlineHTML(workDir, content)
		if err != nil {
			res.Error = fmt.Sprintf("写入 HTML 产物失败: %v", err)
			res.Category = ResultCategoryExecutionFailed
			return res
		}
	case pathArg != "":
		if t.exec == nil {
			res.Error = "executor 未接线"
			return res
		}
		var err error
		abs, err = t.exec.resolvePathWithSandbox(ctx, pathArg)
		if err != nil {
			res.Error = err.Error()
			res.Category = ResultCategoryValidationRejected
			return res
		}
	default:
		res.Error = "缺少成果来源：给 path，或 kind=html 时给 content"
		res.Category = ResultCategoryValidationRejected
		return res
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		res.Error = fmt.Sprintf("成果文件不存在: %s", filepath.Base(abs))
		res.Category = ResultCategoryValidationRejected
		return res
	}
	ext := strings.ToLower(filepath.Ext(abs))
	mime, ok := exts[ext]
	if !ok {
		res.Error = fmt.Sprintf("文件 %s 扩展名与 kind=%s 不符（该类型支持 %s）",
			filepath.Base(abs), kind, strings.Join(sortedKeys(exts), "/"))
		res.Category = ResultCategoryValidationRejected
		return res
	}

	rel, err := t.relToWorkDir(workDir, abs)
	if err != nil {
		res.Error = "成果不在工作目录内，无法展示（对话栏只能引用工作区内文件）"
		res.Category = ResultCategoryValidationRejected
		return res
	}

	res.Success = true
	res.Path = abs
	res.Artifacts = []Artifact{{Kind: kind, Path: rel, Title: title, Caption: caption, MIME: mime}}
	res.Output = fmt.Sprintf("已把 %s 展示给用户（对话栏出现 %s 卡片）：%s。不要在终答里重复贴路径，直接说明这是什么、要看哪里即可。",
		artifactLabel(kind, title, rel), kindLabelCN(kind), rel)
	return res
}

// executeWidget 处理 kind=widget：引用 widgets/ 目录包里的界面部件。
// 部件由 widget_save 工具创建（Agent 自写渲染代码），工件 path 为 widget://<id>，
// 对话栏渲染成 sandbox iframe 卡片；data 是卡片挂载时下发给部件的初始数据。
func (t *showArtifactTool) executeWidget(args map[string]any) *Result {
	res := &Result{Tool: "ShowArtifact"}
	widgetID := strings.TrimSpace(strArg(args, "widget_id"))
	data := strArg(args, "data")
	title := strings.TrimSpace(strArg(args, "title"))
	caption := strings.TrimSpace(strArg(args, "caption"))

	if widgetID == "" {
		res.Error = "kind=widget 缺少必填参数 widget_id"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if t.widgetLookup == nil {
		res.Error = "界面部件校验未接线（widgets 管理面未注入），无法展示部件"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if len(data) > artifactWidgetDataMaxBytes {
		res.Error = fmt.Sprintf("data 初始数据 %d 字节超过上限 %d", len(data), artifactWidgetDataMaxBytes)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	name, enabled, ok := t.widgetLookup(widgetID)
	if !ok {
		res.Error = fmt.Sprintf("部件不存在: %s（先用 widget_save 创建，widget_list 可查已有部件）", widgetID)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if !enabled {
		res.Error = fmt.Sprintf("部件 %s 已停用，请先在界面部件页启用后再展示", widgetID)
		res.Category = ResultCategoryValidationRejected
		return res
	}

	res.Success = true
	res.Artifacts = []Artifact{{
		Kind:    "widget",
		Path:    "widget://" + widgetID,
		Title:   title,
		Caption: caption,
		Data:    data,
	}}
	label := title
	if label == "" {
		label = name
	}
	res.Output = fmt.Sprintf("已把界面部件 %s（%s）展示给用户（对话栏出现部件卡片，数据更新走 widget_data 推送通道）。不要在终答里重复贴路径，直接说明这是什么、要看哪里即可。",
		label, widgetID)
	return res
}


// workDirFor 取工作目录：ctx 注入值优先，缺失回落 Executor 配置目录。
func (t *showArtifactTool) workDirFor(ctx context.Context) string {
	if wd := WorkDirFromContext(ctx); wd != "" {
		return wd
	}
	if t.exec != nil {
		return t.exec.workDirOf(ctx)
	}
	return ""
}

// relToWorkDir 把绝对路径转成工作区相对路径（正斜杠）；不在工作区内时报错。
func (t *showArtifactTool) relToWorkDir(workDir, abs string) (string, error) {
	rootAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return filepath.ToSlash(rel), nil
}

// writeInlineHTML 把内联 HTML 落盘到 <工作目录>/.bma/artifacts/，返回绝对路径。
// 文件名 = 时间戳 + 内容 sha1 前 8 位：同内容重复展示复用同一文件，不同内容不互相覆盖。
func (t *showArtifactTool) writeInlineHTML(workDir, content string) (string, error) {
	dir := filepath.Join(workDir, ".bma", "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(content))
	name := fmt.Sprintf("%d-%s.html", time.Now().Unix(), hex.EncodeToString(sum[:4]))
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return abs, nil
}

// strArg 取字符串参数（类型不符返回空串）。
func strArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// sortedKeys 返回 map 键的稳定排序（错误文案可读且可断言）。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// kindLabelCN 展示类型的中文名（工具回执文案）。
func kindLabelCN(kind string) string {
	switch kind {
	case "image":
		return "图片"
	case "video":
		return "视频"
	case "audio":
		return "音频"
	case "html":
		return "网页预览"
	case "widget":
		return "界面部件"
	}
	return kind
}

// artifactLabel 回执里的成果称呼：标题优先，其次文件名。
func artifactLabel(kind, title, rel string) string {
	if title != "" {
		return title
	}
	return filepath.Base(rel)
}
