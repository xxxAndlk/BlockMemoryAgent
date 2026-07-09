// Package logoutput provides a rotation-aware log writer.
//
// It knows nothing about structured logging or the standard log package; it
// only produces an io.WriteCloser that writes to a file which is rotated by
// day and organized per entry (backend/tui/web). Callers decide whether to
// combine it with os.Stderr, slog, or any other writer.
package logoutput

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry identifies the source entry point for log file organization.
type Entry string

const (
	EntryBackend Entry = "backend" // HTTP backend service entry
	EntryTUI     Entry = "tui"     // terminal TUI entry
	EntryWeb     Entry = "web"     // web frontend entry
)

// Deprecated: use EntryBackend instead.
const EntryBrowser = EntryBackend

// dailyWriter rotates log files by day. It is safe for concurrent use.
type dailyWriter struct {
	mu      sync.Mutex
	dir     string
	entry   Entry
	curDate string
	curFile *os.File
}

// Write implements io.Writer. It opens the log file for today on first write
// and rolls over to a new file when the date changes.
func (w *dailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	today := time.Now().Format("2006-01-02")
	if w.curFile == nil || w.curDate != today {
		if w.curFile != nil {
			// Best-effort close of the previous day's file.
			_ = w.curFile.Close()
		}
		subDir := filepath.Join(w.dir, string(w.entry))
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			return 0, fmt.Errorf("create log dir: %w", err)
		}
		path := filepath.Join(subDir, fmt.Sprintf("%s.log", today))
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return 0, fmt.Errorf("open log file: %w", err)
		}
		w.curFile = f
		w.curDate = today
	}
	return w.curFile.Write(p)
}

// Close implements io.WriteCloser.
func (w *dailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curFile != nil {
		err := w.curFile.Close()
		w.curFile = nil
		return err
	}
	return nil
}

// NewWriter creates a daily-rotating log writer for the given entry.
// The returned writer writes to logs/<entry>/YYYY-MM-DD.log under dir.
// An empty dir defaults to "logs".
func NewWriter(entry Entry, dir string) (io.WriteCloser, error) {
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &dailyWriter{dir: dir, entry: entry}
	// Trigger file creation early so configuration errors surface at init time.
	if _, err := w.Write([]byte("")); err != nil {
		return nil, err
	}
	return w, nil
}
