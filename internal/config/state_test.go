package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWriteStateFailureKeepsPreviousState(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	st := HostState{Paused: true, ChangedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), By: "tray"}
	if err := WriteState(p, st); err != nil {
		t.Fatal(err)
	}
	before := readText(t, p)
	oldMove := moveFileEx
	t.Cleanup(func() { moveFileEx = oldMove })
	moveFileEx = func(string, string) error { return errors.New("move failed") }
	if err := WriteState(p, HostState{By: "admin_api"}); err == nil {
		t.Fatal("expected write failure")
	}
	if readText(t, p) != before || !slices.Equal(dirNames(t, dir), []string{"state.json"}) {
		t.Fatalf("previous state changed or temporary file left: %q", dirNames(t, dir))
	}
}

func TestReadStateMissing(t *testing.T) {
	st, exists, err := ReadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil || exists || st != (HostState{}) {
		t.Fatalf("st %+v exists %v err %v", st, exists, err)
	}
}

// §5.10 спецификации v2: формат state.json и атомарная перезапись без .bak.
func TestWriteStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locallm")
	p := filepath.Join(dir, "state.json")
	at := time.Date(2026, 11, 8, 20, 15, 0, 0, time.UTC)
	if err := WriteState(p, HostState{Paused: true, ChangedAt: at, By: "tray"}); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, p); got != `{"version":1,"paused":true,"changed_at":"2026-11-08T20:15:00Z","by":"tray"}`+"\n" {
		t.Fatalf("state.json %q", got)
	}
	st, exists, err := ReadState(p)
	if err != nil || !exists || !st.Paused || st.By != "tray" || !st.ChangedAt.Equal(at) {
		t.Fatalf("st %+v exists %v err %v", st, exists, err)
	}
	local := time.Date(2026, 11, 9, 1, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	if err := WriteState(p, HostState{ChangedAt: local, By: "admin_api"}); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, p); got != `{"version":1,"paused":false,"changed_at":"2026-11-08T22:00:00Z","by":"admin_api"}`+"\n" {
		t.Fatalf("state.json %q", got)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"state.json"}) {
		t.Fatalf("files %q", names)
	}
}

// §5.10: строгий разбор — испорченный файл даёт ошибку (хост тогда стартует на паузе).
func TestReadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"version 2":     `{"version":2,"paused":true}`,
		"no version":    `{"paused":true}`,
		"no paused":     `{"version":1}`,
		"unknown field": `{"version":1,"paused":true,"extra":1}`,
		"wrong type":    `{"version":1,"paused":"yes"}`,
		"trailing data": `{"version":1,"paused":true}{}`,
		"bad time":      `{"version":1,"paused":true,"changed_at":"yesterday"}`,
		"empty":         ``,
		"not json":      `paused`,
		"null":          `null`,
		"too large":     `{"version":1,"paused":true}` + strings.Repeat(" ", 70000),
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := ReadState(p); err == nil || !exists || !strings.HasPrefix(err.Error(), "state file "+p) {
			t.Errorf("%s: exists %v err %v", name, exists, err)
		}
	}
	p := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(p, []byte("{\"version\":1,\"paused\":false}\r\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, exists, err := ReadState(p); err != nil || !exists || st != (HostState{}) {
		t.Fatalf("minimal file: st %+v exists %v err %v", st, exists, err)
	}
	if _, _, err := ReadState(dir); err == nil {
		t.Fatal("a directory must be an error")
	}
}

func TestReadStateOffsetTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"paused":true,"changed_at":"2026-11-08T23:15:00+03:00","by":"tray"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _, err := ReadState(p)
	if err != nil || !st.ChangedAt.Equal(time.Date(2026, 11, 8, 20, 15, 0, 0, time.UTC)) {
		t.Fatalf("st %+v err %v", st, err)
	}
}
