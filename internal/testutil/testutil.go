// Package testutil — общие помощники тестов без GPU.
package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

var (
	buildOnce sync.Once
	buildDir  string
	buildPath string
	buildErr  error
)

// Main запускает тесты пакета и удаляет каталог со сборкой fakeupstream.
func Main(m *testing.M) int {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	return code
}

// BuildFakeUpstream собирает cmd/fakeupstream один раз на тестовый бинарь и возвращает путь к exe.
func BuildFakeUpstream(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = fmt.Errorf("go env GOMOD: %w", err)
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(out)))
		if buildDir, buildErr = os.MkdirTemp("", "locallm-fakeupstream-"); buildErr != nil {
			return
		}
		buildPath = filepath.Join(buildDir, "fakeupstream.exe")
		cmd := exec.Command("go", "build", "-o", buildPath, "./cmd/fakeupstream")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/fakeupstream: %v\n%s", err, b)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

// FreePort — свободный порт на 127.0.0.1 (listen :0, затем close).
func FreePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// ProcessAlive — процесс с этим PID существует и ещё не завершился.
func ProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev != windows.WAIT_OBJECT_0
}

// KillPID завершает процесс, если он жив (уборка сирот в t.Cleanup).
func KillPID(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
		_ = p.Release()
	}
}

// StubFile создаёт пустой файл (и каталоги к нему).
func StubFile(t testing.TB, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
