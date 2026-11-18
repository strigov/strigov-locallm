package scheduler

import (
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func testParams() Params {
	return Params{SwapCooldown: 30 * time.Second, MaxQueue: 8, FailedTTL: 120 * time.Second}
}

// mdl — определение модели стенда: C:\models\<id>.gguf с -c 4096.
func mdl(id string) config.Model { return mdlCtx(id, 4096) }

// mdlCtx — то же определение с -c n (другое определение того же имени).
func mdlCtx(id string, n int) config.Model {
	return config.Model{ID: id, Base: id, Path: `C:\models\` + id + `.gguf`, Args: []string{"-c", strconv.Itoa(n)}, ContextLength: n}
}

func testCatalog(ids ...string) []config.Model {
	out := make([]config.Model, len(ids))
	for i, id := range ids {
		out[i] = mdl(id)
	}
	return out
}

// isStart — StartModel поколения gen с определением mdl(id).
func isStart(s effStartModel, gen uint64, id string) bool {
	return s.Gen == gen && reflect.DeepEqual(s.Model, mdl(id))
}

type harness struct {
	t    *testing.T
	p    Params
	st   State
	now  time.Time
	effs []Effect // эффекты последнего шага
	all  []Effect // эффекты всех шагов
}

func newHarness(t *testing.T, p Params) *harness {
	return &harness{t: t, p: p, st: initialState(t0, testCatalog("a", "b", "c"), false), now: t0}
}

func (h *harness) at(d time.Duration) { h.now = t0.Add(d) }

// do выполняет один шаг и проверяет I11, I13, I15 и I18.
func (h *harness) do(ev Event) []Effect {
	h.t.Helper()
	in, before := h.st, h.st.clone()
	before.Catalog = cloneCatalog(before.Catalog)
	before.Running.Args = slices.Clone(before.Running.Args)
	next, effs := step(in, ev, h.now, h.p)
	if !reflect.DeepEqual(in, before) {
		h.t.Fatalf("I11: step mutated its input on %T", ev)
	}
	if v := v2Violation(next); v != "" {
		h.t.Fatalf("%s after %T", v, ev)
	}
	h.st, h.effs = next, effs
	h.all = append(h.all, effs...)
	return effs
}

func (h *harness) up(gen uint64, model string) *Upstream {
	return &Upstream{Model: model, PID: 1000 + int(gen)}
}

// ready доводит автомат из Empty до Ready(model): Load → Baseline 900 → LoadOK.
func (h *harness) ready(model string) {
	h.t.Helper()
	h.do(evLoad{Model: model})
	g := h.st.Gen
	h.do(evBaseline{Gen: g, MiB: 900})
	h.do(evLoadOK{Gen: g, Upstream: h.up(g, model)})
	h.mustState(PhaseReady, model, "")
	if h.st.Intent != nil {
		h.t.Fatalf("intent %+v left after ready", *h.st.Intent)
	}
}

func (h *harness) mustState(ph Phase, cur, target string) {
	h.t.Helper()
	if h.st.Phase != ph || h.st.Current != cur || h.st.Target != target {
		h.t.Fatalf("state %s cur=%q target=%q, want %s cur=%q target=%q",
			h.st.Phase, h.st.Current, h.st.Target, ph, cur, target)
	}
}

func acq(id uint64, model string) evAcquire  { return evAcquire{ID: id, Model: model} }
func xacq(id uint64, model string) evAcquire { return evAcquire{ID: id, Model: model, Explicit: true} }

func ofType[T Effect](effs []Effect) []T {
	var out []T
	for _, e := range effs {
		if v, ok := e.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func indexOf[T Effect](effs []Effect) int {
	for i, e := range effs {
		if _, ok := e.(T); ok {
			return i
		}
	}
	return -1
}

func grantIDs(effs []Effect) []uint64 {
	var ids []uint64
	for _, g := range ofType[effGrant](effs) {
		ids = append(ids, g.ID)
	}
	return ids
}

func rejectOf[E error](t *testing.T, effs []Effect, id uint64) E {
	t.Helper()
	for _, r := range ofType[effReject](effs) {
		if r.ID != id {
			continue
		}
		var target E
		if !errors.As(r.Err, &target) {
			t.Fatalf("reject %d: unexpected error %T: %v", id, r.Err, r.Err)
		}
		return target
	}
	t.Fatalf("no reject for waiter %d in %+v", id, effs)
	var zero E
	return zero
}

func hasLog(effs []Effect, level slog.Level, msg string) bool {
	for _, l := range ofType[effLog](effs) {
		if l.Level == level && l.Msg == msg {
			return true
		}
	}
	return false
}

// phaseLog — индекс строки журнала о переходе в фазу ph и её reason; -1, если перехода не было.
func phaseLog(effs []Effect, ph Phase) (int, string) {
	for i, e := range effs {
		l, ok := e.(effLog)
		if !ok || l.Msg != "phase" {
			continue
		}
		var phase, reason string
		for _, a := range l.Attrs {
			switch a.Key {
			case "phase":
				phase = a.Value.String()
			case "reason":
				reason = a.Value.String()
			}
		}
		if phase == ph.String() {
			return i, reason
		}
	}
	return -1, ""
}

func TestT01EmptyAcquireLoadsAndGrants(t *testing.T) {
	h := newHarness(t, testParams())
	effs := h.do(acq(1, "a"))
	h.mustState(PhaseLoading, "a", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 1, "a") {
		t.Fatalf("start %+v", s)
	}
	h.do(evBaseline{Gen: 1, MiB: 900})
	h.at(5 * time.Second)
	effs = h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	h.mustState(PhaseReady, "a", "")
	g := ofType[effGrant](effs)
	if len(g) != 1 || g[0].ID != 1 || g[0].Gen != 1 || g[0].Upstream == nil || g[0].Waited != 5*time.Second {
		t.Fatalf("grant %+v", g)
	}
	if h.st.Inflight != 1 || len(h.st.Queue) != 0 || h.st.PID != 1001 || h.st.Baseline != 900 {
		t.Fatalf("state %+v", h.st)
	}
}

func TestT02ReadyAcquireGrantsImmediately(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	effs := h.do(acq(1, "a"))
	if !slices.Equal(grantIDs(effs), []uint64{1}) || h.st.Inflight != 1 || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("effects %+v state %+v", effs, h.st)
	}
}

func TestT03HotModelRejectsWithRetryAfter(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a") // LastActivity = t0
	h.at(10500 * time.Millisecond)
	effs := h.do(acq(1, "b"))
	e := rejectOf[*CooldownError](t, effs, 1)
	if e.RetryAfter != 20 || e.Loaded != "a" || e.Requested != "b" {
		t.Fatalf("cooldown %+v", e)
	}
	if e.Error() != `model "a" is in use; switching to "b" is blocked for 20 s (swap_cooldown)` {
		t.Fatalf("message %q", e.Error())
	}
	h.mustState(PhaseReady, "a", "")
	if len(h.st.Queue) != 0 || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("queue %+v", h.st.Queue)
	}
	h.do(acq(2, "a")) // Inflight 1: горячая по Inflight
	h.at(10 * time.Minute)
	if e := rejectOf[*CooldownError](t, h.do(acq(3, "b")), 3); e.RetryAfter != 30 {
		t.Fatalf("retry after with inflight: %d", e.RetryAfter)
	}
}

func TestT04ColdModelSwapsThroughAllPhases(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	up := h.st.Upstream
	h.at(31 * time.Second)
	effs := h.do(acq(1, "b"))
	h.mustState(PhaseStopping, "a", "b")
	ri, si := indexOf[effRetireUpstream](effs), indexOf[effStopModel](effs)
	if ri < 0 || si < 0 || ri > si {
		t.Fatalf("retire %d stop %d in %+v", ri, si, effs)
	}
	if r := effs[ri].(effRetireUpstream); r.Gen != 1 || r.Upstream != up || r.Reason != RetireStop {
		t.Fatalf("retire %+v", r)
	}
	if effs[si].(effStopModel).Gen != 1 || h.st.Upstream != nil {
		t.Fatalf("stop %+v upstream %v", effs[si], h.st.Upstream)
	}
	h.at(32 * time.Second)
	effs = h.do(evStopped{Gen: 1})
	h.mustState(PhaseWaitVRAM, "", "b")
	if w := ofType[effWaitVRAM](effs); len(w) != 1 || w[0] != (effWaitVRAM{Gen: 1, Baseline: 900}) {
		t.Fatalf("wait %+v", w)
	}
	h.at(33 * time.Second)
	effs = h.do(evVRAMFreed{Gen: 1, Waited: time.Second, LastMiB: 950})
	h.mustState(PhaseLoading, "b", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "b") {
		t.Fatalf("start %+v", s)
	}
	h.at(40 * time.Second)
	h.do(evBaseline{Gen: 2, MiB: 900})
	effs = h.do(evLoadOK{Gen: 2, Upstream: h.up(2, "b")})
	g := ofType[effGrant](effs)
	if len(g) != 1 || g[0].ID != 1 || g[0].Gen != 2 || g[0].Waited != 9*time.Second {
		t.Fatalf("grant %+v", g)
	}
}

func TestT05DrainingQueuesCurrentBehindTarget(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.at(time.Second)
	h.do(xacq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	effs := h.do(acq(3, "a"))
	if len(ofType[effGrant](effs)) != 0 || len(h.st.Queue) != 2 || h.st.Queue[0].ID != 2 || h.st.Queue[1].ID != 3 {
		t.Fatalf("queue %+v effects %+v", h.st.Queue, effs)
	}
	h.at(2 * time.Second)
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "b")
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseLoading, "b", "")
	h.at(10 * time.Second)
	effs = h.do(evLoadOK{Gen: 2, Upstream: h.up(2, "b")})
	h.mustState(PhaseReady, "b", "")
	if !slices.Equal(grantIDs(effs), []uint64{2}) || h.st.Intent != nil || len(h.st.Queue) != 1 {
		t.Fatalf("grants %v intent %v queue %+v", grantIDs(effs), h.st.Intent, h.st.Queue)
	}
	h.at(11 * time.Second)
	effs = h.do(evRelease{Gen: 2})
	tm := ofType[effSetTimer](effs)
	if len(tm) != 1 || tm[0] != (effSetTimer{Kind: TimerCooldown, At: t0.Add(41 * time.Second)}) {
		t.Fatalf("timer %+v", tm)
	}
	h.mustState(PhaseReady, "b", "")
	h.at(41 * time.Second)
	effs = h.do(evTimer{Kind: TimerCooldown})
	h.mustState(PhaseStopping, "b", "a")
	if _, reason := phaseLog(effs, PhaseDraining); reason != "cooldown_timer" {
		t.Fatalf("reason %q", reason)
	}
}

func TestT06DrainingCancelsWhenTargetWaitersLeave(t *testing.T) {
	p := testParams()
	p.SwapCooldown = 0
	h := newHarness(t, p)
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	h.do(acq(3, "a"))
	effs := h.do(evWaiterGone{ID: 2})
	h.mustState(PhaseReady, "a", "")
	if !slices.Equal(grantIDs(effs), []uint64{3}) || h.st.Inflight != 2 || len(h.st.Queue) != 0 {
		t.Fatalf("grants %v inflight %d queue %+v", grantIDs(effs), h.st.Inflight, h.st.Queue)
	}
	if _, reason := phaseLog(effs, PhaseReady); reason != "cancel" {
		t.Fatalf("reason %q", reason)
	}
}

func TestT07ExplicitDrainingSurvivesWaiterGone(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(xacq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	h.do(evWaiterGone{ID: 2})
	h.mustState(PhaseDraining, "a", "b")
	if h.st.Intent == nil || *h.st.Intent != (Intent{Kind: IntentLoad, Model: "b"}) || len(h.st.Queue) != 0 {
		t.Fatalf("intent %v queue %+v", h.st.Intent, h.st.Queue)
	}
}

func TestT08StoppingReachesEmptyAfterWaiterLeaves(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.at(31 * time.Second)
	h.do(acq(1, "b"))
	h.mustState(PhaseStopping, "a", "b")
	h.do(evWaiterGone{ID: 1})
	h.mustState(PhaseStopping, "a", "")
	h.do(evStopped{Gen: 1})
	effs := h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	if len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("unexpected start %+v", effs)
	}

	h = newHarness(t, testParams())
	h.ready("a")
	h.at(31 * time.Second)
	h.do(acq(1, "b"))
	h.do(acq(2, "c"))
	h.do(evWaiterGone{ID: 1})
	h.mustState(PhaseStopping, "a", "c")
	h.do(evStopped{Gen: 1})
	effs = h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseLoading, "c", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "c") {
		t.Fatalf("start %+v", s)
	}
}

func TestT09LoadFailedIsStickyForFailedTTL(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(acq(2, "a"))
	h.at(5 * time.Second)
	effs := h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "error: out of memory"})
	for _, id := range []uint64{1, 2} {
		e := rejectOf[*LoadFailedError](t, effs, id)
		if e.Cached || e.ExitCode != 1 || e.Error() != `model "a" failed to load (exit code 1): error: out of memory` {
			t.Fatalf("reject %d: %+v %q", id, e, e.Error())
		}
	}
	h.mustState(PhaseWaitVRAM, "", "")
	if f := h.st.Failed["a"]; f.Until != t0.Add(125*time.Second) || f.ExitCode != 1 || h.st.Intent != nil {
		t.Fatalf("failed %+v intent %v", f, h.st.Intent)
	}
	if !hasLog(effs, slog.LevelError, "model failed") || len(ofType[effWaitVRAM](effs)) != 1 {
		t.Fatalf("effects %+v", effs)
	}
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	h.at(65 * time.Second)
	effs = h.do(acq(3, "a"))
	e := rejectOf[*LoadFailedError](t, effs, 3)
	if !e.Cached || e.RetryIn != 60*time.Second ||
		e.Error() != `model "a" failed to load (exit code 1): error: out of memory; cached for 60 s, load it explicitly to retry` {
		t.Fatalf("cached %+v %q", e, e.Error())
	}
	if len(ofType[effStartModel](effs)) != 0 || len(h.st.Queue) != 0 {
		t.Fatalf("cached reject must not load: %+v", effs)
	}
	h.at(126 * time.Second)
	effs = h.do(acq(4, "a"))
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "a") {
		t.Fatalf("start after ttl %+v", s)
	}
}

// failA — Empty с меткой failed на a (Until = t0 + 120 s).
func failA(t *testing.T) *harness {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	if !h.st.Failed["a"].Active(h.now) {
		t.Fatal("mark not set")
	}
	return h
}

func TestT10LoadAndUnloadClearFailedMark(t *testing.T) {
	h := failA(t)
	effs := h.do(evLoad{Model: "a"})
	if _, ok := h.st.Failed["a"]; ok {
		t.Fatal("Load must clear the mark")
	}
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "a") {
		t.Fatalf("start %+v", s)
	}
	h = failA(t)
	effs = h.do(evUnload{Model: "a"})
	if _, ok := h.st.Failed["a"]; ok || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("Unload must clear the mark without loading: %+v", effs)
	}
	h.mustState(PhaseEmpty, "", "")
}

func TestT11ExplicitLoadIgnoresCooldown(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.at(time.Second)
	effs := h.do(evLoad{Model: "b"})
	h.mustState(PhaseDraining, "a", "b")
	if _, reason := phaseLog(effs, PhaseDraining); reason != "explicit_load" {
		t.Fatalf("reason %q", reason)
	}
}

func TestT12LoadDuringLoadingWaitsForReady(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(evLoad{Model: "b"})
	h.mustState(PhaseLoading, "a", "")
	if h.st.Intent == nil || *h.st.Intent != (Intent{Kind: IntentLoad, Model: "b"}) {
		t.Fatalf("intent %v", h.st.Intent)
	}
	effs := h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	h.mustState(PhaseDraining, "a", "b")
	gi := indexOf[effGrant](effs)
	di, _ := phaseLog(effs, PhaseDraining)
	if !slices.Equal(grantIDs(effs), []uint64{1}) || gi > di {
		t.Fatalf("grant must precede draining: %+v", effs)
	}
}

func TestT13LastExplicitLoadWins(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(evLoad{Model: "b"})
	h.do(evLoad{Model: "c"})
	h.mustState(PhaseDraining, "a", "c")
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "c")
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	var models []string
	for _, s := range ofType[effStartModel](h.all) {
		models = append(models, s.Model.ID)
	}
	if !slices.Equal(models, []string{"a", "c"}) {
		t.Fatalf("started %v, want [a c]", models)
	}
}

func TestT14UnloadAllDrainsThenServesQueue(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(evUnload{Model: ""})
	h.mustState(PhaseDraining, "a", "")
	h.do(acq(2, "b"))
	h.mustState(PhaseDraining, "a", "")
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "b")
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseLoading, "b", "")
	if h.st.Intent != nil {
		t.Fatalf("intent %v", h.st.Intent)
	}

	h = newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(evUnload{Model: ""})
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "")
	h.do(evStopped{Gen: 1})
	effs := h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	if h.st.Intent != nil || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("intent %v effects %+v", h.st.Intent, effs)
	}
}

func TestT15QueueFullRejectsImplicitOnly(t *testing.T) {
	p := testParams()
	p.MaxQueue = 2
	h := newHarness(t, p)
	h.do(acq(1, "a"))
	h.do(acq(2, "a"))
	e := rejectOf[*QueueFullError](t, h.do(acq(3, "b")), 3)
	if e.Error() != "swap queue is full (2 waiting requests)" {
		t.Fatalf("message %q", e.Error())
	}
	effs := h.do(xacq(4, "b"))
	if len(ofType[effReject](effs)) != 0 || len(h.st.Queue) != 3 {
		t.Fatalf("explicit waiter must pass: %+v queue %+v", effs, h.st.Queue)
	}
}

func TestT16VRAMTimeoutWarnsAndStartsNext(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.at(31 * time.Second)
	h.do(acq(1, "b"))
	h.do(evStopped{Gen: 1})
	effs := h.do(evVRAMFreed{Gen: 1, Waited: 10 * time.Second, LastMiB: 5000, TimedOut: true})
	if !hasLog(effs, slog.LevelWarn, "vram wait finished") {
		t.Fatalf("no warn in %+v", effs)
	}
	if s := ofType[effStartModel](effs); len(s) != 1 || s[0].Model.ID != "b" {
		t.Fatalf("start %+v", s)
	}
	if h.st.LastWait == nil || !h.st.LastWait.TimedOut || h.st.LastWait.LastMiB != 5000 {
		t.Fatalf("last wait %+v", h.st.LastWait)
	}
}

func TestT17ProcessExitRetiresFirst(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "a"))
	up := h.st.Upstream
	effs := h.do(evProcessExited{Gen: 1, ExitCode: 3, Message: "crash"})
	if r, ok := effs[0].(effRetireUpstream); !ok || r.Gen != 1 || r.Upstream != up || r.Reason != RetireExited {
		t.Fatalf("first effect %+v", effs[0])
	}
	h.mustState(PhaseWaitVRAM, "", "")
	if h.st.Inflight != 0 || h.st.Upstream != nil || !h.st.Failed["a"].Active(h.now) || len(ofType[effWaitVRAM](effs)) != 1 {
		t.Fatalf("state %+v effects %+v", h.st, effs)
	}
	before := h.st.clone()
	effs = h.do(evRelease{Gen: 1})
	if !reflect.DeepEqual(h.st, before) || len(effs) != 1 || !hasLog(effs, slog.LevelDebug, "stale event") {
		t.Fatalf("stale release changed state or effects %+v", effs)
	}
}

func TestT18StaleEventsChangeNothing(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	before := h.st.clone()
	stale := []Event{
		evRelease{Gen: 9}, evBaseline{Gen: 9, MiB: 1}, evLoadOK{Gen: 9}, evLoadFailed{Gen: 9, ExitCode: 1, Message: "x"},
		evProcessExited{Gen: 9, ExitCode: 1, Message: "x"}, evStopped{Gen: 9}, evVRAMFreed{Gen: 9},
		evStopFailed{Gen: 9, PID: 1, Message: "x"},
		// своё поколение, но не своя фаза (Ready):
		evLoadOK{Gen: 1}, evLoadFailed{Gen: 1, ExitCode: 1, Message: "x"}, evStopped{Gen: 1}, evVRAMFreed{Gen: 1},
		evStopFailed{Gen: 1, PID: 1, Message: "x"},
	}
	for _, ev := range stale {
		effs := h.do(ev)
		if !reflect.DeepEqual(h.st, before) {
			t.Fatalf("%T %+v changed state", ev, ev)
		}
		if len(effs) != 1 || !hasLog(effs, slog.LevelDebug, "stale event") {
			t.Fatalf("%T %+v effects %+v", ev, ev, effs)
		}
	}
}

func TestT19AllWaitersOfTargetGrantedTogether(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "b"))
	h.do(acq(2, "b"))
	h.do(acq(3, "b"))
	effs := h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "b")})
	if !slices.Equal(grantIDs(effs), []uint64{1, 2, 3}) || h.st.Inflight != 3 || len(h.st.Queue) != 0 {
		t.Fatalf("grants %v inflight %d", grantIDs(effs), h.st.Inflight)
	}
}

func TestT20QueuedModelBypassesCooldownReject(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	h.mustState(PhaseReady, "a", "")
	effs := h.do(acq(3, "b"))
	if len(ofType[effReject](effs)) != 0 || len(h.st.Queue) != 2 || h.st.Queue[1].ID != 3 {
		t.Fatalf("b must queue: %+v queue %+v", effs, h.st.Queue)
	}
	rejectOf[*CooldownError](t, h.do(acq(4, "c")), 4)
}

func TestT21CooldownTimerStartsSwapToQueueHead(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	h.at(5 * time.Second)
	effs := h.do(evRelease{Gen: 1})
	tm := ofType[effSetTimer](effs)
	if len(tm) != 1 || tm[0] != (effSetTimer{Kind: TimerCooldown, At: t0.Add(35 * time.Second)}) {
		t.Fatalf("timer %+v", tm)
	}
	h.at(35 * time.Second)
	h.do(evTimer{Kind: TimerCooldown})
	h.mustState(PhaseStopping, "a", "b")
}

func TestT22ZeroCooldownDrainsWhileStreaming(t *testing.T) {
	p := testParams()
	p.SwapCooldown = 0
	h := newHarness(t, p)
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	effs := h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "b")
	if indexOf[effRetireUpstream](effs) < 0 || indexOf[effStopModel](effs) < 0 {
		t.Fatalf("effects %+v", effs)
	}
}

func TestT23LoadCurrentCancelsDraining(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(xacq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	h.do(evLoad{Model: "a"})
	h.mustState(PhaseReady, "a", "")
	if h.st.Intent != nil || len(h.st.Queue) != 1 || h.st.Queue[0].ID != 2 {
		t.Fatalf("intent %v queue %+v", h.st.Intent, h.st.Queue)
	}
}

func TestT24ExplicitAcquireClearsFailedMark(t *testing.T) {
	h := failA(t)
	effs := h.do(xacq(2, "a"))
	if _, ok := h.st.Failed["a"]; ok {
		t.Fatal("explicit acquire must clear the mark")
	}
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "a") {
		t.Fatalf("start %+v", s)
	}
	if !slices.Equal(grantIDs(h.do(evLoadOK{Gen: 2, Upstream: h.up(2, "a")})), []uint64{2}) {
		t.Fatal("explicit waiter not granted")
	}
}

func checkI6(t *testing.T, h *harness) {
	t.Helper()
	for _, w := range h.st.Queue {
		if f, ok := h.st.Failed[w.Model]; ok && f.Active(h.now) {
			t.Fatalf("I6: waiter %d of failed model %q", w.ID, w.Model)
		}
	}
}

func TestT25ExplicitAcquireAndLoadFailedBothOrders(t *testing.T) {
	// Порядок 1: Loading(b) после Load(b) + явный Acquire(b), затем LoadFailed(b).
	h := newHarness(t, testParams())
	h.do(evLoad{Model: "b"})
	effs := h.do(xacq(1, "b"))
	if h.st.Intent == nil || *h.st.Intent != (Intent{Kind: IntentLoad, Model: "b"}) || len(h.st.Queue) != 1 ||
		len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("order 1 after acquire: intent %v queue %+v effects %+v", h.st.Intent, h.st.Queue, effs)
	}
	checkI6(t, h)
	effs = h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	if e := rejectOf[*LoadFailedError](t, effs, 1); e.Cached {
		t.Fatal("must be the outcome of this attempt, not the cache")
	}
	if h.st.Intent != nil || !h.st.Failed["b"].Active(h.now) {
		t.Fatalf("intent %v failed %+v", h.st.Intent, h.st.Failed)
	}
	checkI6(t, h)
	if effs = h.do(evVRAMFreed{Gen: 1}); len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("no reload without a new command: %+v", effs)
	}

	// Порядок 2: LoadFailed(b), затем явный Acquire(b).
	h = newHarness(t, testParams())
	h.do(evLoad{Model: "b"})
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	effs = h.do(xacq(1, "b"))
	if _, ok := h.st.Failed["b"]; ok || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("order 2: failed %+v effects %+v", h.st.Failed, effs)
	}
	h.mustState(PhaseWaitVRAM, "", "b")
	checkI6(t, h)
	effs = h.do(evVRAMFreed{Gen: 1})
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "b") {
		t.Fatalf("exactly one new start expected: %+v", s)
	}
	if !slices.Equal(grantIDs(h.do(evLoadOK{Gen: 2, Upstream: h.up(2, "b")})), []uint64{1}) {
		t.Fatal("waiter must get the outcome of the new attempt")
	}
}

func TestT26DeferredLoadShownAsLoading(t *testing.T) {
	status := func(h *harness, id string) string { return newSnapshot(h.st, h.p).ModelStatus(id, h.now) }
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(evLoad{Model: "b"})
	h.mustState(PhaseLoading, "a", "")
	if status(h, "a") != StatusLoading || status(h, "b") != StatusLoading || status(h, "c") != StatusUnloaded {
		t.Fatalf("statuses a=%s b=%s c=%s", status(h, "a"), status(h, "b"), status(h, "c"))
	}
	h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	h.mustState(PhaseDraining, "a", "b")
	if status(h, "a") != StatusLoaded || status(h, "b") != StatusLoading {
		t.Fatalf("after ready: a=%s b=%s", status(h, "a"), status(h, "b"))
	}

	h = newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(evLoad{Model: "b"})
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	h.mustState(PhaseWaitVRAM, "", "b")
	if status(h, "b") != StatusLoading || status(h, "a") != StatusFailed {
		t.Fatalf("after fail: a=%s b=%s", status(h, "a"), status(h, "b"))
	}
}

func assertHaltedSilent(t *testing.T, h *harness) {
	t.Helper()
	before := h.st.clone()
	for _, ev := range []Event{acq(10, "b"), evLoad{Model: "c"}, evVRAMFreed{Gen: h.st.Gen}, evStopped{Gen: h.st.Gen}, evUnload{}} {
		if effs := h.do(ev); len(effs) != 0 {
			t.Fatalf("E15: %T gave effects %+v", ev, effs)
		}
		if !reflect.DeepEqual(h.st, before) {
			t.Fatalf("E15: %T changed state", ev)
		}
	}
}

func checkFatalStep(t *testing.T, effs []Effect, pid int) {
	t.Helper()
	if len(effs) != 2 || !hasLog(effs, slog.LevelError, "llama-server exit not confirmed; host is exiting") {
		t.Fatalf("E14 effects %+v", effs)
	}
	f := ofType[effFatal](effs)
	if len(f) != 1 || !errors.Is(f[0].Err, ErrStopUnconfirmed) {
		t.Fatalf("fatal %+v", f)
	}
	want := `llama-server did not exit after TerminateProcess: model "a", pid ` + itoa(pid) + `, gen 1`
	if f[0].Err.Error() != want {
		t.Fatalf("fatal message %q, want %q", f[0].Err.Error(), want)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestT27StopFailedInStoppingHalts(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.at(31 * time.Second)
	h.do(acq(1, "b"))
	h.mustState(PhaseStopping, "a", "b")
	effs := h.do(evStopFailed{Gen: 1, PID: 1001, Message: "llama-server did not exit after TerminateProcess"})
	checkFatalStep(t, effs, 1001)
	if !h.st.Halted || h.st.Gen != 1 {
		t.Fatalf("state %+v", h.st)
	}
	h.mustState(PhaseStopping, "a", "b")
	assertHaltedSilent(t, h)
}

func TestT28StopFailedInLoadingHalts(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	effs := h.do(evStopFailed{Gen: 1, PID: 1001, Message: "llama-server did not exit after TerminateProcess"})
	checkFatalStep(t, effs, 1001)
	if len(ofType[effReject](effs)) != 0 || !h.st.Halted || len(h.st.Queue) != 1 {
		t.Fatalf("no reject expected: %+v state %+v", effs, h.st)
	}
	h.mustState(PhaseLoading, "a", "")
	assertHaltedSilent(t, h)
}

func TestT29DrainEndRetiresThenStops(t *testing.T) {
	p := testParams()
	p.SwapCooldown = 0
	h := newHarness(t, p)
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	effs := h.do(evRelease{Gen: 1})
	ri, si := indexOf[effRetireUpstream](effs), indexOf[effStopModel](effs)
	if ri < 0 || si < 0 || ri > si || len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("P8 effects %+v", effs)
	}
	if effs = h.do(evStopped{Gen: 1}); len(ofType[effStartModel](effs)) != 0 {
		t.Fatalf("start before VRAMFreed: %+v", effs)
	}
	if s := ofType[effStartModel](h.do(evVRAMFreed{Gen: 1})); len(s) != 1 || !isStart(s[0], 2, "b") {
		t.Fatalf("start %+v", s)
	}
}

// LoadedOnly (GET /props, F5-D1): Lease только в Ready; иначе отказ, и состояние не меняется — ни ожидающего, ни намерения.
func TestAcquireLoadedOnly(t *testing.T) {
	p := testParams()
	p.SwapCooldown = 0
	h := newHarness(t, p)
	only := func(id uint64, model string) evAcquire { return evAcquire{ID: id, Model: model, LoadedOnly: true} }
	rejected := func(id uint64, model string) {
		t.Helper()
		before := h.st.clone()
		r := ofType[effReject](h.do(only(id, model)))
		if len(r) != 1 || r[0].ID != id || !errors.Is(r[0].Err, ErrModelNotLoaded) || !reflect.DeepEqual(h.st, before) {
			t.Fatalf("waiter %d (%q): rejects %+v, state %+v", id, model, r, h.st)
		}
	}
	rejected(1, "a") // Empty
	rejected(2, "")
	h.do(evLoad{Model: "a"})
	rejected(3, "a") // Loading: Upstream ещё нет
	gen := h.st.Gen
	h.do(evBaseline{Gen: gen, MiB: 900})
	h.do(evLoadOK{Gen: gen, Upstream: h.up(gen, "a")})
	h.mustState(PhaseReady, "a", "")
	rejected(4, "b") // Ready с другой моделью: пересадка не начинается
	for i, model := range []string{"a", ""} {
		id := uint64(5 + i)
		g := ofType[effGrant](h.do(only(id, model)))
		if len(g) != 1 || g[0].ID != id || g[0].Model != "a" || g[0].Upstream == nil || h.st.Inflight != i+1 {
			t.Fatalf("grant %+v, inflight %d", g, h.st.Inflight)
		}
	}
	h.do(acq(7, "b"))
	h.mustState(PhaseDraining, "a", "b")
	rejected(8, "a") // Draining: поколение уходит, новых Lease нет
}
