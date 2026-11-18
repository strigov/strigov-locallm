package scheduler_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

func ctxFor(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestLoopAcquireLoadsAndGrants(t *testing.T) {
	b := schedtest.New()
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), b)
	l, err := s.Acquire(ctxFor(t, 5*time.Second), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if l.Model != "a" || l.Gen != 1 || !l.Valid() {
		t.Fatalf("lease %+v", l)
	}
	resp, err := (&http.Client{Transport: l.Upstream.Transport}).Get(l.Upstream.URL.String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health %d", resp.StatusCode)
	}
	sn := s.Snapshot()
	if sn.Phase != scheduler.PhaseReady || sn.Current != "a" || sn.Inflight != 1 || sn.PID != 1001 || sn.Baseline != 900 {
		t.Fatalf("snapshot %+v", sn)
	}
	l.Release()
	schedtest.Eventually(t, 2*time.Second, func() bool { return s.Snapshot().Inflight == 0 })
	if got := b.Starts(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("starts %v", got)
	}
}

func TestLoopReleaseIsIdempotent(t *testing.T) {
	s, _ := schedtest.Start(t, schedtest.Config("a"), schedtest.New())
	ctx := ctxFor(t, 5*time.Second)
	l1, err := s.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := s.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	l1.Release()
	l1.Release()
	if err := s.Load("a"); err != nil { // события обрабатываются по порядку: оба Release — раньше
		t.Fatal(err)
	}
	if n := s.Snapshot().Inflight; n != 1 {
		t.Fatalf("inflight %d, want 1", n)
	}
	l2.Release()
	if err := s.Load("a"); err != nil {
		t.Fatal(err)
	}
	if n := s.Snapshot().Inflight; n != 0 {
		t.Fatalf("inflight %d, want 0", n)
	}
}

func TestLoopWaiterGoneRacesGrant(t *testing.T) {
	b := schedtest.New()
	gate := make(chan struct{})
	b.SetPlan("a", schedtest.Plan{Gate: gate})
	s, _ := schedtest.Start(t, schedtest.Config("a"), b, func(sc *scheduler.Scheduler) {
		scheduler.SetBeforeWaiterGone(sc, func() {
			close(gate) // Grant выдаётся раньше, чем будет обработан WaiterGone
			schedtest.Eventually(t, 5*time.Second, func() bool {
				sn := sc.Snapshot()
				return sn.Phase == scheduler.PhaseReady && sn.Inflight == 1
			})
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for s.Snapshot().Phase != scheduler.PhaseLoading {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	l, err := s.Acquire(ctx, "a", false)
	if l != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("lease %v err %v", l, err)
	}
	schedtest.Eventually(t, 2*time.Second, func() bool { return s.Snapshot().Inflight == 0 })
}

func TestLoopSnapshotIsInstantUnderLoad(t *testing.T) {
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), schedtest.New())
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.Load([]string{"a", "b"}[i%2])
		}
	}()
	start := time.Now()
	for range 100_000 {
		if s.Snapshot() == nil {
			t.Fatal("nil snapshot")
		}
	}
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	if elapsed > 200*time.Millisecond {
		t.Fatalf("100000 snapshots took %v", elapsed)
	}
}

func TestLoopLoadDuringLoadingVisibleOnReturn(t *testing.T) {
	b := schedtest.New()
	gate := make(chan struct{})
	b.SetPlan("a", schedtest.Plan{Gate: gate})
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), b)
	defer close(gate)
	if err := s.Load("a"); err != nil {
		t.Fatal(err)
	}
	if sn := s.Snapshot(); sn.Phase != scheduler.PhaseLoading || sn.Current != "a" {
		t.Fatalf("after Load(a): %+v", sn)
	}
	if err := s.Load("b"); err != nil {
		t.Fatal(err)
	}
	sn := s.Snapshot()
	if sn.Intent == nil || *sn.Intent != (scheduler.Intent{Kind: scheduler.IntentLoad, Model: "b"}) ||
		sn.ModelStatus("b", time.Now()) != scheduler.StatusLoading || sn.ModelStatus("a", time.Now()) != scheduler.StatusLoading {
		t.Fatalf("after Load(b): %+v", sn)
	}
	if err := s.Load("zz"); !errors.Is(err, scheduler.ErrModelNotFound) {
		t.Fatalf("unknown model: %v", err)
	}
}

func TestLoopRetireBeforeNextEvent(t *testing.T) {
	s, _ := schedtest.Start(t, schedtest.Config("a", "c"), schedtest.New())
	l, err := s.Acquire(ctxFor(t, 5*time.Second), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unload(""); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Phase != scheduler.PhaseDraining || !l.Valid() {
		t.Fatalf("draining expected, lease valid: %+v", s.Snapshot())
	}
	l.Release()
	if err := s.Unload("c"); err != nil { // обрабатывается после Release и всех его эффектов
		t.Fatal(err)
	}
	if l.Valid() || l.Context().Err() == nil {
		t.Fatal("RetireUpstream must run before the next event is handled")
	}
}

func TestLoopProcessExitRetiresAndMarksFailed(t *testing.T) {
	b := schedtest.New()
	s, _ := schedtest.Start(t, schedtest.Config("a"), b)
	ctx := ctxFor(t, 5*time.Second)
	l, err := s.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	b.Exit(3, "fatal: crashed")
	schedtest.Eventually(t, 3*time.Second, func() bool { return s.Snapshot().Phase == scheduler.PhaseEmpty })
	if l.Valid() {
		t.Fatal("lease must be invalid after the process exit")
	}
	sn := s.Snapshot()
	if f := sn.Failed["a"]; sn.ModelStatus("a", time.Now()) != scheduler.StatusFailed || f.ExitCode != 3 || f.Message != "fatal: crashed" {
		t.Fatalf("failed %+v", sn.Failed)
	}
	l.Release() // устаревший Release — счётчик не уходит в минус
	_, err = s.Acquire(ctx, "a", false)
	var le *scheduler.LoadFailedError
	if !errors.As(err, &le) || !le.Cached || le.ExitCode != 3 {
		t.Fatalf("cached reject expected: %v", err)
	}
	if n := s.Snapshot().Inflight; n != 0 {
		t.Fatalf("inflight %d", n)
	}
}

func TestLoopExecutorClassifiesStartErrors(t *testing.T) {
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Fail: true})
	b.SetPlan("b", schedtest.Plan{PlainErr: true})
	b.SetPlan("c", schedtest.Plan{Gate: make(chan struct{})}) // не открывается: истекает load_timeout
	cfg := schedtest.Config("a", "b", "c")
	cfg.Llama.LoadTimeout = 300 * time.Millisecond
	s, _ := schedtest.Start(t, cfg, b)
	for _, c := range []struct {
		model string
		code  int
		msg   string
	}{
		{"a", 1, "error: failed to load model"},
		{"b", -1, "spawn: exec: file does not exist"},
		{"c", -1, "timeout: load timeout"},
	} {
		_, err := s.Acquire(ctxFor(t, 5*time.Second), c.model, false)
		var le *scheduler.LoadFailedError
		if !errors.As(err, &le) || le.Cached || le.ExitCode != c.code || le.Message != c.msg {
			t.Fatalf("%s: %v", c.model, err)
		}
	}
}

func TestLoopStartUnconfirmedIsFatal(t *testing.T) {
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{StopUnconfirmed: true})
	s, fatal := schedtest.Start(t, schedtest.Config("a", "b"), b)
	ctx := ctxFor(t, 3*time.Second)
	go func() { _, _ = s.Acquire(ctx, "a", false) }()
	select {
	case err := <-fatal:
		if !errors.Is(err, scheduler.ErrStopUnconfirmed) || !strings.Contains(err.Error(), `model "a", pid 1001, gen 1`) {
			t.Fatalf("fatal %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onFatal was not called")
	}
	for name, cmd := range map[string]func() error{ // C1: CmdResult нет — ErrHalted
		"Load":        func() error { return s.Load("b") },
		"Unload":      func() error { return s.Unload("") },
		"ForceUnload": func() error { return s.ForceUnload("a") },
		"Restart":     func() error { return s.Restart("a") },
		"Pause":       func() error { return s.Pause(true) },
		"Resume":      s.Resume,
		"Reconfigure": func() error { return s.Reconfigure(nil, false) },
	} {
		if err := cmd(); !errors.Is(err, scheduler.ErrHalted) {
			t.Fatalf("%s after halt: %v", name, err)
		}
	}
	sn := s.Snapshot()
	if !sn.Halted || sn.Phase != scheduler.PhaseLoading || sn.Gen != 1 || len(b.Starts()) != 1 || len(sn.Catalog) != 2 {
		t.Fatalf("halted snapshot %+v starts %v", sn, b.Starts())
	}
}

func TestLoopStopUnconfirmedIsFatal(t *testing.T) {
	b := schedtest.New()
	s, fatal := schedtest.Start(t, schedtest.Config("a"), b)
	l, err := s.Acquire(ctxFor(t, 5*time.Second), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	b.SetStopErr(scheduler.ErrStopUnconfirmed)
	if err := s.Unload(""); err != nil {
		t.Fatal(err)
	}
	l.Release()
	select {
	case err := <-fatal:
		if !errors.Is(err, scheduler.ErrStopUnconfirmed) || !strings.Contains(err.Error(), `model "a", pid 1001, gen 1`) {
			t.Fatalf("fatal %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onFatal was not called")
	}
	if sn := s.Snapshot(); !sn.Halted || sn.Phase != scheduler.PhaseStopping {
		t.Fatalf("snapshot %+v", sn)
	}
}

func TestLoopExplicitAcquireIgnoresCooldown(t *testing.T) {
	cfg := schedtest.Config("a", "b")
	cfg.Swap.SwapCooldown = time.Hour
	s, _ := schedtest.Start(t, cfg, schedtest.New())
	ctx := ctxFor(t, 5*time.Second)
	la, err := s.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Acquire(ctx, "b", false)
	var ce *scheduler.CooldownError
	if !errors.As(err, &ce) || ce.RetryAfter != 3600 {
		t.Fatalf("implicit acquire must hit cooldown: %v", err)
	}
	got := make(chan *scheduler.Lease, 1)
	go func() { l, _ := s.Acquire(ctx, "b", true); got <- l }()
	schedtest.Eventually(t, 2*time.Second, func() bool { return s.Snapshot().Phase == scheduler.PhaseDraining })
	la.Release()
	select {
	case lb := <-got:
		if lb == nil || lb.Model != "b" || lb.Gen != 2 {
			t.Fatalf("lease %+v", lb)
		}
		lb.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("explicit acquire did not finish")
	}
}

func TestLoopAcquireLoadedNeverLoads(t *testing.T) {
	b := schedtest.New()
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), b)
	ctx := ctxFor(t, 5*time.Second)
	if _, err := s.AcquireLoaded(ctx, "a"); !errors.Is(err, scheduler.ErrModelNotLoaded) {
		t.Fatalf("nothing loaded: %v", err)
	}
	la, err := s.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	la.Release()
	l, err := s.AcquireLoaded(ctx, "")
	if err != nil || l.Model != "a" || !l.Valid() {
		t.Fatalf("lease %+v, err %v", l, err)
	}
	l.Release()
	if _, err := s.AcquireLoaded(ctx, "b"); !errors.Is(err, scheduler.ErrModelNotLoaded) {
		t.Fatalf("another model: %v", err)
	}
	if got := b.Starts(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("starts %v", got)
	}
}

// runLoop запускает Run планировщика, созданного вручную; уборка — как у schedtest.Start.
func runLoop(t *testing.T, s *scheduler.Scheduler, b *schedtest.Backend) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		b.Close()
	})
}

// gateProbe держит исполнителя StartModel на замере Baseline до закрытия gate, то есть до Backend.Start.
type gateProbe struct{ gate chan struct{} }

func (p gateProbe) Name() string { return "gate" }

func (p gateProbe) UsageMiB(ctx context.Context) (int64, error) {
	select {
	case <-p.gate:
		return 900, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Команда возвращается после публикации Snapshot: её результат уже виден.
func TestLoopCommandResultVisibleInSnapshot(t *testing.T) {
	b := schedtest.New()
	gate := make(chan struct{})
	b.SetPlan("a", schedtest.Plan{Gate: gate}) // модель держится в Loading: Snapshot читается до Ready
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), b)
	defer close(gate)
	if err := s.Pause(false); err != nil {
		t.Fatal(err)
	}
	if !s.Snapshot().Paused {
		t.Fatal("pause is not visible on return")
	}
	if err := s.Load("a"); !errors.Is(err, scheduler.ErrPaused) {
		t.Fatalf("load on pause: %v", err)
	}
	if err := s.Restart("a"); !errors.Is(err, scheduler.ErrPaused) {
		t.Fatalf("restart on pause: %v", err)
	}
	if _, err := s.Acquire(ctxFor(t, time.Second), "a", false); !errors.Is(err, scheduler.ErrPaused) {
		t.Fatalf("acquire on pause: %v", err)
	}
	if err := s.Resume(); err != nil || s.Snapshot().Paused {
		t.Fatalf("resume: %v, paused %v", err, s.Snapshot().Paused)
	}
	for _, err := range []error{s.Load("zz"), s.Unload("zz"), s.Restart("zz")} {
		if !errors.Is(err, scheduler.ErrModelNotFound) {
			t.Fatalf("unknown model: %v", err)
		}
	}
	if err := s.Load("a"); err != nil {
		t.Fatal(err)
	}
	if sn := s.Snapshot(); sn.Phase != scheduler.PhaseLoading || sn.Current != "a" || sn.Running.ID != "a" {
		t.Fatalf("load is not visible on return: %+v", sn)
	}
}

// StartModel несёт определение момента P11, даже если Reconfigure пришёл раньше Backend.Start.
func TestLoopStartModelCarriesP11Definition(t *testing.T) {
	b := schedtest.New()
	gate := make(chan struct{})
	s := scheduler.New(schedtest.Config("a", "b"), b, gateProbe{gate: gate}, scheduler.Init{}, nil, nil)
	runLoop(t, s, b)
	if err := s.Load("a"); err != nil {
		t.Fatal(err)
	}
	next := schedtest.Config("a", "b").Models
	next[0].Args = []string{"-c", "8192"}
	if err := s.Reconfigure(next, true); err != nil {
		t.Fatal(err)
	}
	next[0].Args[1] = "1" // каталог скопирован: правка вызывающего автомату не видна
	close(gate)
	schedtest.Eventually(t, 5*time.Second, func() bool { return s.Snapshot().Phase == scheduler.PhaseReady })
	sn := s.Snapshot()
	if defs := b.Defs(); len(defs) != 1 || !slices.Equal(defs[0].Args, []string{"-c", "4096"}) {
		t.Fatalf("defs %+v", defs)
	}
	if !sn.RestartRequired() || !slices.Equal(sn.Running.Args, []string{"-c", "4096"}) || !slices.Equal(sn.Catalog[0].Args, []string{"-c", "8192"}) {
		t.Fatalf("snapshot running %+v catalog %+v", sn.Running, sn.Catalog)
	}
	if err := s.Restart("a"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := s.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Gen == 2
	})
	if defs := b.Defs(); len(defs) != 2 || !slices.Equal(defs[1].Args, []string{"-c", "8192"}) || s.Snapshot().RestartRequired() {
		t.Fatalf("defs after restart %+v", defs)
	}
}

// ForceUnload снимает поколение с причиной RetireForced; Lease становится недействительным.
func TestLoopForceUnloadRetiresWithReason(t *testing.T) {
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), schedtest.New())
	l, err := s.Acquire(ctxFor(t, 5*time.Second), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err := s.ForceUnload(""); err != nil {
		t.Fatal(err)
	}
	if err := s.Unload("b"); err != nil { // барьер: шаг обрабатывается после всех эффектов ForceUnload
		t.Fatal(err)
	}
	if l.Valid() || l.Upstream.RetireReason() != scheduler.RetireForced {
		t.Fatalf("lease valid %v, reason %d", l.Valid(), l.Upstream.RetireReason())
	}
	if sn := s.Snapshot(); sn.Inflight != 0 || sn.Phase == scheduler.PhaseReady {
		t.Fatalf("snapshot %+v", sn)
	}
}

func TestLoopInitPaused(t *testing.T) {
	b := schedtest.New()
	s := scheduler.New(schedtest.Config("a"), b, schedtest.NewProbe(900), scheduler.Init{Paused: true}, nil, nil)
	if !s.Snapshot().Paused {
		t.Fatal("initial snapshot is not paused")
	}
	runLoop(t, s, b)
	if _, err := s.Acquire(ctxFor(t, time.Second), "a", false); !errors.Is(err, scheduler.ErrPaused) {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Load("a"); !errors.Is(err, scheduler.ErrPaused) {
		t.Fatalf("load: %v", err)
	}
	if len(b.Starts()) != 0 {
		t.Fatalf("starts %v", b.Starts())
	}
}

// RC0 через API и копия каталога.
func TestLoopReconfigureGuardAndCopy(t *testing.T) {
	s, _ := schedtest.Start(t, schedtest.Config("a", "b"), schedtest.New())
	l, err := s.Acquire(ctxFor(t, 5*time.Second), "a", false)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	err = s.Reconfigure(schedtest.Config("b").Models, true)
	var iu *scheduler.InUseError
	if !errors.As(err, &iu) || !slices.Equal(iu.Models, []string{"a"}) ||
		err.Error() != `model "a" is in use (loaded, loading or requested); unload it first` {
		t.Fatalf("guard: %v", err)
	}
	if len(s.Snapshot().Catalog) != 2 {
		t.Fatalf("catalog changed: %+v", s.Snapshot().Catalog)
	}
	next := schedtest.Config("a", "c").Models
	if err := s.Reconfigure(next, true); err != nil { // b не занята
		t.Fatal(err)
	}
	next[1].ID = "zz"
	if sn := s.Snapshot(); len(sn.Catalog) != 2 || sn.Catalog[1].ID != "c" {
		t.Fatalf("catalog %+v", sn.Catalog)
	}
	if err := s.Load("b"); !errors.Is(err, scheduler.ErrModelNotFound) {
		t.Fatalf("removed model: %v", err)
	}
	if e := (&scheduler.InUseError{Models: []string{"a", "b"}}).Error(); e != `models "a", "b" are in use (loaded, loading or requested); unload them first` {
		t.Fatalf("plural message %q", e)
	}
}
