package control

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

var admin = Actor{Kind: KindAdmin, Session: "abcd1234"}

// twoModels — a (-c 8192) и b (-c 4096) в каталоге моделей.
const twoModels = "  a:\n    path: '{models}\\a.gguf'\n    args: ['-c', '8192']\n  b:\n    path: '{models}\\b.gguf'\n    args: ['-c', '4096']\n"

// defaultHost — режим file, swap.default_model a, файлы a, b и c с заголовками GGUF.
func defaultHost() testutil.HostOptions {
	return testutil.HostOptions{DefaultModel: "a", Models: twoModels, Stubs: []string{"a.gguf", "b.gguf", "c.gguf"}}
}

// rig — контроллер над настоящим планировщиком с фейковым бэкендом; конфиг — во временном каталоге теста.
type rig struct {
	t     *testing.T
	h     testutil.HostDir
	cfg   *config.Config
	live  *config.Live
	b     *schedtest.Backend
	sched *scheduler.Scheduler
	ctl   *Controller
	log   *schedtest.SyncBuffer
}

func newRig(t *testing.T, o testutil.HostOptions) *rig {
	t.Helper()
	h := testutil.NewHostDir(t, o)
	cfg, err := config.Load(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	b := schedtest.New()
	buf := &schedtest.SyncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, nil)) // журнал планировщика и контроллера — в один буфер:
	// строку F1 «forced unload» пишет автомат, и тесты считают её одной
	sched := scheduler.New(cfg, b, schedtest.NewProbe(900), scheduler.Init{}, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		b.Close()
	})
	live := config.NewLive(cfg)
	ctl := New(Options{Live: live, Sched: sched, Logs: b, Cache: gguf.NewCache(0), Log: log})
	return &rig{t: t, h: h, cfg: cfg, live: live, b: b, sched: sched, ctl: ctl, log: buf}
}

// etag — ETag models.yaml на диске.
func (r *rig) etag() string {
	r.t.Helper()
	_, etag, _, err := config.ReadModelsFile(r.h.ModelsFile)
	if err != nil {
		r.t.Fatal(err)
	}
	return etag
}

// model — модель документа с файлом в каталоге моделей.
func (r *rig) model(id, file string, args ...string) config.DocModel {
	return config.DocModel{ID: id, Path: filepath.Join(r.h.ModelsDir, file), Args: args}
}

func doc(models ...config.DocModel) *config.ModelsDoc { return &config.ModelsDoc{Models: models} }

// loadWait — Load от имени администратора и ожидание Ready.
func (r *rig) loadWait(id string) {
	r.t.Helper()
	if err := r.ctl.Load(admin, id); err != nil {
		r.t.Fatal(err)
	}
	schedtest.Eventually(r.t, 5*time.Second, func() bool {
		s := r.sched.Snapshot()
		return s.Current == id && s.Phase == scheduler.PhaseReady
	})
}

func catalogIDs(models []config.Model) string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return strings.Join(ids, ",")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func queueHas(s *scheduler.Snapshot, id string) bool {
	return slices.ContainsFunc(s.Queue, func(q scheduler.QueueEntry) bool { return q.Model == id })
}

// §13: строка журнала каждой команды — после успеха; неизвестное имя в журнал не попадает.
func TestCommandsLog(t *testing.T) {
	r := newRig(t, defaultHost())
	r.loadWait("b")
	if err := r.ctl.Restart(admin, "b"); err != nil {
		t.Fatal(err)
	}
	if err := r.ctl.Unload(admin, "", false); err != nil {
		t.Fatal(err)
	}
	if err := r.ctl.Load(admin, "no-such-model"); !errors.Is(err, scheduler.ErrModelNotFound) {
		t.Fatalf("unknown model: %v", err)
	}
	log := r.log.String()
	for _, want := range []string{
		`msg="admin command" action=load actor=admin_api session=abcd1234 model=b force=false`,
		`msg="admin command" action=restart actor=admin_api session=abcd1234 model=b force=false`,
		`msg="admin command" action=unload actor=admin_api session=abcd1234 model="" force=false`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	if strings.Contains(log, "no-such-model") {
		t.Fatal("unknown model name logged")
	}
}

// §5.10, §9.5: пауза пишет state.json, persisted == true; снятие паузы из трея — тоже.
func TestPausePersisted(t *testing.T) {
	r := newRig(t, defaultHost())
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r.ctl.now = func() time.Time { return at }
	persisted, err := r.ctl.Pause(admin, false)
	if err != nil || !persisted || !r.sched.Snapshot().Paused {
		t.Fatalf("pause: %v %v", persisted, err)
	}
	st, ok, err := config.ReadState(r.h.State)
	if err != nil || !ok || !st.Paused || st.By != KindAdmin || !st.ChangedAt.Equal(at) {
		t.Fatalf("state %+v %v %v", st, ok, err)
	}
	if persisted, err = r.ctl.Resume(Actor{Kind: KindTray}); err != nil || !persisted || r.sched.Snapshot().Paused {
		t.Fatalf("resume: %v %v", persisted, err)
	}
	if st, _, _ = config.ReadState(r.h.State); st.Paused || st.By != KindTray || !r.ctl.pauseSaved.Load() {
		t.Fatalf("state %+v", st)
	}
	for _, want := range []string{"action=pause actor=admin_api", "action=resume actor=tray", "paused=false persisted=true"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("no %q in log", want)
		}
	}
}

// §9.5: state.json не записан — пауза действует, persisted == false, pause_saved == false.
func TestPauseWriteFailure(t *testing.T) {
	r := newRig(t, defaultHost())
	if err := os.Mkdir(r.h.State, 0o755); err != nil { // на месте state.json — каталог
		t.Fatal(err)
	}
	persisted, err := r.ctl.Pause(admin, false)
	if err != nil || persisted || !r.sched.Snapshot().Paused || r.ctl.pauseSaved.Load() {
		t.Fatalf("pause: %v %v", persisted, err)
	}
	if !strings.Contains(r.log.String(), "paused=true persisted=false") {
		t.Fatal("no warning in log")
	}
	if err := os.Remove(r.h.State); err != nil {
		t.Fatal(err)
	}
	if persisted, err = r.ctl.Resume(admin); err != nil || !persisted || !r.ctl.pauseSaved.Load() {
		t.Fatalf("resume: %v %v", persisted, err)
	}
}

// fakeSched — Scheduler для теста порядка: Pause и Resume запоминают порядок команд.
type fakeSched struct {
	Scheduler // остальные методы не вызываются
	mu        sync.Mutex
	paused    bool
	order     []bool
}

func (f *fakeSched) Pause(bool) error { return f.set(true) }
func (f *fakeSched) Resume() error    { return f.set(false) }

func (f *fakeSched) set(p bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = p
	f.order = append(f.order, p)
	return nil
}

func (f *fakeSched) Snapshot() *scheduler.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &scheduler.Snapshot{Paused: f.paused}
}

// §4.4: 1000 команд паузы из 8 горутин — записи state.json в порядке команд автомата, последняя равна состоянию.
func TestPauseResumeOrdering(t *testing.T) {
	f := &fakeSched{}
	cfg := &config.Config{}
	cfg.Server.StateFile = filepath.Join(t.TempDir(), "state.json")
	ctl := New(Options{Live: config.NewLive(cfg), Sched: f})
	var mu sync.Mutex
	var written []bool
	ctl.writeState = func(_ string, st config.HostState) error {
		if rand.IntN(4) == 0 {
			time.Sleep(time.Microsecond) // без мьютекса паузы записи перемешались бы
		}
		mu.Lock()
		written = append(written, st.Paused)
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 125 {
				var err error
				if (g+i)%2 == 0 {
					_, err = ctl.Pause(admin, false)
				} else {
					_, err = ctl.Resume(admin)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(written) != 1000 || !slices.Equal(written, f.order) {
		t.Fatalf("%d writes; order differs from the scheduler", len(written))
	}
	if written[len(written)-1] != f.Snapshot().Paused {
		t.Fatal("last write differs from the scheduler state")
	}
}

// F3-D1 (ledger планировщика): Pause(force) и Unload(force) через контроллер снимают поколение (RetireForced).
// Строка F1 «forced unload» — только одна, её пишет автомат (forceRetire), а не контроллер; при выгрузке во время
// Loading F1 не выполняется и строки нет.
func TestForcedPauseAbortsLeases(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(r *rig) error
	}{
		{"pause", func(r *rig) error { _, err := r.ctl.Pause(admin, true); return err }},
		{"unload", func(r *rig) error { return r.ctl.Unload(admin, "a", true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, defaultHost())
			r.loadWait("a")
			lease, err := r.sched.Acquire(context.Background(), "a", false)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if err := tc.do(r); err != nil {
				t.Fatal(err)
			}
			// Команда возвращает результат раньше эффектов шага (§4.5), а Retire — раньше строки F1:
			// ждём конца цепочки остановки, когда все эффекты шага (включая журналы) выполнены.
			schedtest.Eventually(t, 5*time.Second, func() bool {
				s := r.sched.Snapshot()
				return s.Current == "" && s.Phase == scheduler.PhaseEmpty
			})
			if lease.Valid() || lease.Upstream.RetireReason() != scheduler.RetireForced {
				t.Fatalf("lease valid %v, retire reason %v", lease.Valid(), lease.Upstream.RetireReason())
			}
			log := r.log.String()
			if n := strings.Count(log, `msg="forced unload"`); n != 1 || !strings.Contains(log, `msg="forced unload" model=a`) ||
				!strings.Contains(log, "aborted=1") {
				t.Fatalf("forced unload lines: %d\n%s", n, log)
			}
		})
	}
	t.Run("loading", func(t *testing.T) { // F1 не выполняется в Loading: строки нет
		r := newRig(t, defaultHost())
		gate := make(chan struct{})
		r.b.SetPlan("a", schedtest.Plan{Gate: gate})
		if err := r.ctl.Load(admin, "a"); err != nil {
			t.Fatal(err)
		}
		schedtest.Eventually(t, 5*time.Second, func() bool {
			s := r.sched.Snapshot()
			return s.Current == "a" && s.Phase == scheduler.PhaseLoading
		})
		if err := r.ctl.Unload(admin, "a", true); err != nil {
			t.Fatal(err)
		}
		close(gate) // загрузка доезжает до Ready, намерение выгрузки проходит обычным путём (RetireStop, не F1)
		schedtest.Eventually(t, 5*time.Second, func() bool {
			s := r.sched.Snapshot()
			return s.Current == "" && s.Phase == scheduler.PhaseEmpty
		})
		if n := strings.Count(r.log.String(), `msg="forced unload"`); n != 0 {
			t.Fatalf("forced unload during Loading: %d lines\n%s", n, r.log.String())
		}
	})
}
