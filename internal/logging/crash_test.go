package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCrashFileCreatesDirAndAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	p := filepath.Join(dir, "crash.log")
	for _, s := range []string{"one\n", "two\n"} {
		f, err := OpenCrashFile(dir, 100)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if got, _ := os.ReadFile(p); string(got) != "one\ntwo\n" {
		t.Fatalf("crash.log = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "crash.1.log")); !os.IsNotExist(err) {
		t.Fatalf("crash.1.log must not exist: %v", err)
	}
}

func TestOpenCrashFileKeepsOneOldFile(t *testing.T) {
	dir := t.TempDir()
	p, old := filepath.Join(dir, "crash.log"), filepath.Join(dir, "crash.1.log")
	big := bytes.Repeat([]byte("a"), 101)
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenCrashFile(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, _ := os.ReadFile(old); !bytes.Equal(got, big) {
		t.Fatalf("crash.1.log must hold the previous crash.log, got %d bytes", len(got))
	}
	if st, err := os.Stat(p); err != nil || st.Size() != 0 {
		t.Fatalf("crash.log must be empty after rotation: %v", err)
	}
	// Ровно на пределе — без сдвига.
	if err := os.WriteFile(p, big[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = OpenCrashFile(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, _ := os.Stat(p); st.Size() != 100 {
		t.Fatalf("crash.log at the limit must stay, size %d", st.Size())
	}
}
