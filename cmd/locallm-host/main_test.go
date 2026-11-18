package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/desktop"
	"github.com/strigov/strigov-locallm/internal/gateway"
	"github.com/strigov/strigov-locallm/internal/logging"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/testutil"
	"github.com/strigov/strigov-locallm/internal/vram"
)

// testConf — параметры writeTestConfig; пустые поля получают значения по умолчанию.
type testConf struct {
	Dir, Listen, ServerExe, VramSource, DefaultModel, Cooldown, WaitTimeout string
	Port                                                                    int
	ModelArgs                                                               map[string][]string
	Extra                                                                   string                         // ключи верхнего уровня перед models: строки YAML с "\n" в конце
	Profiles                                                                map[string]map[string][]string // профили: id модели → имя профиля → args
	ModelsMode                                                              string                         // "" — models в locallm.yaml (legacy); "file" — в <Dir>\models.yaml; "absent" — ни ключа, ни файла
	Headers                                                                 bool                           // заглушки моделей — заголовки GGUF (llama, context_length 131072, embedding_length 4096)
}

func yamlList(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// writeTestConfig пишет <Dir>\locallm.yaml с моделями a и b (заглушки <Dir>\models\<id>.gguf), ключами
// <Dir>\keys.txt и <Dir>\admin-key.txt, состоянием <Dir>\state.json и журналом <Dir>\logs; модели — по c.ModelsMode
// (§5.1 спецификации v2). Возвращает путь к файлу.
func writeTestConfig(t *testing.T, c testConf) string {
	t.Helper()
	if c.Dir == "" {
		c.Dir = t.TempDir()
	}
	if c.Listen == "" {
		c.Listen = fmt.Sprintf("127.0.0.1:%d", testutil.FreePort(t))
	}
	if c.Port == 0 {
		c.Port = testutil.FreePort(t)
	}
	if c.ServerExe == "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		c.ServerExe = exe
	}
	if c.VramSource == "" {
		c.VramSource = "none"
	}
	if c.Cooldown == "" {
		c.Cooldown = "0s"
	}
	if c.WaitTimeout == "" {
		c.WaitTimeout = "30s"
	}
	var models strings.Builder
	for _, id := range []string{"a", "b"} {
		p := filepath.Join(c.Dir, "models", id+".gguf")
		if c.Headers {
			testutil.GGUFEmbd(t, p, "llama", 131072, 4096)
		} else {
			testutil.StubFile(t, p)
		}
		fmt.Fprintf(&models, "  %s:\n    path: '%s'\n    args: %s\n", id, p, yamlList(c.ModelArgs[id]))
		if prof := c.Profiles[id]; len(prof) > 0 {
			models.WriteString("    profiles:\n")
			for _, name := range slices.Sorted(maps.Keys(prof)) {
				fmt.Fprintf(&models, "      %s: %s\n", name, yamlList(prof[name]))
			}
		}
	}
	modelsKey := "models:\n" + models.String()
	switch c.ModelsMode {
	case "file":
		if err := os.WriteFile(filepath.Join(c.Dir, "models.yaml"), []byte("version: 1\n"+modelsKey), 0o644); err != nil {
			t.Fatal(err)
		}
		modelsKey = ""
	case "absent":
		modelsKey = ""
	}
	y := fmt.Sprintf(`server:
  listen: %s
  api_keys_file: '%s'
  admin_key_file: '%s'
  state_file: '%s'
llama:
  server_exe: '%s'
  port: %d
  default_args: []
  load_timeout: 30s
  stop_timeout: 5s
  log_lines: 1000
swap:
  default_model: '%s'
  swap_cooldown: %s
  wait_timeout: %s
  vram_free_timeout: 1s
  vram_margin_mib: 512
  max_queue: 8
  failed_ttl: 120s
vram:
  source: %s
logging:
  dir: '%s'
  level: debug
  max_size_mib: 10
  max_files: 5
%s%s`, c.Listen, filepath.Join(c.Dir, "keys.txt"), filepath.Join(c.Dir, "admin-key.txt"), filepath.Join(c.Dir, "state.json"),
		c.ServerExe, c.Port, c.DefaultModel, c.Cooldown, c.WaitTimeout,
		c.VramSource, filepath.Join(c.Dir, "logs"), c.Extra, modelsKey)
	path := filepath.Join(c.Dir, "locallm.yaml")
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakeSys struct {
	stderr  bytes.Buffer
	dialogs []string
}

// newTestSys — sys без побочных эффектов: Job Object не трогается, окна не показываются.
func newTestSys(t *testing.T) (*fakeSys, sys) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	f := &fakeSys{}
	return f, sys{
		stderr:   &f.stderr,
		enterJob: func() error { return nil },
		detect:   vram.Detect,
		dialog:   func(text string) { f.dialogs = append(f.dialogs, text) },
		exit:     func(code int) { t.Errorf("unexpected exit(%d)", code) },
		backend: func(cfg *config.Config, log *slog.Logger) (scheduler.Backend, gateway.LogSource) {
			sup := supervisor.New(cfg, log)
			return sup, sup
		},
		instance:   desktop.AcquireInstance, // имена — от временного пути конфига теста
		openURL:    func(*slog.Logger, string) error { return errors.New("browser is disabled in tests") },
		confirm:    func(string) bool { t.Error("unexpected confirmation dialog"); return false },
		openFile:   func(*slog.Logger, string) error { return errors.New("notepad is disabled in tests") },
		openFolder: func(*slog.Logger, string) error { return errors.New("explorer is disabled in tests") },
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunBadFlagExits2(t *testing.T) {
	f, s := newTestSys(t)
	if code := run([]string{"--bogus", "--no-dialog"}, s); code != 2 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(f.stderr.String(), "-bogus") || !strings.Contains(f.stderr.String(), "(exit code 2)") || len(f.dialogs) != 0 {
		t.Fatalf("stderr %q, dialogs %v", f.stderr.String(), f.dialogs)
	}
	if log := readFile(t, logging.FallbackPath()); !strings.Contains(log, `msg="locallm-host failed to start"`) || !strings.Contains(log, "exit_code=2") {
		t.Fatalf("fallback log: %s", log)
	}
	if code := run([]string{"--bogus"}, s); code != 2 || len(f.dialogs) != 1 ||
		!strings.Contains(f.dialogs[0], "failed to start (exit code 2)") || !strings.Contains(f.dialogs[0], "Log: "+logging.FallbackPath()) {
		t.Fatalf("dialog: code %d, %v", code, f.dialogs)
	}
}

func TestRunMissingConfigExits2(t *testing.T) {
	f, s := newTestSys(t)
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := run([]string{"--no-dialog", "--config", missing}, s); code != 2 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(f.stderr.String(), "nope.yaml") {
		t.Fatalf("stderr %q", f.stderr.String())
	}
}

func TestRunJobObjectFailureExits3(t *testing.T) {
	f, s := newTestSys(t)
	s.enterJob = func() error { return errors.New("access is denied") }
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir})
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 3 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	if log := readFile(t, filepath.Join(dir, "logs", "host.log")); !strings.Contains(log, "exit_code=3") || !strings.Contains(log, "access is denied") {
		t.Fatalf("host.log: %s", log)
	}
}

func TestRunBadKeysFileExits4(t *testing.T) {
	f, s := newTestSys(t)
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir})
	if err := os.WriteFile(filepath.Join(dir, "keys.txt"), []byte("tooshortkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 4 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	log := readFile(t, filepath.Join(dir, "logs", "host.log"))
	if !strings.Contains(f.stderr.String(), "line 1") || strings.Contains(f.stderr.String(), "tooshortkey") || strings.Contains(log, "tooshortkey") {
		t.Fatalf("key line must be named, key value never shown: stderr %q", f.stderr.String())
	}
}

func TestRunVRAMSourceFailureExits5(t *testing.T) {
	f, s := newTestSys(t)
	s.detect = func(context.Context, string, *slog.Logger) (vram.Probe, error) {
		return nil, errors.New("vram source nvidia-smi is unavailable")
	}
	cfg := writeTestConfig(t, testConf{})
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 5 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
}

func TestRunBusyListenExits6(t *testing.T) {
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
	keys := readFile(t, filepath.Join(dir, "keys.txt")) // создан на шаге 5, до net.Listen
	lines := strings.Split(strings.TrimSpace(keys), "\n")
	if len(lines) != 2 || len(strings.TrimSpace(lines[1])) != 43 {
		t.Fatalf("generated keys file: %d lines", len(lines))
	}
	if log := readFile(t, filepath.Join(dir, "logs", "host.log")); strings.Contains(log, strings.TrimSpace(lines[1])) {
		t.Fatal("generated key leaked into host.log")
	}
}

func TestFatalRuntimeClosesServerAndExits7(t *testing.T) {
	var logBuf bytes.Buffer
	var order []string
	exit := -1
	h := &host{
		sys:     sys{dialog: func(s string) { order = append(order, "dialog: "+s) }, exit: func(c int) { exit = c }},
		log:     slog.New(slog.NewTextHandler(&logBuf, nil)),
		logPath: `C:\logs\host.log`,
		srv:     &http.Server{},
		code:    make(chan int, 1),
	}
	if !h.setTrayQuit(func() { order = append(order, "icon removed") }) {
		t.Fatal("tray must be accepted before exit")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.srv.Serve(ln) }()
	h.fatalRuntime(fmt.Errorf("%w: model %q, pid %d, gen %d", scheduler.ErrStopUnconfirmed, "a", 4242, 3))
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("http.Server was not closed")
	}
	select {
	case c := <-h.code:
		if c != 7 {
			t.Fatalf("exit code in the channel: %d", c)
		}
	default:
		t.Fatal("exit code 7 must be sent")
	}
	// §4.7: значок убирается до окна, чтобы человек не нажимал пункты меню останавливающегося хоста.
	if exit != 7 || len(order) != 2 || order[0] != "icon removed" || !strings.Contains(order[1], "exit code 7") ||
		!strings.Contains(order[1], "pid 4242") || !strings.Contains(order[1], `Log: C:\logs\host.log`) {
		t.Fatalf("exit %d, order %q", exit, order)
	}
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "pid 4242") {
		t.Fatalf("log: %s", logBuf.String())
	}
}

// Предупреждения конфига (§5.2 спецификации v2) попадают в host.log сразу после его открытия (шаг 3).
func TestRunLogsConfigWarnings(t *testing.T) {
	f, s := newTestSys(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir, Listen: ln.Addr().String(),
		Extra: "models_dirs: ['" + filepath.Join(dir, "no-such-models-dir") + "']\n"})
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 6 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	log := readFile(t, filepath.Join(dir, "logs", "host.log"))
	if !strings.Contains(log, `msg="config warning"`) || !strings.Contains(log, "no-such-models-dir") || !strings.Contains(log, "skipped") {
		t.Fatalf("host.log: %s", log)
	}
}
