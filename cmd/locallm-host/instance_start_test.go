package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/desktop"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// §4.7 шаг 1a, §11.5: хост с этим конфигом уже запущен — попросить его открыть панель и выйти с кодом 0, не открывая
// конфиг и журнал.
func TestRunSecondInstanceAsksFirstAndExits0(t *testing.T) {
	f, s := newTestSys(t)
	dir := t.TempDir()
	cfg := writeTestConfig(t, testConf{Dir: dir})
	first, isFirst, err := desktop.AcquireInstance(desktop.InstanceName(cfg))
	if err != nil || !isFirst {
		t.Fatalf("test instance: %v %v", isFirst, err)
	}
	defer first.Close()
	var asked atomic.Int32
	go first.Serve(func() { asked.Add(1) })
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	if asked.Load() != 1 || len(f.dialogs) != 0 {
		t.Fatalf("asked %d, dialogs %q", asked.Load(), f.dialogs)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the second instance must stop before opening the log")
	}
}

// §11.5: первый не ответил за instanceWait — код 6, окно на русском (§3.3), строка в журнале до его открытия.
func TestRunSecondInstanceNoAnswerExits6(t *testing.T) {
	prev := instanceWait
	t.Cleanup(func() { instanceWait = prev })
	instanceWait = 200 * time.Millisecond
	f, s := newTestSys(t)
	cfg := writeTestConfig(t, testConf{})
	first, _, err := desktop.AcquireInstance(desktop.InstanceName(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close() // не отвечает: Serve не запущен
	if code := run([]string{"--config", cfg}, s); code != 6 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	want := "locallm-host уже запущен, но не отвечает. Если он завис, завершите его в Диспетчере задач и запустите снова"
	if len(f.dialogs) != 1 || f.dialogs[0] != want || !strings.Contains(f.stderr.String(), "did not answer within 200ms (exit code 6)") {
		t.Fatalf("dialogs %q, stderr %q", f.dialogs, f.stderr.String())
	}
	if log := readFile(t, filepath.Join(os.Getenv("LOCALAPPDATA"), "locallm", "logs", "host.log")); !strings.Contains(log, "exit_code=6") {
		t.Fatalf("fallback log: %s", log)
	}
}

// §11.5: мьютекс не создался — хост работает без защиты, предупреждение после шага 3.
func TestRunInstanceUnavailableWarns(t *testing.T) {
	f, s := newTestSys(t)
	s.instance = func(string) (*desktop.Instance, bool, error) { return nil, false, errors.New("CreateMutex: denied") }
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
	if log := readFile(t, filepath.Join(dir, "logs", "host.log")); !strings.Contains(log, `msg="single instance check is unavailable; running without it"`) {
		t.Fatalf("host.log: %s", log)
	}
}

// §11.6: штатный выход через тестовый хук — код 0, строка журнала с причиной, сервер закрыт, state.json не записан.
func TestRunQuitHookExits0(t *testing.T) {
	f, s := newTestSys(t)
	dir := t.TempDir()
	listen := fmt.Sprintf("127.0.0.1:%d", testutil.FreePort(t))
	cfg := writeTestConfig(t, testConf{Dir: dir, Listen: listen})
	s.testQuit = func(quit func()) {
		go func() {
			for i := 0; i < 200; i++ {
				if resp, err := http.Get("http://" + listen + "/health"); err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			quit()
		}()
	}
	if code := run([]string{"--no-dialog", "--config", cfg}, s); code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, f.stderr.String())
	}
	if log := readFile(t, filepath.Join(dir, "logs", "host.log")); !strings.Contains(log, `msg="locallm-host exiting" reason=test`) {
		t.Fatalf("host.log: %s", log)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json on exit: %v", err)
	}
	if resp, err := http.Get("http://" + listen + "/health"); err == nil {
		resp.Body.Close()
		t.Fatal("server still answers after exit")
	}
}

// §11.6: Halted во время выхода (флаг onFatal или ErrHalted от Shutdown) — побеждает onFatal, код 0 не отправляется.
func TestShutdownAfterFatalKeepsCode7(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fatal bool
		err   error
	}{
		{"fatal flag", true, nil},
		{"halted", false, scheduler.ErrHalted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := &host{log: slog.New(slog.NewTextHandler(&buf, nil)), srv: &http.Server{}, code: make(chan int, 1),
				cfg: &config.Config{Llama: config.Llama{StopTimeout: time.Second}}}
			h.fatal.Store(tc.fatal)
			calls := 0
			h.ctlShutdown = func(_ context.Context, closeServer func()) error { calls++; closeServer(); return tc.err }
			h.shutdown("tray")
			h.shutdown("tray") // второй вызов ничего не делает
			select {
			case c := <-h.code:
				t.Fatalf("shutdown sent code %d; onFatal must win", c)
			default:
			}
			if calls != 1 || strings.Contains(buf.String(), "locallm-host exiting") {
				t.Fatalf("calls %d, log:\n%s", calls, buf.String())
			}
		})
	}
}

// §11.6: канал кода с буфером 1 — побеждает первый; значок убирается один раз, после этого трей не создаётся.
func TestFinishFirstWinsAndRemovesIconOnce(t *testing.T) {
	h := &host{code: make(chan int, 1)}
	quits := 0
	if !h.setTrayQuit(func() { quits++ }) {
		t.Fatal("tray refused before exit")
	}
	h.finish(0)
	h.finish(7)
	if c := <-h.code; c != 0 || quits != 1 {
		t.Fatalf("code %d, icon removed %d times", c, quits)
	}
	if h.setTrayQuit(func() {}) {
		t.Fatal("tray must not start after the icon was removed")
	}
}

// §11.3, §11.5: браузер не открылся — строка журнала и окно с адресом панели без кода (в своей горутине); код не
// попадает в журнал.
func TestOpenPanelFailureShowsAddressWithoutCode(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:18080"
	dialogs := make(chan string, 1)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	var opened string
	h := &host{sys: sys{dialog: func(s string) { dialogs <- s }}, log: log,
		ctl: control.New(control.Options{Live: config.NewLive(cfg), Auth: control.NewAuth(nil, log),
			OpenURL: func(u string) error { opened = u; return errors.New("no default browser") }})}
	h.panelRequest()
	select {
	case d := <-dialogs:
		if d != "Не удалось открыть браузер. Откройте панель вручную: http://127.0.0.1:18080/ui/" {
			t.Fatalf("dialog %q", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no dialog")
	}
	code := strings.TrimPrefix(opened, "http://127.0.0.1:18080/ui/#otp=")
	if len(code) != 43 || strings.Contains(buf.String(), code) || !strings.Contains(buf.String(), `msg="second instance asked for the panel"`) ||
		!strings.Contains(buf.String(), `msg="panel was not opened"`) {
		t.Fatalf("code length %d, log:\n%s", len(code), buf.String())
	}
	h.noDialog = true
	h.openPanel()
	select {
	case d := <-dialogs:
		t.Fatalf("--no-dialog: dialog %q", d)
	case <-time.After(100 * time.Millisecond):
	}
}
