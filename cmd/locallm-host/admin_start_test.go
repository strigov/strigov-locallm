package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// §4.7 шаг 5: ключ, который есть и среди ключей клиентов, и в файле ключа администратора, — код 4; в тексте — номер
// ключа, значение не попадает ни в stderr, ни в журнал.
func TestRunAdminKeySharedWithClientExits4(t *testing.T) {
	f, s := newTestSys(t)
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir})
	const shared = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" // тестовое значение во временном каталоге
	for _, name := range []string{"keys.txt", "admin-key.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(shared+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 4 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	log := readFile(t, filepath.Join(dir, "logs", "host.log"))
	if !strings.Contains(f.stderr.String(), "key 1 is also a client API key") || strings.Contains(f.stderr.String(), shared) ||
		strings.Contains(log, shared) {
		t.Fatalf("stderr %q", f.stderr.String())
	}
}

// §4.7 шаг 5, §5.10: файл ключа администратора создаётся при первом старте до net.Listen — строка-комментарий и один
// ключ из 43 символов, не равный ключу клиента; в журнал пишется путь, но не ключ.
func TestRunCreatesAdminKeyFile(t *testing.T) {
	f, s := newTestSys(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir, Listen: ln.Addr().String()})
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 6 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	admin := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(dir, "admin-key.txt"))), "\n")
	client := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(dir, "keys.txt"))), "\n")
	key := strings.TrimSpace(admin[len(admin)-1])
	if len(admin) != 2 || !strings.HasPrefix(admin[0], "#") || len(key) != 43 || key == strings.TrimSpace(client[len(client)-1]) {
		t.Fatalf("admin key file: %d lines, key length %d", len(admin), len(key))
	}
	log := readFile(t, filepath.Join(dir, "logs", "host.log"))
	if !strings.Contains(log, `msg="admin keys file created"`) || !strings.Contains(log, "admin-key.txt") || strings.Contains(log, key) {
		t.Fatal("host.log must name the admin key file and never contain the key")
	}
}

// §4.7 шаг 5a: нет state.json — пауза снята; файл с paused — как записано; испорченный файл — пауза и предупреждение.
// Через run: испорченный state.json даёт строку журнала до net.Listen.
func TestRunCorruptStateStartsPaused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var buf bytes.Buffer
	quiet := slog.New(slog.NewTextHandler(&buf, nil))
	if readPaused(path, quiet) {
		t.Fatal("no state.json: paused")
	}
	for _, paused := range []bool{true, false} {
		if err := config.WriteState(path, config.HostState{Paused: paused, ChangedAt: time.Now(), By: "tray"}); err != nil {
			t.Fatal(err)
		}
		if got := readPaused(path, quiet); got != paused {
			t.Fatalf("state.json paused=%v: readPaused %v", paused, got)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("readable state.json logged: %s", buf.String())
	}

	f, s := newTestSys(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := writeTestConfig(t, testConf{Dir: dir, Listen: ln.Addr().String(), DefaultModel: "a"})
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 6 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	if log := readFile(t, filepath.Join(dir, "logs", "host.log")); !strings.Contains(log, `msg="state file is unreadable; starting paused"`) {
		t.Fatalf("host.log: %s", log)
	}
	if !readPaused(path, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("corrupt state.json: not paused")
	}
}

// §4.7 шаг 9: default_model грузится, только если она задана и пауза не включена; на паузе — строка журнала.
func TestPreloadDefault(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	var loaded []string
	load := func(m string) error { loaded = append(loaded, m); return nil }
	preloadDefault(log, "a", true, load)
	preloadDefault(log, "", false, load)
	preloadDefault(log, "", true, load)
	preloadDefault(log, "b", false, load)
	if !slices.Equal(loaded, []string{"b"}) {
		t.Fatalf("loaded %v", loaded)
	}
	if !strings.Contains(buf.String(), `msg="default model skipped: host is paused" model=a`) || strings.Count(buf.String(), "skipped") != 1 {
		t.Fatalf("log: %s", buf.String())
	}
	preloadDefault(log, "c", false, func(string) error { return scheduler.ErrHalted })
	if !strings.Contains(buf.String(), `msg="default model preload failed" model=c`) {
		t.Fatalf("log: %s", buf.String())
	}
}
