// Package schedtest — фейковые Backend и Probe для тестов планировщика и шлюза: «процесс» —
// httptest-сервер в этом же процессе (по умолчанию fakeupstream.New), без exe и без GPU.
package schedtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/fakeupstream"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Plan — поведение фейкового запуска модели.
type Plan struct {
	Handler         http.Handler  // вместо fakeupstream.New
	Gate            chan struct{} // Start ждёт закрытия Gate; отмена ctx — Failure{timeout}
	Fail            bool          // Start → *Failure{"exited", 1, ["error: failed to load model"]}
	PlainErr        bool          // Start → ошибка, не реализующая LoadFailure
	StopUnconfirmed bool          // Start → scheduler.ErrStopUnconfirmed, процесс остаётся «живым»
}

// Failure реализует scheduler.LoadFailure, как *supervisor.LoadError.
type Failure struct {
	Reason   string
	ExitCode int
	Tail     []string
}

func (f *Failure) Error() string {
	return fmt.Sprintf("llama-server %s (exit code %d)", f.Reason, f.ExitCode)
}
func (f *Failure) LoadFailure() (string, int, []string) { return f.Reason, f.ExitCode, f.Tail }

type proc struct {
	model string
	pid   int
	since time.Time
	srv   *httptest.Server
	done  chan scheduler.ExitInfo
	once  sync.Once
}

func (p *proc) exit(code int, tail []string) {
	p.once.Do(func() {
		if p.srv != nil {
			p.srv.CloseClientConnections()
			go p.srv.Close()
		}
		p.done <- scheduler.ExitInfo{Code: code, At: time.Now(), Tail: tail}
		close(p.done)
	})
}

// Backend — фейковый scheduler.Backend и gateway.LogSource.
type Backend struct {
	Log *SyncBuffer // строки запросов фейковых процессов (формат fakeupstream)

	mu      sync.Mutex
	plans   map[string]Plan
	cur     *proc
	pid     int
	starts  []string
	defs    []config.Model
	stopErr error
	lines   map[string][]string
	last    string
}

var _ scheduler.Backend = (*Backend)(nil)

// New — бэкенд без процессов; PID первого запуска — 1001.
func New() *Backend {
	return &Backend{Log: &SyncBuffer{}, plans: map[string]Plan{}, lines: map[string][]string{}, pid: 1000}
}

func (b *Backend) SetPlan(model string, p Plan) { b.mu.Lock(); b.plans[model] = p; b.mu.Unlock() }
func (b *Backend) SetStopErr(err error)         { b.mu.Lock(); b.stopErr = err; b.mu.Unlock() }
func (b *Backend) Starts() []string             { b.mu.Lock(); defer b.mu.Unlock(); return slices.Clone(b.starts) }

// Defs — определения, полученные Start, по порядку (§14.2 v2: StartModel несёт определение момента P11).
func (b *Backend) Defs() []config.Model {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.defs)
}

func (b *Backend) Start(ctx context.Context, m config.Model) (*scheduler.Upstream, error) {
	b.mu.Lock()
	if b.cur != nil {
		b.mu.Unlock()
		return nil, errors.New("schedtest: llama-server is already running")
	}
	pl := b.plans[m.ID]
	b.pid++
	pid := b.pid
	b.starts = append(b.starts, m.ID)
	b.defs = append(b.defs, m)
	b.last = m.ID
	b.lines[m.ID] = append(b.lines[m.ID], fmt.Sprintf("--- start pid=%d ---", pid))
	b.mu.Unlock()
	if pl.Gate != nil {
		select {
		case <-pl.Gate:
		case <-ctx.Done():
			return nil, &Failure{Reason: "timeout", ExitCode: -1, Tail: []string{"load timeout"}}
		}
	}
	switch {
	case pl.StopUnconfirmed:
		b.mu.Lock()
		b.cur = &proc{model: m.ID, pid: pid, since: time.Now(), done: make(chan scheduler.ExitInfo, 1)}
		b.mu.Unlock()
		return nil, scheduler.ErrStopUnconfirmed
	case pl.Fail:
		b.AppendLog(m.ID, "error: failed to load model")
		return nil, &Failure{Reason: "exited", ExitCode: 1, Tail: []string{"error: failed to load model"}}
	case pl.PlainErr:
		return nil, errors.New("exec: file does not exist")
	}
	h := pl.Handler
	if h == nil {
		h = fakeupstream.New(fakeupstream.Options{ModelID: m.ID}, b.Log)
	}
	p := &proc{model: m.ID, pid: pid, since: time.Now(), srv: httptest.NewServer(h), done: make(chan scheduler.ExitInfo, 1)}
	u, _ := url.Parse(p.srv.URL)
	b.mu.Lock()
	b.cur = p
	b.mu.Unlock()
	return scheduler.NewUpstream(m.ID, u, pid, p.done), nil
}

func (b *Backend) Stop(context.Context) error {
	b.mu.Lock()
	p, err := b.cur, b.stopErr
	if err == nil {
		b.cur = nil
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if p != nil {
		p.exit(0, nil)
	}
	return nil
}

func (b *Backend) Status() scheduler.BackendStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return scheduler.BackendStatus{}
	}
	return scheduler.BackendStatus{Running: true, Model: b.cur.model, PID: b.cur.pid, Since: b.cur.since}
}

// Exit — «падение» текущего процесса с кодом и хвостом вывода.
func (b *Backend) Exit(code int, tail ...string) {
	b.mu.Lock()
	p := b.cur
	b.cur = nil
	b.mu.Unlock()
	if p != nil {
		b.AppendLog(p.model, tail...)
		p.exit(code, tail)
	}
}

// Logs — как supervisor.Logs: model "" — последняя запускавшаяся; ok == false — не запускалась.
func (b *Backend) Logs(model string, tail int) ([]string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if model == "" {
		model = b.last
	}
	l, ok := b.lines[model]
	if !ok {
		return nil, false
	}
	if tail > 0 && len(l) > tail {
		l = l[len(l)-tail:]
	}
	return slices.Clone(l), true
}

func (b *Backend) AppendLog(model string, lines ...string) {
	b.mu.Lock()
	b.lines[model] = append(b.lines[model], lines...)
	b.mu.Unlock()
}

// ForgetLogs — как supervisor.ForgetLogs: забыть вывод имён ids, кроме работающего «процесса».
func (b *Backend) ForgetLogs(ids []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		if b.cur != nil && b.cur.model == id {
			continue
		}
		delete(b.lines, id)
		if b.last == id {
			b.last = ""
		}
	}
}

// Close завершает текущий «процесс» (уборка в t.Cleanup).
func (b *Backend) Close() {
	b.mu.Lock()
	p := b.cur
	b.cur = nil
	b.mu.Unlock()
	if p != nil {
		p.exit(0, nil)
	}
}

// Probe — фейковый vram.Probe с заданным значением.
type Probe struct{ mib atomic.Int64 }

func NewProbe(mib int64) *Probe {
	p := &Probe{}
	p.mib.Store(mib)
	return p
}

func (p *Probe) Set(mib int64)                           { p.mib.Store(mib) }
func (p *Probe) Name() string                            { return "fake" }
func (p *Probe) UsageMiB(context.Context) (int64, error) { return p.mib.Load(), nil }

// SyncBuffer — потокобезопасный bytes.Buffer для журналов.
type SyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *SyncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Config — конфиг для тестов без файлов (валидация config.Load не применяется).
func Config(ids ...string) *config.Config {
	c := &config.Config{
		Path:    "test.yaml",
		Version: "test",
		Server:  config.Server{Listen: "127.0.0.1:0"},
		Llama:   config.Llama{ServerExe: `C:\fake\llama-server.exe`, Port: 1, LoadTimeout: 10 * time.Second, StopTimeout: 2 * time.Second, LogLines: 1000},
		Swap: config.Swap{SwapCooldown: 0, WaitTimeout: 5 * time.Second, VRAMFreeTimeout: time.Second, VRAMMarginMiB: 512,
			MaxQueue: 8, FailedTTL: 120 * time.Second},
		Vram:    config.Vram{Source: "none"},
		Logging: config.Logging{Level: "debug"},
	}
	for _, id := range ids {
		c.Models = append(c.Models, config.Model{ID: id, Path: `C:\models\` + id + `.gguf`, Args: []string{"-c", "4096"}, ContextLength: 4096})
	}
	return c
}

// Start создаёт планировщик с NewProbe(900) и Init{}, выполняет setup до запуска цикла и запускает Run.
// Канал — ошибки onFatal. Уборка: отмена Run, ожидание выхода, Close у бэкенда (если есть).
func Start(t testing.TB, cfg *config.Config, b scheduler.Backend, setup ...func(*scheduler.Scheduler)) (*scheduler.Scheduler, <-chan error) {
	t.Helper()
	fatal := make(chan error, 1)
	s := scheduler.New(cfg, b, NewProbe(900), scheduler.Init{}, func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, f := range setup {
		f(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		if c, ok := b.(interface{ Close() }); ok {
			c.Close()
		}
	})
	return s, fatal
}

// Eventually ждёт cond не дольше d (опрос каждые 5 мс).
func Eventually(t testing.TB, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
	}
}
