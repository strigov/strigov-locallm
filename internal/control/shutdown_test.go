package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// shutdownSched — фейковый планировщик для Shutdown: записывает Pause и закрытие сервера, фазы отдаёт по очереди
// (последняя повторяется).
type shutdownSched struct {
	Scheduler    // остальные методы не вызываются
	mu           sync.Mutex
	events       []string
	pauseErr     error
	pauseBlock   chan struct{}   // F3-B1: не nil — Pause ждёт его (тест высвобождает через t.Cleanup)
	haltAfterCtx context.Context // F3-B3: не nil — Snapshot сообщает Halted, когда этот ctx завершён
	phases       []scheduler.Phase
	halted       bool
	paused       bool
}

func (f *shutdownSched) record(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *shutdownSched) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func (f *shutdownSched) Pause(force bool) error {
	if f.pauseBlock != nil {
		<-f.pauseBlock // F3-B1: Pause дольше срока ctx — срок должен оборвать весь Shutdown
	}
	f.record(fmt.Sprintf("pause force=%v", force))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr == nil {
		f.paused = true
	}
	return f.pauseErr
}

func (f *shutdownSched) Snapshot() *scheduler.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.phases[0]
	if len(f.phases) > 1 {
		f.phases = f.phases[1:]
	}
	return &scheduler.Snapshot{Phase: p, Halted: f.halted || (f.haltAfterCtx != nil && f.haltAfterCtx.Err() != nil),
		Paused: f.paused}
}

func shutdownController(t *testing.T, f *shutdownSched) *Controller {
	cfg := &config.Config{}
	cfg.Server.StateFile = filepath.Join(t.TempDir(), "state.json")
	ctl := New(Options{Live: config.NewLive(cfg), Sched: f})
	ctl.shutdownPoll = time.Millisecond
	ctl.writeState = func(string, config.HostState) error {
		t.Error("state.json must not be written on exit")
		return nil
	}
	return ctl
}

// §11.6 пп. 2–4: Pause(true), затем закрытие сервера, затем ожидание WaitVRAM; state.json не пишется; мьютекс паузы
// не отпускается — Resume во время выхода ждёт.
func TestShutdownOrder(t *testing.T) {
	f := &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseReady, scheduler.PhaseDraining, scheduler.PhaseStopping, scheduler.PhaseWaitVRAM}}
	ctl := shutdownController(t, f)
	if err := ctl.Shutdown(context.Background(), func() { f.record("close") }); err != nil {
		t.Fatal(err)
	}
	if got := f.log(); !slices.Equal(got, []string{"pause force=true", "close"}) {
		t.Fatalf("events %v", got)
	}
	if len(f.phases) != 1 || f.phases[0] != scheduler.PhaseWaitVRAM {
		t.Fatalf("Shutdown returned before WaitVRAM: %v left", f.phases)
	}
	resumed := make(chan struct{})
	go func() { _, _ = ctl.Resume(admin); close(resumed) }()
	select {
	case <-resumed:
		t.Fatal("Resume ran during shutdown: the pause mutex must stay locked")
	case <-time.After(100 * time.Millisecond):
	}
}

// §11.6 п. 4: Empty и Loading — сразу; Halted — ErrHalted (побеждает onFatal); срок ctx — ctx.Err(); ошибка Pause
// возвращается как есть, сервер всё равно закрыт.
func TestShutdownOutcomes(t *testing.T) {
	stopped := errors.New("scheduler stopped")
	for _, tc := range []struct {
		name     string
		f        *shutdownSched
		deadline time.Duration
		want     error
	}{
		{"empty", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseEmpty}}, time.Second, nil},
		{"loading", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseLoading}}, time.Second, nil},
		{"halted", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseStopping}, halted: true}, time.Second, scheduler.ErrHalted},
		{"timeout", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseReady}}, 50 * time.Millisecond, context.DeadlineExceeded},
		{"pause halted", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseStopping}, pauseErr: scheduler.ErrHalted}, time.Second, scheduler.ErrHalted},
		{"pause failed", &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseReady}, pauseErr: stopped}, time.Second, stopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := shutdownController(t, tc.f)
			ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
			defer cancel()
			closed := false
			err := ctl.Shutdown(ctx, func() { closed = true })
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) || !closed {
				t.Fatalf("err %v (want %v), server closed %v", err, tc.want, closed)
			}
		})
	}
}

// F3-B1: срок ctx ограничивает весь вызов Shutdown, а не только ожидание фазы: Pause, заблокированный дольше срока,
// не должен заставлять «Выход» ждать (предел §11.6 — 3 × stop_timeout + 2 с). Рабочая горутина после срока ещё
// завершается в фоне — канал с буфером 1, t.Cleanup высвобождает Pause.
func TestShutdownDeadlineBoundsPause(t *testing.T) {
	f := &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseEmpty}, pauseBlock: make(chan struct{})}
	t.Cleanup(func() { close(f.pauseBlock) }) // без этого рабочая горутина ждала бы вечно
	ctl := shutdownController(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := ctl.Shutdown(ctx, func() {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown returned after %v with a 50 ms deadline", d)
	}
}

// F3-B2: Halted важнее срока ctx. Если пауза автомата (шаг 2 §11.6) задержалась дольше срока, а автомат тем временем
// остановился (onFatal), выход с ошибкой срока дал бы хосту код 0 вместо 7.
func TestShutdownDeadlineKeepsHalted(t *testing.T) {
	f := &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseStopping}, halted: true, pauseBlock: make(chan struct{})}
	t.Cleanup(func() { close(f.pauseBlock) })
	ctl := shutdownController(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := ctl.Shutdown(ctx, func() {}); !errors.Is(err, scheduler.ErrHalted) {
		t.Fatalf("err %v", err)
	}
}

// F3-B3: Halted, опубликованный после последнего опроса внутреннего цикла, всё равно побеждает: рабочая горутина
// прочитала Halted=false, затем истёк срок ctx — её ctx.Err() из канала done финальная проверка заменяет на
// scheduler.ErrHalted. Фейк держит фазу Ready и сообщает Halted только после завершения ctx.
func TestShutdownDeadlineHaltedAfterLastPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	f := &shutdownSched{phases: []scheduler.Phase{scheduler.PhaseReady}, haltAfterCtx: ctx}
	ctl := shutdownController(t, f)
	if err := ctl.Shutdown(ctx, func() {}); !errors.Is(err, scheduler.ErrHalted) {
		t.Fatalf("err %v", err)
	}
}

// §11.6 на настоящем автомате: загруженная модель выгружается, пауза только в памяти, state.json не появляется.
func TestShutdownUnloadsWithoutStateFile(t *testing.T) {
	r := newRig(t, defaultHost())
	r.loadWait("a")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	closed := false
	if err := r.ctl.Shutdown(ctx, func() { closed = true }); err != nil {
		t.Fatal(err)
	}
	s := r.sched.Snapshot()
	if !closed || !s.Paused || s.Current != "" || (s.Phase != scheduler.PhaseWaitVRAM && s.Phase != scheduler.PhaseEmpty) {
		t.Fatalf("closed %v, snapshot phase %v current %q paused %v", closed, s.Phase, s.Current, s.Paused)
	}
	if _, err := os.Stat(r.h.State); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json on exit: %v", err)
	}
}
