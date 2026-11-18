package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

const testKey = "test-key-0123456789abcdef0123456789abcdef"

// stack — шлюз поверх планировщика с заданным бэкендом, на httptest-сервере.
type stack struct {
	t     *testing.T
	cfg   *config.Config
	live  *config.Live
	b     scheduler.Backend
	sched *scheduler.Scheduler
	srv   *server
	url   string
	logs  *syncBuffer // журнал шлюза (уровень debug); планировщик пишет в io.Discard
	fatal <-chan error
}

type stackOpt func(*server)

// withAfterGrant — тестовый шов G13: f вызывается между Grant и проверкой lease.Valid().
func withAfterGrant(f func(r *http.Request)) stackOpt { return func(s *server) { s.afterGrant = f } }

func newStack(t *testing.T, cfg *config.Config, b scheduler.Backend, logs LogSource, opts ...stackOpt) *stack {
	t.Helper()
	sched, fatal := schedtest.Start(t, cfg, b)
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	live := config.NewLive(cfg)
	srv := newServer(live, sched, logs, schedtest.NewProbe(1234), [][]byte{[]byte(testKey)}, log)
	for _, o := range opts {
		o(srv)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return &stack{t: t, cfg: cfg, live: live, b: b, sched: sched, srv: srv, url: hs.URL, logs: buf, fatal: fatal}
}

// newRealStack — шлюз над настоящим супервизором: «llama-server» — fakeupstream.exe на свободном порту,
// модели a и b (заглушки *.gguf), args[id] — флаги fakeupstream для модели.
func newRealStack(t *testing.T, args map[string][]string, mutate func(*config.Config), opts ...stackOpt) (*stack, *supervisor.Supervisor) {
	t.Helper()
	cfg := schedtest.Config("a", "b")
	cfg.Llama.ServerExe = testutil.BuildFakeUpstream(t)
	cfg.Llama.Port = testutil.FreePort(t)
	cfg.Llama.DefaultArgs = nil
	cfg.Llama.StopTimeout = 5 * time.Second
	cfg.Swap.WaitTimeout = 30 * time.Second
	dir := t.TempDir()
	for i := range cfg.Models {
		m := &cfg.Models[i]
		m.Path = filepath.Join(dir, m.ID+".gguf")
		testutil.StubFile(t, m.Path)
		m.Args = args[m.ID]
	}
	if mutate != nil {
		mutate(cfg)
	}
	sup := supervisor.New(cfg, nil)
	t.Cleanup(func() { _ = sup.Stop(context.Background()) }) // после остановки цикла: процесс не остаётся
	return newStack(t, cfg, sup, sup, opts...), sup
}

// req — запрос к шлюзу; hdr — пары имя, значение: "Host" задаёт req.Host, пустое значение удаляет заголовок.
// Тело "" — запрос без тела.
func (s *stack) req(method, path, body string, hdr ...string) (*http.Response, []byte) {
	s.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.url+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		switch {
		case hdr[i] == "Host":
			req.Host = hdr[i+1]
		case hdr[i+1] == "":
			req.Header.Del(hdr[i])
		default:
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp, readAll(resp)
}

// call — запрос к шлюзу; key "" — без Authorization.
func (s *stack) call(method, path, body, key string) (*http.Response, []byte) {
	s.t.Helper()
	var hdr []string
	if body != "" {
		hdr = append(hdr, "Content-Type", "application/json")
	}
	if key != "" {
		hdr = append(hdr, "Authorization", "Bearer "+key)
	}
	return s.req(method, path, body, hdr...)
}

func (s *stack) authed(method, path, body string) (*http.Response, []byte) {
	s.t.Helper()
	return s.call(method, path, body, testKey)
}

// wantError проверяет статус, Content-Type, code и type ошибки §9.6 и возвращает объект "error".
func wantError(t *testing.T, resp *http.Response, body []byte, status int, code, typ string) map[string]any {
	t.Helper()
	if resp.StatusCode != status || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q, body %s; want %d", resp.StatusCode, resp.Header.Get("Content-Type"), body, status)
	}
	e := decodeError(t, body)
	if e["code"] != code || e["type"] != typ {
		t.Fatalf("error %v; want code %q, type %q", e, code, typ)
	}
	return e
}
