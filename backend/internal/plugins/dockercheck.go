// dockercheck.go docker 依赖自检 + 残留清理（2026-10-28）。
//
// 背景：启动不再对 LLM 做连通性硬校验（模型可在应用内切换），改为校验"容器是否正确
// 运行"——docker 可用性、启用中的 docker 插件镜像是否就绪、bma 项目容器是否在跑。
// 同时回收历史残留：
//   - bma-plugin-* 容器按 (插件, 工作目录哈希) 命名（见 mcpbridge/docker.go），
//     换项目目录/进程被强杀/镜像重建后旧容器会残留（exited 或仍 running），
//     每次启动只增不减会占磁盘与内存；
//   - 镜像重建（同名 tag 重打）后旧镜像变成悬空镜像，只被残留容器引用，容器删掉后
//     变成 <none>:<none> 占空间。
//
// 清理口径（保守、幂等、best-effort）：
//   - 只删 **已停止** 且名字带 bma-plugin- 、带 compose 项目标签的容器（运行中的可能是
//     另一个 BMA 实例（tui + server 多开）正在用的 keeper，绝不动）；
//   - 只 prune **悬空镜像**（无 tag、无容器引用），不动任何带 tag 的镜像；
//   - 显式清理入口（/api/plugins/cleanup?all=true）才连运行中的 keeper 一起删。
package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
)

// dockerCLITimeout 单次 docker CLI 调用超时（Docker Desktop 卡死时不无限挂起启动）。
const dockerCLITimeout = 20 * time.Second

// dockerPruneTimeout 镜像 prune 超时（prune 走磁盘统计，比普通查询慢）。
const dockerPruneTimeout = 90 * time.Second

// DockerImageStatus 启用中的 docker 插件镜像就绪状态。
type DockerImageStatus struct {
	Plugin  string `json:"plugin"`  // 插件 ID（ui_design 等）
	Image   string `json:"image"`   // plugins.yaml settings.image
	Present bool   `json:"present"` // 本地镜像是否存在（缺失时插件启动会一直失败）
}

// DockerContainerStatus bma 项目容器状态（含 compose 长驻服务与插件 keeper）。
type DockerContainerStatus struct {
	Name    string `json:"name"`
	Service string `json:"service,omitempty"` // com.docker.compose.service 标签
	State   string `json:"state"`             // running | exited | created | dead | ...
	Status  string `json:"status,omitempty"`  // docker ps 的 Status 原文（含 health 标记）
	Image   string `json:"image,omitempty"`
	// WorkDirs 容器挂载的宿主侧源路径（keeper 容器据此看"属于哪个工作目录"）。
	WorkDirs []string `json:"workdirs,omitempty"`
	// PluginKeeper 是否由后端插件系统创建的 bma-plugin-* 容器。
	PluginKeeper bool `json:"plugin_keeper,omitempty"`
	// Stale 运行中的 keeper 实际镜像已落后于插件配置的 tag（镜像重建后没换容器，
	// 会继续跑旧代码/旧 env）；下一次插件调用会重建，启动期清理也会回收。
	Stale bool `json:"stale,omitempty"`
}

// DockerReport docker 依赖自检结果（启动日志 + GET /api/capabilities 共用）。
type DockerReport struct {
	Available bool   `json:"available"`         // docker CLI 可用且 daemon 可达
	Version   string `json:"version,omitempty"` // docker server 版本
	Error     string `json:"error,omitempty"`   // 不可用原因
	// Required 是否有启用中的 docker 插件（没有时 docker 不可用不算是问题）。
	Required   bool                    `json:"required"`
	Images     []DockerImageStatus     `json:"images,omitempty"`     // 启用插件所需镜像
	Containers []DockerContainerStatus `json:"containers,omitempty"` // bma 项目容器
	// MissingImages 缺失的镜像（插件启动会失败）。
	MissingImages []string `json:"missing_images,omitempty"`
	// StoppedContainers 已停止的 bma-plugin-* 残留容器名（下次启动会被自动回收）。
	StoppedContainers []string `json:"stopped_containers,omitempty"`
	// StaleContainers 运行中但镜像与配置 tag 不一致的 keeper（镜像重建后未换容器，
	// 会继续跑旧代码）；启动清理会回收，下次插件调用按新镜像重建。
	StaleContainers []string `json:"stale_containers,omitempty"`
}

// Healthy 报告是否通过：有 docker 依赖时必须 docker 可用 + 启用插件镜像齐全 +
// 无停止/过期的插件容器；无 docker 依赖时 docker 缺失不算失败（纯 http/stdio 部署）。
// 容器为空不算失败（插件预热是异步的，首次启动可能还没拉起）。
func (r DockerReport) Healthy() bool {
	if !r.Available {
		return !r.Required
	}
	return len(r.MissingImages) == 0 && len(r.StoppedContainers) == 0 && len(r.StaleContainers) == 0
}

// Summary 生成一行人类可读摘要（启动日志/能力自检共用）。
func (r DockerReport) Summary() string {
	if !r.Available {
		if r.Required {
			return "docker 不可用: " + r.Error
		}
		return "docker 不可用（当前无启用中的 docker 插件，不影响使用）: " + r.Error
	}
	var parts []string
	parts = append(parts, "docker "+r.Version)
	running, stopped := 0, 0
	for _, c := range r.Containers {
		if c.State == "running" {
			running++
		} else {
			stopped++
		}
	}
	parts = append(parts, fmt.Sprintf("bma 容器 running=%d stopped=%d", running, stopped))
	if len(r.MissingImages) > 0 {
		parts = append(parts, "缺镜像: "+strings.Join(r.MissingImages, ", "))
	}
	if len(r.StoppedContainers) > 0 {
		parts = append(parts, "残留停止容器: "+strings.Join(r.StoppedContainers, ", "))
	}
	if len(r.StaleContainers) > 0 {
		parts = append(parts, "过期 keeper(镜像已更新): "+strings.Join(r.StaleContainers, ", "))
	}
	return strings.Join(parts, "；")
}

// DockerReport 采集 docker 依赖健康状态（只读，不发任何写操作）。
//
// 检查项：
//  1. docker CLI + daemon 可达（docker version --format '{{.Server.Version}}'）；
//  2. 启用中的 docker transport 插件的镜像是否存在于本地；
//  3. 带 compose 项目标签 bma 的全部容器（compose 长驻服务 + 插件 keeper）状态。
func (m *Manager) DockerReport(ctx context.Context) DockerReport {
	report := DockerReport{}
	// 先算依赖：无启用 docker 插件时，docker 不可用不算问题（纯 http/stdio 部署）。
	pluginImages := m.dockerPluginImages()
	report.Required = len(pluginImages) > 0
	version, err := dockerServerVersion(ctx)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Available = true
	report.Version = version

	// 1) 启用中的 docker 插件镜像（去重；同一镜像多个插件只报一条，Plugin 取首个）。
	seenImage := map[string]bool{}
	pluginIDs := make([]string, 0, len(pluginImages))
	for id := range pluginImages {
		pluginIDs = append(pluginIDs, id)
	}
	sort.Strings(pluginIDs)
	for _, id := range pluginIDs {
		image := pluginImages[id]
		if seenImage[image] {
			continue
		}
		seenImage[image] = true
		present := dockerImageExists(ctx, image)
		report.Images = append(report.Images, DockerImageStatus{Plugin: id, Image: image, Present: present})
		if !present {
			report.MissingImages = append(report.MissingImages, image)
		}
	}
	sort.Strings(report.MissingImages)

	// 2) bma 项目容器（含 compose 服务与插件 keeper）。
	containers, err := dockerListBMAContainers(ctx)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	tagIDs := map[string]string{} // 插件 id → 配置 tag 当前镜像 ID（判过期用）
	for id, image := range pluginImages {
		if tagID := dockerTagImageID(ctx, image); tagID != "" {
			tagIDs[id] = tagID
		}
	}
	for i := range containers {
		c := &containers[i]
		if !c.PluginKeeper {
			continue
		}
		// keeper 的挂载源 = 它服务的工作目录（帮助判断"这堆容器属于哪些目录"）。
		c.WorkDirs = dockerMountSources(ctx, c.Name)
		if c.State != "running" {
			report.StoppedContainers = append(report.StoppedContainers, c.Name)
			continue
		}
		if wantID := tagIDs[c.Service]; wantID != "" {
			if got := dockerContainerImageID(ctx, c.Name); got != wantID {
				c.Stale = true
				report.StaleContainers = append(report.StaleContainers, c.Name)
			}
		}
	}
	sort.Strings(report.StoppedContainers)
	sort.Strings(report.StaleContainers)
	report.Containers = containers
	return report
}

// DockerCleanupResult 清理结果（启动日志 / API 返回共用）。
type DockerCleanupResult struct {
	RemovedContainers []string `json:"removed_containers,omitempty"` // 已删除的容器名
	PrunedImages      int      `json:"pruned_images,omitempty"`      // 删除的悬空镜像数
	Reclaimed         string   `json:"reclaimed,omitempty"`          // docker 报告的回收空间
	Errors            []string `json:"errors,omitempty"`             // 单项失败原因（不影响其他项）
}

// CleanupDocker 清理 docker 残留（best-effort；单项失败不影响其他项）。
//
// includeRunning=false（默认，启动期调用）：删已停止的 bma-plugin-* 容器，外加
// **工作目录已被删除**的运行中 keeper（宿主机挂载源 os.Stat 不存在 = 容器已无法复用，
// 典型场景是删掉旧项目文件夹/改了默认工作目录）；正常在用的 keeper（可能属于另一个
// BMA 实例）不动。
// includeRunning=true（显式清理）：连正常运行中的 keeper 一起删——会掐断其他 BMA
// 实例的在途 MCP 会话（下次调用自动重建），仅限用户主动触发（换镜像/释放内存）。
//
// 结束后对悬空镜像做一次 prune（无 tag、无容器引用），把旧 keeper 引用的旧镜像清掉。
func (m *Manager) CleanupDocker(ctx context.Context, includeRunning bool) DockerCleanupResult {
	res := DockerCleanupResult{}
	// 无 docker（没装/daemon 没起）：直接返回，不自造错误噪声（自检报告会单独提示）。
	if _, err := dockerServerVersion(ctx); err != nil {
		return res
	}
	names := m.listPluginContainers(ctx, includeRunning)
	if !includeRunning {
		// 额外回收：挂载源已不存在的运行中 keeper（无法再服务任何会话）+ 镜像已落后于
		// 配置 tag 的过期 keeper（下次调用必然按新镜像重建，留着只会跑旧代码占内存）。
		names = append(names, m.listOrphanKeepers(ctx)...)
		if stale, err := m.listStaleKeepers(ctx); err == nil {
			names = append(names, stale...)
		} else {
			res.Errors = append(res.Errors, fmt.Sprintf("list stale keepers: %v", err))
		}
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue // 孤儿与过期可能同时命中同一个容器
		}
		seen[name] = true
		if err := dockerRemoveContainer(ctx, name); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("rm %s: %v", name, err))
			continue
		}
		res.RemovedContainers = append(res.RemovedContainers, name)
	}
	res.PrunedImages, res.Reclaimed = pruneDanglingImages(ctx, &res)
	return res
}

// listPluginContainers 列出 bma-plugin-* 容器名（includeRunning=false 时只要已停止的）。
// 双重过滤（名字前缀 + compose 项目标签）防误删用户自建的同前缀容器。
func (m *Manager) listPluginContainers(ctx context.Context, includeRunning bool) []string {
	args := []string{"ps", "-a", "--format", "{{.Names}}\t{{.State}}",
		"--filter", "name=bma-plugin-", "--filter", "label=com.docker.compose.project=bma"}
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", args...).Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(fields) != 2 || fields[0] == "" {
			continue
		}
		if !includeRunning && fields[1] == "running" {
			continue
		}
		names = append(names, fields[0])
	}
	sort.Strings(names)
	return names
}

// listOrphanKeepers 返回挂载源已不存在的**运行中** keeper 容器名。
//
// keeper 的 /workspace 绑定会话工作目录；目录被删除/改名后容器仍会常驻运行（docker
// 允许已删目录的 bind 挂载），但它已无法服务任何会话——属于纯占资源。只删这种；
// 有活目录的一律保留（可能被另一个 BMA 实例使用）。
func (m *Manager) listOrphanKeepers(ctx context.Context) []string {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "ps",
		"--format", "{{.Names}}",
		"--filter", "name=bma-plugin-",
		"--filter", "label=com.docker.compose.project=bma").Output()
	if err != nil {
		return nil
	}
	var orphans []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		srcs := dockerMountSources(ctx, name)
		if len(srcs) == 0 {
			// 无绑定挂载（如 web_search 类全局 keeper）：不存在孤儿概念。
			continue
		}
		alive := false
		for _, src := range srcs {
			if _, statErr := os.Stat(src); statErr == nil {
				alive = true
				break
			}
		}
		if !alive {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	return orphans
}

// listStaleKeepers 返回运行中、但镜像已落后于插件配置 tag 的 keeper 容器名。
//
// 判定口径 = 容器实际镜像 ID ≠ 配置 tag 当前镜像 ID（镜像重建后没换容器，会一直
// 跑旧代码/旧 env）；容器引用的镜像被删时同样命中。这类容器对新会话没有任何价值
// ——下次调用必然 `docker rm -f` + 重建——因此在启动期回收是安全的。
//
// 只判定**本进程配置里启用中**的插件：另一个 BMA 实例（不同 plugins.yaml / 不同
// 工作目录）的 keeper 不在此列，避免误删。
func (m *Manager) listStaleKeepers(ctx context.Context) ([]string, error) {
	containers, err := dockerListBMAContainers(ctx)
	if err != nil {
		return nil, err
	}
	pluginImages := m.dockerPluginImages()
	if len(pluginImages) == 0 {
		return nil, nil
	}
	tagIDs := map[string]string{} // 插件 id → 配置 tag 当前镜像 ID
	var names []string
	for _, c := range containers {
		if !c.PluginKeeper || c.State != "running" || c.Service == "" {
			continue
		}
		image, ok := pluginImages[c.Service]
		if !ok {
			continue // 不是本进程当前启用的插件：不判定
		}
		wantID, ok := tagIDs[c.Service]
		if !ok {
			if wantID = dockerTagImageID(ctx, image); wantID == "" {
				continue // 本地没有该 tag：无从比对，保守保留
			}
			tagIDs[c.Service] = wantID
		}
		if dockerContainerImageID(ctx, c.Name) != wantID {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// dockerPluginImages 返回启用中的 docker 插件 id → 配置镜像 tag（无 docker 插件时为空）。
//
// 优先用实例表；**启动期 CleanupDocker 在 Load 之前执行**，此时实例表还没建，回落到
// 磁盘上的 plugins.yaml（与 Load 同一解析口径，env 插值一致），否则启动清理永远
// 判定不出"过期 keeper"。
func (m *Manager) dockerPluginImages() map[string]string {
	out := map[string]string{}
	m.mu.RLock()
	for id, inst := range m.instances {
		if inst == nil || !inst.config.Enabled() {
			continue
		}
		if img := dockerImageOf(inst.config.Settings); img != "" {
			out[id] = img
		}
	}
	configDir, cfg := m.configDir, m.cfg
	m.mu.RUnlock()
	if len(out) > 0 {
		return out
	}
	if configDir != "" {
		if c, err := config.LoadPluginsConfig(configPath(configDir)); err == nil && c != nil {
			cfg = c
		}
	}
	if cfg == nil {
		return out
	}
	for id, pc := range cfg.Plugins {
		if !pc.Enabled() {
			continue
		}
		if img := dockerImageOf(pc.Settings); img != "" {
			out[id] = img
		}
	}
	return out
}

// dockerImageOf 从插件 settings 里取 docker 镜像；非 docker transport 返回空。
// 注意：plugins 包不能 import mcpbridge（mcpbridge import plugins，会成环），
// 这里只读两个原始键（与 mcpbridge.FromSettings 的解析保持一致）。
func dockerImageOf(settings map[string]any) string {
	if settings == nil {
		return ""
	}
	transport, _ := settings["transport"].(string)
	if transport != "docker" {
		return ""
	}
	image, _ := settings["image"].(string)
	return image
}

// dockerServerVersion 探测 docker CLI 与 daemon（失败时返回可行动的错误文案）。
func dockerServerVersion(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker 不可达（Docker Desktop 未启动？）: %s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerImageExists 判断本地是否已有该镜像标签。
func dockerImageExists(ctx context.Context, image string) bool {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	return exec.CommandContext(cctx, "docker", "image", "inspect", image).Run() == nil
}

// dockerListBMAContainers 列出带 compose 项目标签 bma 的全部容器及其状态。
func dockerListBMAContainers(ctx context.Context) ([]DockerContainerStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project=bma",
		"--format", "{{json .}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var list []DockerContainerStatus
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if c, ok := parseDockerPSContainer(line); ok {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// parseDockerPSContainer 解析 `docker ps --format '{{json .}}'` 的单行输出。
// 非法行（空行/非 JSON）返回 ok=false。抽成纯函数便于单测（CI 无 docker）。
func parseDockerPSContainer(line string) (DockerContainerStatus, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return DockerContainerStatus{}, false
	}
	var raw struct {
		Names  string `json:"Names"`
		Image  string `json:"Image"`
		State  string `json:"State"`
		Status string `json:"Status"`
		Labels string `json:"Labels"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return DockerContainerStatus{}, false
	}
	// docker ps 的 Labels 是 "k=v,k=v" 逗号拼接串。
	service := ""
	for _, kv := range strings.Split(raw.Labels, ",") {
		if v, ok := strings.CutPrefix(kv, "com.docker.compose.service="); ok {
			service = v
		}
	}
	name := strings.SplitN(raw.Names, ",", 2)[0]
	return DockerContainerStatus{
		Name:         name,
		Service:      service,
		State:        raw.State,
		Status:       raw.Status,
		Image:        raw.Image,
		PluginKeeper: strings.HasPrefix(name, "bma-plugin-"),
	}, true
}

// dockerMountSources 返回容器 **bind 挂载** 的宿主侧源路径（best-effort，失败返回 nil）。
// 只取 Type=bind：命名卷的 Source 是宿主上的 docker 内部路径（Linux 虚拟机内），
// 在本机 os.Stat 不到，会把正常容器误判成孤儿。
func dockerMountSources(ctx context.Context, name string) []string {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "inspect", "-f",
		`{{range .Mounts}}{{.Type}}|{{.Source}}|{{.Destination}}{{"\n"}}{{end}}`, name).Output()
	if err != nil {
		return nil
	}
	var srcs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) != "bind" {
			continue
		}
		if s := strings.TrimSpace(parts[1]); s != "" {
			srcs = append(srcs, s)
		}
	}
	sort.Strings(srcs)
	return srcs
}

// dockerRemoveContainer 删除单个容器（含运行中的：docker rm -f）。
func dockerRemoveContainer(ctx context.Context, name string) error {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "rm", "-f", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pruneDanglingImages 删除悬空镜像（无 tag、且没有任何容器引用），返回删除数量与
// 回收空间文案。失败只记入 res.Errors，不影响清理主流程。
//
// 不用 `docker image prune -f`：containerd 镜像存储下它会把**仍被容器引用**的
// untagged 镜像一并删掉（2026-10-30 实测：两个运行中 keeper 的镜像被清成不存在的
// ID，容器变成"幽灵"）。这里显式取"全部容器（含已停止）引用的镜像 ID"求差集，
// 只 rmi 无人引用的那些；带 tag 的镜像一律不动。
func pruneDanglingImages(ctx context.Context, res *DockerCleanupResult) (int, string) {
	cctx, cancel := context.WithTimeout(ctx, dockerPruneTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "images", "-q", "-f", "dangling=true").Output()
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("list dangling images: %v", err))
		return 0, ""
	}
	ids := splitOutputLines(string(out))
	if len(ids) == 0 {
		return 0, ""
	}
	inUse := dockerReferencedImageIDs(cctx)
	var (
		removed int
		freed   int64
	)
	for _, id := range ids {
		if imageIDInUse(inUse, id) {
			continue // 仍被容器引用：删掉会让容器变成引用不存在镜像的幽灵
		}
		size, _ := dockerImageSize(cctx, id)
		rmOut, rmErr := exec.CommandContext(cctx, "docker", "rmi", id).CombinedOutput()
		if rmErr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("rmi %s: %v (%s)", shortDockerID(id), rmErr, strings.TrimSpace(string(rmOut))))
			continue
		}
		removed++
		freed += size
	}
	if removed == 0 {
		return 0, ""
	}
	return removed, humanBytes(freed)
}

// dockerReferencedImageIDs 返回所有容器（运行中 + 已停止）引用的镜像 ID 集合。
// 已停止容器也保留其镜像：用户可能还要 docker start 回来，属"仍在使用"。
func dockerReferencedImageIDs(ctx context.Context) map[string]bool {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq").Output()
	if err != nil {
		return nil
	}
	containers := splitOutputLines(string(out))
	if len(containers) == 0 {
		return nil
	}
	args := append([]string{"inspect", "-f", "{{.Image}}"}, containers...)
	inspect, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, id := range splitOutputLines(string(inspect)) {
		set[id] = true
	}
	return set
}

// imageIDInUse 判断镜像（可能是短 ID）是否在引用集合中（集合里是完整 sha256 ID）。
func imageIDInUse(set map[string]bool, id string) bool {
	if len(set) == 0 {
		return false
	}
	if set[id] {
		return true
	}
	short := strings.TrimPrefix(id, "sha256:")
	for full := range set {
		if strings.HasPrefix(strings.TrimPrefix(full, "sha256:"), short) {
			return true
		}
	}
	return false
}

// dockerImageSize 返回镜像大小（字节）；失败返回 0。
func dockerImageSize(ctx context.Context, id string) (int64, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "-f", "{{.Size}}", id).Output()
	if err != nil {
		return 0, err
	}
	var size int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &size); err != nil {
		return 0, err
	}
	return size, nil
}

// dockerTagImageID 返回本地 tag 当前指向的镜像 ID（tag 不存在/失败返回空）。
func dockerTagImageID(ctx context.Context, image string) string {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "image", "inspect", "-f", "{{.Id}}", image).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dockerContainerImageID 返回容器实际使用的镜像 ID（失败返回空）。
// 镜像被删后仍返回容器创建时记录的 ID，可与 tag 当前 ID 比对识别过期 keeper。
func dockerContainerImageID(ctx context.Context, name string) string {
	cctx, cancel := context.WithTimeout(ctx, dockerCLITimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "docker", "inspect", "-f", "{{.Image}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// splitOutputLines 按行切分 docker CLI 输出（去空行/空白）。
func splitOutputLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			lines = append(lines, s)
		}
	}
	return lines
}

// shortDockerID 截短 sha256 ID 便于日志阅读。
func shortDockerID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// humanBytes 把字节数格式化成人类可读文案（1.2GB / 431.2MB / 0B）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
