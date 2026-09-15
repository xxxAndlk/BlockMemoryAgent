// clean.go 日志保留期清理（TODO #18-2 T29 数据生命周期）。
// 按文件 mtime 删除超过保留期的日志文件（logs/<entry>/YYYY-MM-DD.log），
// 顺带清掉删空后的入口子目录。retentionDays<=0 时 no-op（显式关闭语义）。
package logoutput

import (
	"os"
	"path/filepath"
	"time"
)

// CleanOldLogs 清理日志根目录下超过保留期的日志文件。
//
// 参数:
//   - rootDir: 日志根目录（与 NewWriter 的 dir 同源，如 ./logs）。
//   - retentionDays: 保留天数；<=0 直接返回 0（no-op，不碰任何文件）。
//
// 返回: 删除的文件数；目录不可读等错误按跳过处理，只返回最后一个错误。
func CleanOldLogs(rootDir string, retentionDays int) (int, error) {
	if retentionDays <= 0 || rootDir == "" {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	removed := 0
	var lastErr error
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 日志目录还没建过：无可清理
		}
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue // 根目录只放入口子目录（backend/tui/web）
		}
		sub := filepath.Join(rootDir, e.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			lastErr = err
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				lastErr = err
				continue
			}
			if info.ModTime().After(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(sub, f.Name())); err != nil {
				lastErr = err
				continue // 单文件失败（占用/权限）不中断整批
			}
			removed++
		}
		// 删空的入口子目录一并移除，防 logs/ 下堆积空壳目录。
		if rest, err := os.ReadDir(sub); err == nil && len(rest) == 0 {
			_ = os.Remove(sub)
		}
	}
	return removed, lastErr
}
