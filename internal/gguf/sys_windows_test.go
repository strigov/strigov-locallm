//go:build windows

package gguf

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Канал с уникальным именем от t.TempDir не передаёт данных: чтение заблокировалось бы.
func TestReadInfoRejectsNamedPipeBeforeRead(t *testing.T) {
	dir := t.TempDir()
	path := `\\.\pipe\` + filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir) + "-secret-gguf"
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_OUTBOUND, windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 0, 0, nil)
	if err != nil {
		t.Fatalf("create synthetic pipe: %v", err)
	}
	result := make(chan error, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		windows.CloseHandle(h) // освобождает также заблокированное чтение при RED
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("ReadInfo did not finish after closing the pipe")
		}
	})
	start := time.Now()
	go func() {
		_, err := ReadInfo(context.Background(), path)
		result <- err
		close(done)
	}()
	select {
	case err := <-result:
		if err == nil || err.Error() != "not a regular disk file" || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret-gguf") {
			t.Fatalf("expected path-free non-regular file error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("rejection took %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadInfo blocked on a named pipe for 1 s")
	}
}
