package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/vram"
)

var errStopped = errors.New("scheduler is not running")

type acquireResult struct {
	lease *Lease
	err   error
}

// envelope — событие с каналом подтверждения (WaiterGone), результата команды (буфер 1, §4.5 v2) или ответа
// (Acquire, буфер 1).
type envelope struct {
	ev     Event
	ack    chan struct{}
	result chan error
	reply  chan acquireResult
}

// Init — начальное состояние автомата (§4.4 v2): Paused — пауза из state.json (подключает план host-v2-admin).
type Init struct{ Paused bool }

// Scheduler — цикл-владелец автомата (§4.5).
type Scheduler struct {
	cfg     *config.Config
	params  Params
	backend Backend
	probe   vram.Probe
	onFatal func(error)
	log     *slog.Logger

	events  chan envelope
	stopped chan struct{}
	snap    atomic.Pointer[Snapshot]
	ids     atomic.Uint64

	// только горутина-владелец:
	st      State
	waiters map[uint64]chan acquireResult
	timer   *time.Timer
	runCtx  context.Context

	beforeGone func() // тестовый шов (export_test.go)
}

// New — onFatal вызывается эффектом Fatal (§6.3); в хосте он не возвращается (§4.7). Каталог автомата — копия
// cfg.Models; дальше он меняется только командой Reconfigure, а из cfg читаются лишь глобальные поля.
func New(cfg *config.Config, b Backend, p vram.Probe, init Init, onFatal func(error), log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if onFatal == nil {
		onFatal = func(error) {}
	}
	s := &Scheduler{
		cfg:     cfg,
		params:  Params{SwapCooldown: cfg.Swap.SwapCooldown, MaxQueue: cfg.Swap.MaxQueue, FailedTTL: cfg.Swap.FailedTTL},
		backend: b, probe: p, onFatal: onFatal, log: log,
		events: make(chan envelope, 64), stopped: make(chan struct{}),
		st: initialState(time.Now(), cloneCatalog(cfg.Models), init.Paused), waiters: map[uint64]chan acquireResult{},
		runCtx: context.Background(),
	}
	s.snap.Store(newSnapshot(s.st, s.params))
	return s
}

// Run — цикл владельца до отмены ctx.
func (s *Scheduler) Run(ctx context.Context) {
	s.runCtx = ctx
	defer func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		close(s.stopped)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case env := <-s.events:
			s.handle(env)
		}
	}
}

func (s *Scheduler) handle(env envelope) {
	if a, ok := env.ev.(evAcquire); ok {
		s.waiters[a.ID] = env.reply
	}
	next, effs := step(s.st, env.ev, time.Now(), s.params) // при Halted — без эффектов (E15)
	s.st = next
	if g, ok := env.ev.(evWaiterGone); ok {
		delete(s.waiters, g.ID)
	}
	s.snap.Store(newSnapshot(s.st, s.params))
	if env.ack != nil {
		close(env.ack)
	}
	if env.result != nil { // §4.5 v2: результат — после Snapshot и раньше остальных эффектов шага
		env.result <- cmdErr(effs)
	}
	for _, e := range effs {
		s.exec(e)
	}
}

// cmdErr — Err эффекта CmdResult; его нет только при Halted (C1) — тогда ErrHalted.
func cmdErr(effs []Effect) error {
	for _, e := range effs {
		if r, ok := e.(effCmdResult); ok {
			return r.Err
		}
	}
	return ErrHalted
}

func (s *Scheduler) exec(e Effect) {
	switch e := e.(type) {
	case effGrant:
		l := &Lease{Model: e.Model, Gen: e.Gen, Upstream: e.Upstream, Waited: e.Waited}
		gen := e.Gen
		l.release = func() { s.post(envelope{ev: evRelease{Gen: gen}}) }
		s.reply(e.ID, acquireResult{lease: l})
	case effReject:
		s.reply(e.ID, acquireResult{err: e.Err})
	case effStartModel:
		go s.startModel(e.Gen, e.Model)
	case effRetireUpstream: // синхронно, до следующего события (§4.5)
		if e.Upstream != nil {
			e.Upstream.Retire(e.Reason)
		}
	case effStopModel:
		go s.stopModel(e.Gen)
	case effWaitVRAM:
		go s.waitVRAM(e.Gen, e.Baseline)
	case effSetTimer:
		if s.timer != nil {
			s.timer.Stop()
		}
		kind := e.Kind
		s.timer = time.AfterFunc(time.Until(e.At), func() { s.post(envelope{ev: evTimer{Kind: kind}}) })
	case effFatal:
		go s.onFatal(e.Err) // владелец не ждёт: onFatal показывает MessageBoxW (§4.7)
	case effLog:
		s.log.LogAttrs(context.Background(), e.Level, e.Msg, e.Attrs...)
	case effCmdResult: // уже отправлен в канал результата (handle)
	}
}

// reply — неблокирующая отправка ожидающему; Lease без получателя сразу освобождается.
func (s *Scheduler) reply(id uint64, r acquireResult) {
	ch, ok := s.waiters[id]
	delete(s.waiters, id)
	if ok {
		select {
		case ch <- r:
			return
		default:
		}
	}
	if r.lease != nil {
		go r.lease.Release()
	}
}

// post — отправка события владельцу; false — цикл остановлен.
func (s *Scheduler) post(env envelope) bool {
	select {
	case s.events <- env:
		return true
	case <-s.stopped:
		return false
	}
}

func (s *Scheduler) measure(d time.Duration) int64 {
	if s.probe == nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(s.runCtx, d)
	defer cancel()
	v, err := s.probe.UsageMiB(ctx)
	if err != nil {
		return -1
	}
	return v
}

// startModel — исполнитель StartModel (§6.3): Baseline, затем Backend.Start с load_timeout. Определение m
// пришло из эффекта (def(m) момента P11, §6.2 v2); в конфиге модель не ищется.
func (s *Scheduler) startModel(gen uint64, m config.Model) {
	base := s.measure(3 * time.Second)
	s.post(envelope{ev: evBaseline{Gen: gen, MiB: base}})
	ctx, cancel := context.WithTimeout(s.runCtx, s.cfg.Llama.LoadTimeout)
	up, err := s.backend.Start(ctx, m)
	cancel()
	var lf LoadFailure
	switch {
	case err == nil:
		if usage := s.measure(3 * time.Second); usage >= 0 && base >= 0 {
			s.log.Info("model vram", "model", m.ID, "gen", gen, "baseline_mib", base, "usage_mib", usage, "model_mib", usage-base)
		}
		s.post(envelope{ev: evLoadOK{Gen: gen, Upstream: up}})
		go s.watch(gen, up)
	case errors.Is(err, ErrStopUnconfirmed):
		s.post(envelope{ev: evStopFailed{Gen: gen, PID: s.backend.Status().PID, Message: err.Error()}})
	case errors.As(err, &lf):
		reason, code, tail := lf.LoadFailure()
		s.post(envelope{ev: evLoadFailed{Gen: gen, ExitCode: code, Message: failureMessage(reason, tail)}})
	default:
		s.post(envelope{ev: evLoadFailed{Gen: gen, ExitCode: -1, Message: "spawn: " + err.Error()}})
	}
}

// failureMessage — хвост вывода; для причин, кроме "exited", с префиксом причины.
func failureMessage(reason string, tail []string) string {
	msg := strings.Join(tail, "\n")
	switch {
	case msg == "":
		return reason
	case reason == "" || reason == "exited":
		return msg
	}
	return reason + ": " + msg
}

// watch — наблюдатель Upstream.Done → ProcessExited.
func (s *Scheduler) watch(gen uint64, up *Upstream) {
	if up == nil || up.Done == nil {
		return
	}
	select {
	case info, ok := <-up.Done:
		if !ok {
			info = ExitInfo{Code: -1}
		}
		s.post(envelope{ev: evProcessExited{Gen: gen, ExitCode: info.Code, Message: strings.Join(info.Tail, "\n")}})
	case <-s.stopped:
	}
}

// stopModel — исполнитель StopModel. Background: остановку не прерывают — отменённый ctx дал бы
// ErrStopUnconfirmed без попытки TerminateProcess (§7.4).
func (s *Scheduler) stopModel(gen uint64) {
	if err := s.backend.Stop(context.Background()); err != nil {
		s.post(envelope{ev: evStopFailed{Gen: gen, PID: s.backend.Status().PID, Message: err.Error()}})
		return
	}
	s.post(envelope{ev: evStopped{Gen: gen}})
}

func (s *Scheduler) waitVRAM(gen uint64, baseline int64) {
	r := vram.WaitReleased(s.runCtx, s.probe, baseline, s.cfg.Swap.VRAMMarginMiB, s.cfg.Swap.VRAMFreeTimeout)
	s.post(envelope{ev: evVRAMFreed{Gen: gen, Waited: r.Waited, LastMiB: r.LastMiB, TimedOut: r.TimedOut, Unknown: r.Unknown}})
}

// Acquire — протокол §6.9 п. 2–4.
func (s *Scheduler) Acquire(ctx context.Context, model string, explicit bool) (*Lease, error) {
	return s.acquire(ctx, evAcquire{Model: model, Explicit: explicit})
}

// AcquireLoaded — Lease на уже загруженную модель (model "" — на текущую) для GET /props (F5-D1).
// Проверка и выдача — один шаг владельца; ответ приходит сразу, загрузка и пересадка не начинаются.
func (s *Scheduler) AcquireLoaded(ctx context.Context, model string) (*Lease, error) {
	return s.acquire(ctx, evAcquire{Model: model, LoadedOnly: true})
}

func (s *Scheduler) acquire(ctx context.Context, ev evAcquire) (*Lease, error) {
	id := s.ids.Add(1)
	ev.ID = id
	reply := make(chan acquireResult, 1)
	select {
	case s.events <- envelope{ev: ev, reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopped:
		return nil, errStopped
	}
	select {
	case r := <-reply:
		return r.lease, r.err
	case <-s.stopped:
		return nil, errStopped
	case <-ctx.Done():
	}
	if s.beforeGone != nil {
		s.beforeGone()
	}
	ack := make(chan struct{})
	if s.post(envelope{ev: evWaiterGone{ID: id}, ack: ack}) {
		select {
		case <-ack:
		case <-s.stopped:
		}
	}
	select { // Grant мог быть выдан до обработки WaiterGone
	case r := <-reply:
		if r.lease != nil {
			r.lease.Release()
		}
	default:
	}
	return nil, ctx.Err()
}

// Load — явное намерение (C2–C4). Команды возвращаются после обработки события и публикации Snapshot:
// результат уже виден в Snapshot(). Ошибки: ErrModelNotFound, ErrPaused, ErrHalted.
func (s *Scheduler) Load(model string) error { return s.command(evLoad{Model: model}) }

// Unload — мягкая выгрузка (C5–C7); model "" — всё. Текущую модель можно выгрузить, даже если её уже нет в
// каталоге. Ошибки: ErrModelNotFound, ErrHalted.
func (s *Scheduler) Unload(model string) error { return s.command(evUnload{Model: model}) }

// ForceUnload — выгрузка с обрывом начатых ответов (C8, F1); только для администратора.
func (s *Scheduler) ForceUnload(model string) error {
	return s.command(evUnload{Model: model, Force: true})
}

// Restart — перезапуск с текущим определением из каталога (C9–C12). Ошибки: ErrModelNotFound, ErrPaused, ErrHalted.
func (s *Scheduler) Restart(model string) error { return s.command(evRestart{Model: model}) }

// Pause — пауза (C13); force — с обрывом начатых ответов. Ошибка: ErrHalted.
func (s *Scheduler) Pause(force bool) error { return s.command(evPause{Force: force}) }

// Resume — снятие паузы (C14); ничего не загружает. Ошибка: ErrHalted.
func (s *Scheduler) Resume() error { return s.command(evResume{}) }

// Reconfigure — замена каталога (C15g, C15); guard — отказ *InUseError, если удаляемое имя занято (RC0).
// Каталог копируется: вызывающий может дальше менять свой срез.
func (s *Scheduler) Reconfigure(catalog []config.Model, guard bool) error {
	return s.command(evReconfigure{Catalog: cloneCatalog(catalog), Guard: guard})
}

// command — команда автомату: результат из канала с буфером 1; errStopped — цикл остановлен.
func (s *Scheduler) command(ev Event) error {
	res := make(chan error, 1)
	if !s.post(envelope{ev: ev, result: res}) {
		return errStopped
	}
	select {
	case err := <-res:
		return err
	case <-s.stopped:
		select { // результат мог прийти одновременно с остановкой
		case err := <-res:
			return err
		default:
			return errStopped
		}
	}
}

// Snapshot — последний опубликованный снимок, без блокировок.
func (s *Scheduler) Snapshot() *Snapshot { return s.snap.Load() }
