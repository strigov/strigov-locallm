package scheduler

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// machine — рабочая копия состояния на время одного вызова step.
type machine struct {
	st     State
	now    time.Time
	p      Params
	effs   []Effect
	reason string // поле reason строк журнала о смене фазы (§13)
}

// step — чистая функция переходов (§6 MVP и §6 v2): не мутирует st, не делает I/O, не читает часы.
func step(st State, ev Event, now time.Time, p Params) (State, []Effect) {
	if st.Halted { // E15 и C1 — раньше всех правил; эффектов нет, в том числе Log и CmdResult
		return st, nil
	}
	m := &machine{st: st.clone(), now: now, p: p, reason: "implicit"}
	if m.handle(ev) && !m.st.Halted {
		m.pump()
	}
	return m.st, m.effs
}

func (m *machine) emit(e Effect) { m.effs = append(m.effs, e) }

func (m *machine) log(level slog.Level, msg string, attrs ...slog.Attr) {
	m.emit(effLog{Level: level, Msg: msg, Attrs: attrs})
}

// result — CmdResult принятой команды или отказа.
func (m *machine) result(err error) { m.emit(effCmdResult{Err: err}) }

// refuse — отказ команды (C2, C3, C5, C9, C10, C15g): состояние не меняется, продвижения нет,
// единственный эффект — CmdResult.
func (m *machine) refuse(err error) bool {
	m.result(err)
	return false
}

func notFound(model string) error { return fmt.Errorf("%w: %q", ErrModelNotFound, model) }

// inCatalog — m ∈ Catalog.
func (m *machine) inCatalog(id string) bool {
	_, ok := findModel(m.st.Catalog, id)
	return ok
}

// setPhase меняет фазу и пишет строку §13: phase, from, to, reason, длительность прошлой фазы.
func (m *machine) setPhase(ph Phase, from, to string) {
	prev, prevMs := m.st.Phase, m.now.Sub(m.st.PhaseSince).Milliseconds()
	m.st.Phase, m.st.PhaseSince = ph, m.now
	m.log(slog.LevelInfo, "phase", slog.String("phase", ph.String()), slog.String("prev_phase", prev.String()),
		slog.String("from", from), slog.String("to", to), slog.String("reason", m.reason), slog.Int64("prev_phase_ms", prevMs))
}

// stale — E13: событие чужого поколения или не своей фазы.
func (m *machine) stale(ev Event, gen uint64) bool {
	m.log(slog.LevelDebug, "stale event", slog.String("event", ev.eventName()), slog.Uint64("event_gen", gen),
		slog.Uint64("gen", m.st.Gen), slog.String("phase", m.st.Phase.String()))
	return false
}

// handle применяет правило события или команды (§6.4, §6.5 MVP и v2); false — событие устаревшее или
// команда отклонена, продвижение не нужно.
func (m *machine) handle(ev Event) bool {
	st := &m.st
	switch e := ev.(type) {
	case evAcquire:
		m.acquire(e)
	case evWaiterGone: // E1
		m.removeWaiter(e.ID)
	case evRelease: // E2
		if e.Gen != st.Gen || !st.serving() || st.Inflight == 0 {
			return m.stale(ev, e.Gen)
		}
		st.Inflight--
		st.LastActivity = m.now
	case evLoad: // C2–C4
		if !m.inCatalog(e.Model) {
			return m.refuse(notFound(e.Model))
		}
		if st.Paused {
			return m.refuse(ErrPaused)
		}
		m.reason = "explicit_load"
		m.applyLoad(e.Model)
		m.result(nil)
	case evUnload: // C5–C8; выгрузка разрешена на паузе
		if e.Model != "" && e.Model != st.Current && !m.inCatalog(e.Model) {
			return m.refuse(notFound(e.Model))
		}
		m.reason = "explicit_unload"
		m.applyUnload(e.Model)
		if e.Force && (e.Model == "" || e.Model == st.Current) && st.serving() { // C8
			m.forceRetire()
		}
		m.result(nil)
	case evRestart: // C9–C12
		if !m.inCatalog(e.Model) {
			return m.refuse(notFound(e.Model))
		}
		if st.Paused {
			return m.refuse(ErrPaused)
		}
		if st.Current == e.Model && (st.Phase == PhaseLoading || st.serving()) { // C11
			m.reason = "restart"
			delete(st.Failed, e.Model)
			st.Intent = &Intent{Kind: IntentRestart, Model: e.Model}
			m.log(slog.LevelInfo, "explicit restart", slog.String("model", e.Model), slog.String("phase", st.Phase.String()))
			if st.Phase == PhaseDraining {
				st.Target = e.Model
			}
		} else { // C12 — как C4
			m.reason = "explicit_load"
			m.applyLoad(e.Model)
		}
		m.result(nil)
	case evPause: // C13
		m.reason = "pause"
		st.Paused = true
		for _, w := range st.Queue {
			m.emit(effReject{ID: w.ID, Err: ErrPaused})
		}
		st.Queue = nil
		st.Intent = &Intent{Kind: IntentUnload}
		if st.Phase == PhaseDraining {
			st.Target = ""
		}
		m.log(slog.LevelInfo, "paused", slog.Bool("force", e.Force), slog.String("phase", st.Phase.String()))
		if e.Force && st.serving() {
			m.forceRetire()
		}
		m.result(nil)
	case evResume: // C14: Intent не меняется — начатая выгрузка доводится до конца
		m.reason = "resume"
		st.Paused = false
		m.log(slog.LevelInfo, "resumed", slog.String("phase", st.Phase.String()))
		m.result(nil)
	case evReconfigure: // C15g, C15
		removed := m.removedNames(e.Catalog)
		if e.Guard {
			if busy := m.busy(removed); len(busy) > 0 { // RC0
				return m.refuse(&InUseError{Models: busy})
			}
		}
		m.reconfigure(e.Catalog, removed)
		m.result(nil)
	case evBaseline: // E6
		if e.Gen != st.Gen {
			return m.stale(ev, e.Gen)
		}
		st.Baseline = e.MiB
		m.log(slog.LevelInfo, "vram baseline", slog.String("model", st.Current), slog.Uint64("gen", st.Gen),
			slog.Int64("baseline_mib", e.MiB))
	case evLoadOK: // E7
		if e.Gen != st.Gen || st.Phase != PhaseLoading {
			return m.stale(ev, e.Gen)
		}
		st.Upstream, st.LastActivity = e.Upstream, m.now
		if e.Upstream != nil {
			st.PID = e.Upstream.PID
		}
		delete(st.Failed, st.Current) // истёкшая метка
		m.setPhase(PhaseReady, st.Current, st.Current)
		m.log(slog.LevelInfo, "model ready", slog.String("model", st.Current), slog.Int("pid", st.PID),
			slog.Uint64("gen", st.Gen), slog.Int64("load_ms", m.now.Sub(st.StartedAt).Milliseconds()))
	case evLoadFailed: // E8
		if e.Gen != st.Gen || st.Phase != PhaseLoading {
			return m.stale(ev, e.Gen)
		}
		m.reason = "load_failed"
		m.processGone(e.ExitCode, e.Message)
	case evProcessExited: // E9
		if e.Gen != st.Gen || !st.serving() {
			return m.stale(ev, e.Gen)
		}
		m.emit(effRetireUpstream{Gen: st.Gen, Upstream: st.Upstream, Reason: RetireExited}) // первым, до остальных эффектов
		st.Upstream, st.Inflight = nil, 0
		m.reason = "process_exit"
		m.processGone(e.ExitCode, e.Message)
	case evStopped: // E10
		if e.Gen != st.Gen || st.Phase != PhaseStopping {
			return m.stale(ev, e.Gen)
		}
		model := st.Current
		st.Current, st.PID, st.Running = "", 0, config.Model{}
		st.Target = m.nextTarget()
		m.setPhase(PhaseWaitVRAM, model, st.Target)
		m.emit(effWaitVRAM{Gen: st.Gen, Baseline: st.Baseline})
	case evVRAMFreed: // E11
		if e.Gen != st.Gen || st.Phase != PhaseWaitVRAM {
			return m.stale(ev, e.Gen)
		}
		st.LastWait = &VRAMWait{Waited: e.Waited, LastMiB: e.LastMiB, TimedOut: e.TimedOut, Unknown: e.Unknown}
		level := slog.LevelInfo
		if e.TimedOut || e.Unknown {
			level = slog.LevelWarn
		}
		m.log(level, "vram wait finished", slog.Uint64("gen", st.Gen), slog.Int64("waited_ms", e.Waited.Milliseconds()),
			slog.Int64("usage_mib", e.LastMiB), slog.Int64("baseline_mib", st.Baseline),
			slog.Bool("timed_out", e.TimedOut), slog.Bool("unknown", e.Unknown))
		target := st.Target
		st.Target = ""
		m.setPhase(PhaseEmpty, "", target)
	case evTimer: // E12
		m.reason = "cooldown_timer"
	case evStopFailed: // E14
		if e.Gen != st.Gen || (st.Phase != PhaseLoading && st.Phase != PhaseStopping) {
			return m.stale(ev, e.Gen)
		}
		st.Halted = true
		m.log(slog.LevelError, "llama-server exit not confirmed; host is exiting", slog.String("model", st.Current),
			slog.Int("pid", e.PID), slog.Uint64("gen", st.Gen), slog.String("err", e.Message))
		m.emit(effFatal{Err: fmt.Errorf("%w: model %q, pid %d, gen %d", ErrStopUnconfirmed, st.Current, e.PID, st.Gen)})
	default:
		panic(fmt.Sprintf("scheduler: unknown event %T", ev))
	}
	return true
}

// acquire — правила A0, A1, A1p (§6.3 v2) и A1a–A6 MVP. LoadedOnly (GET /props, F5-D1) — вне этих правил:
// Lease только на модель в Ready (Model "" — на Current), иначе сразу отказ; без ожидающего и намерения.
func (m *machine) acquire(e evAcquire) {
	st := &m.st
	if e.LoadedOnly {
		switch {
		case st.Paused: // A0
			m.emit(effReject{ID: e.ID, Err: ErrPaused})
		case st.Phase == PhaseReady && (e.Model == "" || e.Model == st.Current):
			m.grant(Waiter{ID: e.ID, Model: st.Current, Since: m.now})
		default:
			m.emit(effReject{ID: e.ID, Err: ErrModelNotLoaded})
		}
		return
	}
	if !m.inCatalog(e.Model) { // A1
		m.emit(effReject{ID: e.ID, Err: notFound(e.Model)})
		return
	}
	if st.Paused { // A1p: и для явного Acquire, A1a не применяется
		m.emit(effReject{ID: e.ID, Err: ErrPaused})
		return
	}
	if e.Explicit { // A1a: намерение и ожидающий — в одном шаге
		m.reason = "explicit_load"
		m.applyLoad(e.Model)
	} else if f, ok := st.Failed[e.Model]; ok && f.Active(m.now) { // A2
		m.emit(effReject{ID: e.ID, Err: &LoadFailedError{Model: e.Model, ExitCode: f.ExitCode, Message: f.Message,
			Cached: true, RetryIn: f.Until.Sub(m.now)}})
		return
	}
	w := Waiter{ID: e.ID, Model: e.Model, Explicit: e.Explicit, Since: m.now}
	if st.Phase == PhaseReady && st.Current == e.Model { // A3
		m.grant(w)
		return
	}
	if !e.Explicit {
		if st.Phase == PhaseReady && m.hot() && !m.queued(e.Model) { // A4
			m.emit(effReject{ID: e.ID, Err: &CooldownError{Loaded: st.Current, Requested: e.Model, RetryAfter: m.retryAfter()}})
			return
		}
		if m.implicitLen() >= m.p.MaxQueue { // A5
			m.emit(effReject{ID: e.ID, Err: &QueueFullError{MaxQueue: m.p.MaxQueue}})
			return
		}
	}
	st.Queue = append(st.Queue, w) // A6
}

// applyLoad — изменения E3 (C4, C12 и A1a).
func (m *machine) applyLoad(model string) {
	st := &m.st
	delete(st.Failed, model)
	st.Intent = &Intent{Kind: IntentLoad, Model: model}
	m.log(slog.LevelInfo, "explicit load", slog.String("model", model), slog.String("phase", st.Phase.String()))
	if st.Phase != PhaseDraining {
		return
	}
	if st.Current == model { // владелец сказал «оставь model»; отменяет и перезапуск
		st.Target = ""
		m.setPhase(PhaseReady, model, model)
		return
	}
	st.Target = model
}

// applyUnload — E4 (чужая модель: снимает Load и Restart этого имени) и E5 (всё или Current).
func (m *machine) applyUnload(model string) {
	st := &m.st
	delete(st.Failed, model)
	if model != "" && model != st.Current { // E4
		if st.Intent != nil && st.Intent.Kind != IntentUnload && st.Intent.Model == model {
			st.Intent = nil
		}
		return
	}
	st.Intent = &Intent{Kind: IntentUnload, Model: model} // E5
	m.log(slog.LevelInfo, "explicit unload", slog.String("model", model), slog.String("phase", st.Phase.String()))
	if st.Phase == PhaseDraining {
		st.Target = ""
	}
}

// forceRetire — F1 (§6.4 v2): только из C8 и C13 и только в Ready и Draining.
func (m *machine) forceRetire() {
	st := &m.st
	aborted := st.Inflight
	m.emit(effRetireUpstream{Gen: st.Gen, Upstream: st.Upstream, Reason: RetireForced})
	st.Upstream, st.Inflight, st.Target = nil, 0, ""
	m.reason = "forced_unload"
	m.setPhase(PhaseStopping, st.Current, "")
	m.emit(effStopModel{Gen: st.Gen})
	m.log(slog.LevelWarn, "forced unload", slog.String("model", st.Current), slog.Uint64("gen", st.Gen),
		slog.Int("aborted", aborted))
}

// removedNames — R (§6.7.4): имена Catalog, которых нет в next, в порядке Catalog.
func (m *machine) removedNames(next []config.Model) []string {
	var r []string
	for _, d := range m.st.Catalog {
		if _, ok := findModel(next, d.ID); !ok {
			r = append(r, d.ID)
		}
	}
	return r
}

// busy — R ∩ Busy в порядке Catalog (RC0). Busy: Current в Loading, Ready, Draining; Target; модель Intent;
// модели ожидающих.
func (m *machine) busy(removed []string) []string {
	st := &m.st
	var out []string
	for _, r := range removed {
		switch {
		case r == st.Current && (st.Phase == PhaseLoading || st.serving()),
			r == st.Target,
			st.Intent != nil && r == st.Intent.Model,
			m.queued(r):
			out = append(out, r)
		}
	}
	return out
}

// reconfigure — RC1–RC5 (§6.7.4).
func (m *machine) reconfigure(next []config.Model, removed []string) {
	st := &m.st
	m.reason = "reconfigure"
	gone := func(id string) bool { return slices.Contains(removed, id) }
	kept := make([]Waiter, 0, len(st.Queue))
	for _, w := range st.Queue { // RC1
		if gone(w.Model) {
			m.emit(effReject{ID: w.ID, Err: notFound(w.Model)})
			continue
		}
		kept = append(kept, w)
	}
	st.Queue = kept
	for _, r := range removed { // RC2
		delete(st.Failed, r)
	}
	if st.Intent != nil && st.Intent.Kind != IntentUnload && gone(st.Intent.Model) { // RC3
		st.Intent = nil
	}
	if gone(st.Current) && (st.Phase == PhaseLoading || st.serving()) && st.Intent == nil { // RC4
		st.Intent = &Intent{Kind: IntentUnload, Model: st.Current}
		if st.Phase == PhaseDraining {
			st.Target = ""
		}
	}
	added, changed := 0, 0
	for _, d := range next {
		if old, ok := findModel(st.Catalog, d.ID); !ok {
			added++
		} else if !old.SameLaunch(d) {
			changed++
		}
	}
	st.Catalog = next // RC5; Running не меняется
	m.log(slog.LevelInfo, "catalog updated", slog.Int("added", added), slog.Int("removed", len(removed)),
		slog.Int("changed", changed), slog.Bool("restart_required", restartRequired(st.Phase, st.Current, st.Running, next)))
}

// processGone — общая часть E8 и E9: метка failed (если модель ещё в каталоге), отказ ожидающим Current,
// снятие Load(Current) и Restart(Current), переход в WaitVRAM.
func (m *machine) processGone(code int, msg string) {
	st := &m.st
	model := st.Current
	if m.inCatalog(model) {
		st.Failed[model] = FailInfo{ExitCode: code, Message: msg, At: m.now, Until: m.now.Add(m.p.FailedTTL)}
	}
	kept := make([]Waiter, 0, len(st.Queue))
	for _, w := range st.Queue {
		if w.Model != model {
			kept = append(kept, w)
			continue
		}
		m.emit(effReject{ID: w.ID, Err: &LoadFailedError{Model: model, ExitCode: code, Message: msg}})
	}
	st.Queue = kept
	if st.Intent != nil && st.Intent.Kind != IntentUnload && st.Intent.Model == model {
		st.Intent = nil
	}
	st.Current, st.PID, st.Running = "", 0, config.Model{}
	st.Target = m.nextTarget()
	m.setPhase(PhaseWaitVRAM, model, st.Target)
	m.emit(effWaitVRAM{Gen: st.Gen, Baseline: st.Baseline})
	m.log(slog.LevelError, "model failed", slog.String("model", model), slog.Uint64("gen", st.Gen),
		slog.Int("exit_code", code), slog.String("tail", msg))
}

func (m *machine) grant(w Waiter) {
	m.st.Inflight++
	m.emit(effGrant{ID: w.ID, Model: w.Model, Gen: m.st.Gen, Upstream: m.st.Upstream, Waited: m.now.Sub(w.Since)})
}

func (m *machine) removeWaiter(id uint64) {
	for i, w := range m.st.Queue {
		if w.ID == id {
			m.st.Queue = append(m.st.Queue[:i:i], m.st.Queue[i+1:]...)
			return
		}
	}
}

// hot — §6.1.
func (m *machine) hot() bool {
	st := &m.st
	return m.p.SwapCooldown > 0 && st.Phase == PhaseReady &&
		(st.Inflight > 0 || m.now.Before(st.LastActivity.Add(m.p.SwapCooldown)))
}

// retryAfter — §6.4: при Inflight > 0 полный cooldown, иначе остаток; не меньше 1 с.
func (m *machine) retryAfter() int {
	if m.st.Inflight > 0 {
		return max(1, ceilSeconds(m.p.SwapCooldown))
	}
	return max(1, ceilSeconds(m.st.LastActivity.Add(m.p.SwapCooldown).Sub(m.now)))
}

// nextTarget — §6.1 v2: модель намерения Load или Restart; иначе первый ожидающий; иначе "".
func (m *machine) nextTarget() string {
	if m.st.Intent != nil && m.st.Intent.Kind != IntentUnload {
		return m.st.Intent.Model
	}
	if len(m.st.Queue) > 0 {
		return m.st.Queue[0].Model
	}
	return ""
}

func (m *machine) implicitLen() int {
	n := 0
	for _, w := range m.st.Queue {
		if !w.Explicit {
			n++
		}
	}
	return n
}

func (m *machine) queued(model string) bool {
	for _, w := range m.st.Queue {
		if w.Model == model {
			return true
		}
	}
	return false
}

// pump — продвижение §6.6 до неподвижной точки.
func (m *machine) pump() {
	for i := 0; m.pumpOnce(); i++ {
		if i > 100 {
			panic("scheduler: pump did not reach a fixed point")
		}
	}
}

// pumpOnce применяет первое подходящее правило P1–P11 (в Ready: P1, P2, P3, P3r, P4, P5, P6);
// true — состояние изменилось.
func (m *machine) pumpOnce() bool {
	st := &m.st
	switch st.Phase {
	case PhaseReady:
		granted := false // P1
		kept := make([]Waiter, 0, len(st.Queue))
		for _, w := range st.Queue {
			if w.Model == st.Current {
				m.grant(w)
				granted = true
				continue
			}
			kept = append(kept, w)
		}
		if granted {
			st.Queue = kept
			return true
		}
		if in := st.Intent; in != nil && in.Kind != IntentUnload {
			switch {
			case in.Kind == IntentLoad && in.Model == st.Current: // P2
				st.Intent = nil
			case in.Model != st.Current: // P3: Load(m) или Restart(m), m ≠ Current
				st.Target = in.Model
				m.setPhase(PhaseDraining, st.Current, st.Target)
			default: // P3r: Restart(Current)
				st.Target = st.Current
				m.reason = "restart"
				m.setPhase(PhaseDraining, st.Current, st.Target)
			}
			return true
		}
		if st.Intent != nil { // P4: только IntentUnload
			st.Target = ""
			m.setPhase(PhaseDraining, st.Current, "")
			return true
		}
		if len(st.Queue) > 0 && !m.hot() { // P5
			st.Target = st.Queue[0].Model
			m.setPhase(PhaseDraining, st.Current, st.Target)
			return true
		}
		if len(st.Queue) > 0 && st.Inflight == 0 { // P6 (hot): состояние не меняется
			m.emit(effSetTimer{Kind: TimerCooldown, At: st.LastActivity.Add(m.p.SwapCooldown)})
		}
	case PhaseDraining:
		if st.Intent == nil && !m.queued(st.Target) { // P7 — раньше P8
			st.Target = ""
			m.reason = "cancel"
			m.setPhase(PhaseReady, st.Current, st.Current)
			return true
		}
		if st.Inflight == 0 { // P8
			up := st.Upstream
			st.Upstream = nil
			m.setPhase(PhaseStopping, st.Current, st.Target)
			m.emit(effRetireUpstream{Gen: st.Gen, Upstream: up, Reason: RetireStop})
			m.emit(effStopModel{Gen: st.Gen})
			return true
		}
	case PhaseStopping, PhaseWaitVRAM: // P9
		if t := m.nextTarget(); t != st.Target {
			st.Target = t
			return true
		}
	case PhaseEmpty:
		if st.Intent != nil && st.Intent.Kind == IntentUnload { // P10
			st.Intent = nil
			return true
		}
		if t := m.nextTarget(); t != "" && !st.Paused { // P11
			def, ok := findModel(st.Catalog, t)
			if !ok { // недостижимо по I15
				panic(fmt.Sprintf("scheduler: next target %q is not in the catalog", t))
			}
			st.Gen++
			st.Current, st.Target, st.Baseline, st.PID, st.StartedAt, st.Running = t, "", -1, 0, m.now, def
			if st.Intent != nil && *st.Intent == (Intent{Kind: IntentRestart, Model: t}) {
				st.Intent = &Intent{Kind: IntentLoad, Model: t} // его снимет P2 после Ready
			}
			m.setPhase(PhaseLoading, "", t)
			m.emit(effStartModel{Gen: st.Gen, Model: def})
			return true
		}
	}
	return false
}
