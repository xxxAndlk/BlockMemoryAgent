// Package logging is a thin facade over internal/logoutput.
//
// It preserves the existing Entry constants and an Init helper for callers that
// want a ready-to-use writer (optionally combined with os.Stderr). It no longer
// holds package-global state or mutates the standard log package's output.
package logging

import (
	"io"
	"os"

	"github.com/blockmemory/agent/backend/internal/logoutput"
)

// Entry identifies the log source entry point.
type Entry = logoutput.Entry

const (
	EntryBackend Entry = logoutput.EntryBackend
	EntryTUI     Entry = logoutput.EntryTUI
	EntryWeb     Entry = logoutput.EntryWeb
)

// Deprecated: use EntryBackend instead.
const EntryBrowser = EntryBackend

// Init creates a daily-rotating log writer for entry under dir.
//
// When silent is false the returned writer also forwards writes to os.Stderr,
// matching the previous HTTP-entry behavior. When silent is true (e.g. TUI
// alt-screen mode) only the file writer is returned.
//
// Callers own the returned writer and must Close it on shutdown. This function
// does not modify the global log package output.
func Init(entry Entry, dir string, silent bool) (io.WriteCloser, error) {
	w, err := logoutput.NewWriter(entry, dir)
	if err != nil {
		return nil, err
	}
	if silent {
		return w, nil
	}
	return &multiWriter{file: w, stderr: os.Stderr}, nil
}

// multiWriter writes to both stderr and the daily file. Closing only closes
// the file side; stderr is left open.
type multiWriter struct {
	file   io.WriteCloser
	stderr io.Writer
}

func (m *multiWriter) Write(p []byte) (int, error) {
	// Write to file first; on error still try stderr so logs are not lost.
	nFile, errFile := m.file.Write(p)
	nStderr, errStderr := m.stderr.Write(p)
	if errFile != nil {
		return nFile, errFile
	}
	if errStderr != nil {
		return nStderr, errStderr
	}
	if nFile != nStderr {
		// Should not happen for normal writers; report the smaller count.
		return nFile, nil
	}
	return nFile, nil
}

func (m *multiWriter) Close() error {
	return m.file.Close()
}
