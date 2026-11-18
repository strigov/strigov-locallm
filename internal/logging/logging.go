// Package logging — log/slog в файл с ротацией по размеру (§13).
package logging

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FallbackPath — журнал до открытия основного (§4.7): %LOCALAPPDATA%\locallm\logs\host.log.
func FallbackPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "locallm", "logs", "host.log")
}

func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// Open создаёт <dir>\host.log (каталог создаётся) и TextHandler поверх него; alsoStderr дублирует в stderr.
func Open(dir, level string, maxSizeMiB, maxFiles int, alsoStderr bool) (*slog.Logger, io.Closer, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, nil, err
	}
	rf, err := NewRotatingFile(filepath.Join(dir, "host.log"), int64(maxSizeMiB)<<20, maxFiles)
	if err != nil {
		return nil, nil, err
	}
	var w io.Writer = rf
	if alsoStderr {
		w = io.MultiWriter(rf, os.Stderr)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), rf, nil
}

// RotatingFile — файл без буферизации; перед записью, которая превысит maxBytes,
// сдвигает host.log → host.1.log → … → host.<maxFiles-1>.log, самый старый удаляет.
type RotatingFile struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	maxFiles    int
	f           *os.File
	size        int64
	needsReopen bool // closed by rotation; only Close sets f to nil
}

func NewRotatingFile(path string, maxBytes int64, maxFiles int) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, maxFiles: maxFiles, f: f, size: st.Size()}, nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.needsReopen {
		if err := r.reopen(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil && r.needsReopen {
			return 0, err
		}
		// A recovered rotation failure must not drop this record. Retry on the next Write.
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func (r *RotatingFile) name(i int) string {
	ext := filepath.Ext(r.path)
	return strings.TrimSuffix(r.path, ext) + "." + strconv.Itoa(i) + ext
}

func (r *RotatingFile) reopen() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	r.f, r.size, r.needsReopen = f, st.Size(), false
	return nil
}

func (r *RotatingFile) rotate() (err error) {
	// Unique names preserve crash leftovers without blocking or overwriting them.
	tmp, err := os.MkdirTemp(filepath.Dir(r.path), "."+filepath.Base(r.path)+".rotate-")
	if err != nil {
		return err
	}
	// Remove only an empty directory; never delete a leftover log on an error.
	defer os.Remove(tmp)
	type movedFile struct{ from, to string }
	var moved []movedFile
	move := func(from, to string, optional bool) error {
		if err := os.Rename(from, to); err != nil {
			if optional && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		moved = append(moved, movedFile{from, to})
		return nil
	}
	layoutReady := false
	defer func() {
		if err != nil && !layoutReady {
			for i := len(moved) - 1; i >= 0; i-- {
				m := moved[i]
				if _, checkErr := os.Lstat(m.from); !errors.Is(checkErr, fs.ErrNotExist) {
					// Another writer may have created a file here. Preserve both files.
					err = errors.Join(err, fmt.Errorf("cannot restore log %s: destination is not free", m.from), checkErr)
					continue
				}
				err = errors.Join(err, os.Rename(m.to, m.from))
			}
		}
		if r.needsReopen {
			err = errors.Join(err, r.reopen())
		}
	}()
	r.needsReopen = true
	if err = r.f.Close(); err != nil {
		return err
	}
	active := filepath.Join(tmp, "active.log")
	if err = move(r.path, active, false); err != nil {
		return err
	}
	discard := active
	if r.maxFiles > 1 {
		// Keep the oldest generation until all renames and the new open succeed.
		discard = filepath.Join(tmp, "oldest.log")
		if err = move(r.name(r.maxFiles-1), discard, true); err != nil {
			return err
		}
		for i := r.maxFiles - 2; i >= 1; i-- {
			if err = move(r.name(i), r.name(i+1), true); err != nil {
				return err
			}
		}
		if err = move(active, r.name(1), false); err != nil {
			return err
		}
	}
	if err = r.reopen(); err != nil {
		return err
	}
	layoutReady = true
	if err = os.Remove(discard); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return err
}
