package bootstrap

// lifecycle.go 数据生命周期维护（TODO #18-2 T29）：
// 启动即跑一遍 + 每 24h 定时重复，覆盖三类清理——
//  1. 陈旧知识归档（global_knowledge.block_memory/external_kb，opt-in，默认关）
//  2. 日志文件按保留期删除（logs/<entry>/YYYY-MM-DD.log，默认 30 天）
//  3. 工具输出全文落盘按保留期删除（<workDir>/.bma/tool_outputs，默认 14 天）
//
// 全部 best-effort：单项失败记日志不中断；归档/清理属可再生产物维护，无强一致诉求。

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/logoutput"
)

// 数据保留期默认值（config.yaml data 段 0 值时的回落口径，语义见 config.DataConfig）。
const (
	defaultLogRetentionDays         = 30
	defaultToolOutputsRetentionDays = 14
	dataMaintenanceInterval         = 24 * time.Hour
)

// startDataMaintenance 启动数据生命周期维护循环，返回停止函数（幂等，可安全多次调用）。
// 启动时先同步跑一遍（此时无并发压力，清理失败也只影响磁盘占用），随后按 24h 周期重复。
// 参数 workDir：进程默认工作目录（.bma/tool_outputs 的清理根）；db：知识归档用存储，
// 归档开关关闭或 db 为 nil 时跳过归档步骤（测试/无库场景）；
// onDaily：随每轮额外触发的维护（如经验技能库整理 C），可为 nil；内部含轻量模型调用
// 等慢操作，放 goroutine 异步执行，不阻塞维护循环与启动路径。
func startDataMaintenance(cfg *config.Config, workDir string, db knowledgeArchiver, onDaily func()) (stop func()) {
	logDir := cfg.Logging.Dir
	if logDir == "" {
		logDir = "logs"
	}
	archiveDays := cfg.Data.KnowledgeArchiveDays
	logDays := cfg.Data.LogRetentionDays
	if logDays == 0 {
		logDays = defaultLogRetentionDays
	}
	toolOutDays := cfg.Data.ToolOutputsRetentionDays
	if toolOutDays == 0 {
		toolOutDays = defaultToolOutputsRetentionDays
	}

	run := func() {
		if db != nil {
			if n, err := db.ArchiveStale(context.Background(), archiveDays); err != nil {
				log.Printf("[data-maintenance] [WARN] 陈旧知识归档失败: %v", err)
			} else if n > 0 {
				log.Printf("[data-maintenance] 已归档 %d 条陈旧知识（阈值 %d 天）", n, archiveDays)
			}
		}
		if n, err := logoutput.CleanOldLogs(logDir, logDays); err != nil {
			log.Printf("[data-maintenance] [WARN] 日志清理部分失败: %v", err)
		} else if n > 0 {
			log.Printf("[data-maintenance] 已清理 %d 个过期日志文件（保留 %d 天）", n, logDays)
		}
		if n := cleanOldToolOutputs(workDir, toolOutDays); n > 0 {
			log.Printf("[data-maintenance] 已清理 %d 个过期工具输出文件（保留 %d 天）", n, toolOutDays)
		}
		// 附加日维护（如经验技能库整理）：慢操作异步执行，不阻塞清理循环。
		if onDaily != nil {
			go onDaily()
		}
	}

	run() // 启动即跑一遍

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(dataMaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(done)
	}
}

// knowledgeArchiver 数据维护所需的最小知识库能力（*store.KnowledgeStore 满足；
// 独立接口便于测试桩与 nil 安全）。
type knowledgeArchiver interface {
	ArchiveStale(ctx context.Context, olderThanDays int) (int64, error)
}

// cleanOldToolOutputs 清理工作目录下超过保留期的工具输出落盘文件
//（<workDir>/.bma/tool_outputs，react_agent 超限全文落盘产物）。
// days<=0 或目录不存在时 no-op；返回删除的文件数。
func cleanOldToolOutputs(workDir string, days int) int {
	if days <= 0 || workDir == "" {
		return 0
	}
	dir := filepath.Join(workDir, ".bma", "tool_outputs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // 目录不存在=还没落过盘，静默
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}
