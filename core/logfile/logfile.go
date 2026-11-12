// Package logfile keeps the hub's log on disk: one file that is appended to,
// and set aside under a numbered name once it grows past a size limit.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultMaxSize = 10 << 20
	defaultKeep    = 3
)

// Writer appends to the file at Path. When a write would take the file past
// MaxSize it is renamed to Path.1 (the previous Path.1 to Path.2, and so on)
// and a new file is started; Keep is how many of the old files stay.
type Writer struct {
	Path    string
	MaxSize int64
	Keep    int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open creates the file and its directory as needed and appends to it.
func Open(path string) (*Writer, error) {
	w := &Writer{Path: path, MaxSize: defaultMaxSize, Keep: defaultKeep}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		// A rotation failed half way; try again rather than lose the log
		// for the rest of the run.
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.MaxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *Writer) rotate() error {
	// Closed first: Windows does not rename a file that is open.
	w.f.Close()
	w.f = nil
	if w.Keep <= 0 {
		os.Remove(w.Path)
	} else {
		os.Remove(w.old(w.Keep))
		for i := w.Keep - 1; i >= 1; i-- {
			os.Rename(w.old(i), w.old(i+1))
		}
		os.Rename(w.Path, w.old(1))
	}
	return w.open()
}

func (w *Writer) old(i int) string { return fmt.Sprintf("%s.%d", w.Path, i) }

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
