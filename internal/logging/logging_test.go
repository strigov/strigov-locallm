package logging

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestRotationKeepsMaxFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "host.log")
	rf, err := NewRotatingFile(p, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	for i := 0; i < 10; i++ {
		line := strings.Repeat(fmt.Sprint(i), 39) + "\n"
		if _, err := rf.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"host.log", "host.1.log", "host.2.log"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if st.Size() > 100 {
			t.Fatalf("%s size %d > 100", name, st.Size())
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		first := map[string]int{"host.log": 8, "host.1.log": 6, "host.2.log": 4}[name]
		want := strings.Repeat(fmt.Sprint(first), 39) + "\n" + strings.Repeat(fmt.Sprint(first+1), 39) + "\n"
		if string(data) != want {
			t.Fatalf("%s: wrong retained records: %q", name, data)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "host.3.log")); !os.IsNotExist(err) {
		t.Fatal("host.3.log must not exist with max_files=3")
	}
}

func TestRotationMaxFilesOne(t *testing.T) {
	dir := t.TempDir()
	rf, err := NewRotatingFile(filepath.Join(dir, "host.log"), 50, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	for i := 0; i < 5; i++ {
		if _, err := rf.Write([]byte(strings.Repeat("y", 30) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want only host.log, got %d files", len(entries))
	}
}

func TestWriteIsUnbuffered(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.log")
	rf, err := NewRotatingFile(p, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("content before Close: %q", b)
	}
}

func TestOpenTextFormatAndLevel(t *testing.T) {
	dir := t.TempDir()
	log, closer, err := Open(dir, "warn", 1, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "k", "v")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "level=WARN msg=shown k=v") || strings.Contains(s, "hidden") {
		t.Fatalf("log: %q", s)
	}
}

func TestParseLevel(t *testing.T) {
	for name, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := ParseLevel(name); err != nil || got != want {
			t.Fatalf("%s: level=%v err=%v", name, got, err)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("verbose must be rejected")
	}
}

func TestRotationUsesExistingSizeAndExactLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.log")
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := NewRotatingFile(p, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Write([]byte("67890")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "host.1.log")); !os.IsNotExist(err) {
		t.Fatal("writing exactly the limit must not rotate")
	}
	if _, err := rf.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"host.log": "next", "host.1.log": "1234567890"} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(p), name))
		if err != nil || string(data) != want {
			t.Fatalf("%s: got %q err=%v", name, data, err)
		}
	}
}

func TestCloseIsIdempotentAndWriteAfterCloseFails(t *testing.T) {
	rf, err := NewRotatingFile(filepath.Join(t.TempDir(), "host.log"), 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := rf.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := rf.Write([]byte("closed")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close: n=%d err=%v", n, err)
	}
}

func TestConcurrentWritesKeepWholeRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "host.log")
	rf, err := NewRotatingFile(p, 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Go(func() {
			for row := 0; row < 25; row++ {
				if _, err := fmt.Fprintf(rf, "%d:%d\n", worker, row); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		counts[line]++
	}
	for worker := 0; worker < 8; worker++ {
		for row := 0; row < 25; row++ {
			if line := fmt.Sprintf("%d:%d", worker, row); counts[line] != 1 {
				t.Fatalf("record %q count=%d", line, counts[line])
			}
		}
	}
	if len(counts) != 200 {
		t.Fatalf("unexpected records: got %d, want 200", len(counts))
	}
}

func TestOpenAlsoStderr(t *testing.T) {
	dir := t.TempDir()
	capture, err := os.Create(filepath.Join(dir, "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	previous := os.Stderr
	os.Stderr = capture
	t.Cleanup(func() { os.Stderr = previous })
	log, closer, err := Open(filepath.Join(dir, "logs"), "info", 1, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("duplicated", "count", 2)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(filepath.Join(dir, "logs", "host.log"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(capture.Name())
	if err != nil || string(file) != string(stderr) || !strings.Contains(string(file), "msg=duplicated count=2") {
		t.Fatalf("stderr duplication: file=%q stderr=%q err=%v", file, stderr, err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if log, closer, err := Open(dir, "verbose", 1, 2, false); err == nil || log != nil || closer != nil {
		t.Fatalf("invalid level: logger=%v closer=%v err=%v", log, closer, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid level must not create a log directory")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(file, "info", 1, 2, false); err == nil {
		t.Fatal("a file cannot be used as a log directory")
	}
}

func TestFallbackPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	if got, want := FallbackPath(), filepath.Join(dir, "locallm", "logs", "host.log"); got != want {
		t.Fatalf("fallback path=%q want=%q", got, want)
	}
}

func TestRotationFailurePreservesFilesAndRetries(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows file sharing semantics")
	}
	for _, tc := range []struct {
		name, held string
		maxFiles   int
	}{
		{"active file locked", "host.log", 3},
		{"generation locked after active rename", "host.1.log", 3},
		{"single file locked", "host.log", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := map[string]string{"host.log": "start"}
			if tc.maxFiles > 1 {
				before["host.1.log"], before["host.2.log"] = "older1", "older2"
			}
			for name, data := range before {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rf, err := NewRotatingFile(filepath.Join(dir, "host.log"), 5, tc.maxFiles)
			if err != nil {
				t.Fatal(err)
			}
			defer rf.Close()
			held, err := os.Open(filepath.Join(dir, tc.held))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			for _, data := range []string{"A", "B"} {
				if n, err := rf.Write([]byte(data)); err != nil || n != len(data) {
					t.Errorf("write must append despite failed rotation: n=%d err=%v", n, err)
				}
			}
			before["host.log"] = "startAB"
			for name, want := range before {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != want {
					t.Errorf("failed rotation changed %s: data=%q err=%v", name, data, err)
				}
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			if n, err := rf.Write([]byte("C")); err != nil || n != 1 {
				t.Fatalf("rotation must recover after handle closes: n=%d err=%v", n, err)
			}
			want := map[string]string{"host.log": "C"}
			if tc.maxFiles > 1 {
				want["host.1.log"], want["host.2.log"] = "startAB", "older1"
			}
			for name, expected := range want {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != expected {
					t.Errorf("recovered rotation %s: data=%q err=%v", name, data, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != tc.maxFiles {
				t.Fatalf("rotation must clean its temporary directory: entries=%d err=%v", len(entries), err)
			}
		})
	}
}

func TestRotationPreservesCrashLeftovers(t *testing.T) {
	dir := t.TempDir()
	leftover, err := os.MkdirTemp(dir, ".host.log.rotate-")
	if err != nil {
		t.Fatal(err)
	}
	recovered := filepath.Join(leftover, "active.log")
	if err := os.WriteFile(recovered, []byte("uncommitted log data"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "host.log")
	rf, err := NewRotatingFile(p, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	for _, data := range []string{"first", "next", "last"} {
		if _, err := rf.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{recovered: "uncommitted log data", p: "last", filepath.Join(dir, "host.1.log"): "next"} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Errorf("log data changed at %s: data=%q err=%v", name, data, err)
		}
	}
}

func TestRotationCloseFailureRecovers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host.log")
	rf, err := NewRotatingFile(p, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	// Force the Close inside rotation to fail, without filesystem permissions or timing races.
	if err := rf.f.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := rf.Write([]byte("A")); n != 1 || err != nil {
		t.Fatalf("write after a rotation Close failure: n=%d err=%v", n, err)
	}
	if _, err := rf.Write([]byte("B")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{p: "B", filepath.Join(filepath.Dir(p), "host.1.log"): "firstA"} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Fatalf("recovered Close failure %s: data=%q err=%v", name, data, err)
		}
	}
}
