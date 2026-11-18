package scheduler

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// v2Violation — I13, I15, I18 (§6.9 v2) по состоянию после шага; "" — нарушений нет.
func v2Violation(st State) string {
	in := func(id string) bool {
		_, ok := findModel(st.Catalog, id)
		return ok
	}
	if st.Paused && (len(st.Queue) > 0 || (st.Intent != nil && st.Intent.Kind != IntentUnload)) {
		return fmt.Sprintf("I13: paused with %d waiters, intent %+v", len(st.Queue), st.Intent)
	}
	for _, w := range st.Queue {
		if !in(w.Model) {
			return fmt.Sprintf("I15: waiter %d of %q is not in the catalog", w.ID, w.Model)
		}
	}
	for k := range st.Failed {
		if !in(k) {
			return fmt.Sprintf("I15: failed mark of %q is not in the catalog", k)
		}
	}
	if st.Intent != nil && st.Intent.Kind != IntentUnload && !in(st.Intent.Model) {
		return fmt.Sprintf("I15: intent %+v names a model outside the catalog", *st.Intent)
	}
	if st.Target != "" && !in(st.Target) {
		return fmt.Sprintf("I15: target %q is not in the catalog", st.Target)
	}
	if st.Running.ID != st.Current {
		return fmt.Sprintf("I18: running %q, current %q", st.Running.ID, st.Current)
	}
	return ""
}

// cmdResult — единственный CmdResult шага (I17).
func cmdResult(t *testing.T, effs []Effect) error {
	t.Helper()
	r := ofType[effCmdResult](effs)
	if len(r) != 1 {
		t.Fatalf("want exactly one CmdResult, got %d in %+v", len(r), effs)
	}
	return r[0].Err
}

// cmdOK — команда принята (CmdResult(nil)).
func (h *harness) cmdOK(ev Event) []Effect {
	h.t.Helper()
	effs := h.do(ev)
	if err := cmdResult(h.t, effs); err != nil {
		h.t.Fatalf("%T %+v rejected: %v", ev, ev, err)
	}
	return effs
}

// cmdRejected — команда отклонена: единственный эффект — CmdResult с ошибкой, состояние не изменилось.
func (h *harness) cmdRejected(ev Event) error {
	h.t.Helper()
	before := h.st.clone()
	effs := h.do(ev)
	if len(effs) != 1 {
		h.t.Fatalf("%T %+v: rejected command gave %d effects: %+v", ev, ev, len(effs), effs)
	}
	err := cmdResult(h.t, effs)
	if err == nil {
		h.t.Fatalf("%T %+v accepted, want rejection", ev, ev)
	}
	if !reflect.DeepEqual(h.st, before) {
		h.t.Fatalf("%T %+v: rejected command changed state", ev, ev)
	}
	return err
}

// rejectedAcquire — Acquire отклонён ошибкой target, состояние не изменилось.
func (h *harness) rejectedAcquire(ev evAcquire, target error) {
	h.t.Helper()
	before := h.st.clone()
	r := ofType[effReject](h.do(ev))
	if len(r) != 1 || r[0].ID != ev.ID || !errors.Is(r[0].Err, target) {
		h.t.Fatalf("acquire %+v: rejects %+v, want %v", ev, r, target)
	}
	if !reflect.DeepEqual(h.st, before) {
		h.t.Fatalf("acquire %+v changed state", ev)
	}
}

// logAttr — значение атрибута key первой строки журнала msg.
func logAttr(effs []Effect, msg, key string) (slog.Value, bool) {
	for _, l := range ofType[effLog](effs) {
		if l.Msg != msg {
			continue
		}
		for _, a := range l.Attrs {
			if a.Key == key {
				return a.Value, true
			}
		}
	}
	return slog.Value{}, false
}

func catalogIDs(st State) []string {
	ids := make([]string, len(st.Catalog))
	for i, m := range st.Catalog {
		ids[i] = m.ID
	}
	return ids
}

func loadedOnly(id uint64, model string) evAcquire {
	return evAcquire{ID: id, Model: model, LoadedOnly: true}
}

// retireBeforeStop — RetireUpstream(gen, reason) стоит раньше StopModel(gen) (I12, I16).
func retireBeforeStop(t *testing.T, effs []Effect, gen uint64, reason RetireReason) {
	t.Helper()
	ri, si := indexOf[effRetireUpstream](effs), indexOf[effStopModel](effs)
	if ri < 0 || si < 0 || ri > si {
		t.Fatalf("retire %d stop %d in %+v", ri, si, effs)
	}
	if r := effs[ri].(effRetireUpstream); r.Gen != gen || r.Reason != reason || r.Upstream == nil {
		t.Fatalf("retire %+v, want gen %d reason %d", r, gen, reason)
	}
	if s := effs[si].(effStopModel); s.Gen != gen {
		t.Fatalf("stop %+v", s)
	}
}

func TestT30PauseRejectsWaitersAndDrains(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "a"))
	h.do(xacq(3, "b")) // явный B: Draining(A→B)
	h.do(evLoad{Model: "a"})
	h.do(acq(4, "b")) // неявный B
	h.mustState(PhaseReady, "a", "")
	if h.st.Inflight != 2 || len(h.st.Queue) != 2 {
		t.Fatalf("setup: inflight %d queue %+v", h.st.Inflight, h.st.Queue)
	}
	effs := h.cmdOK(evPause{})
	for _, id := range []uint64{3, 4} {
		if err := rejectOf[error](t, effs, id); !errors.Is(err, ErrPaused) {
			t.Fatalf("waiter %d: %v", id, err)
		}
	}
	h.mustState(PhaseDraining, "a", "")
	if !h.st.Paused || len(h.st.Queue) != 0 || *h.st.Intent != (Intent{Kind: IntentUnload}) || len(ofType[effRetireUpstream](effs)) != 0 {
		t.Fatalf("after pause: %+v effects %+v", h.st, effs)
	}
	if v, ok := logAttr(effs, "paused", "force"); !ok || v.Bool() {
		t.Fatalf("paused log force=%v ok=%v", v, ok)
	}
	mark := len(h.all)
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseDraining, "a", "")
	retireBeforeStop(t, h.do(evRelease{Gen: 1}), 1, RetireStop)
	h.mustState(PhaseStopping, "a", "")
	h.do(evStopped{Gen: 1})
	h.mustState(PhaseWaitVRAM, "", "")
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	if h.st.Intent != nil || len(ofType[effStartModel](h.all[mark:])) != 0 {
		t.Fatalf("intent %+v, starts %+v", h.st.Intent, ofType[effStartModel](h.all[mark:]))
	}
	h.rejectedAcquire(acq(5, "a"), ErrPaused)
	h.rejectedAcquire(xacq(6, "a"), ErrPaused)
	h.rejectedAcquire(loadedOnly(7, ""), ErrPaused)
}

func TestT31ForcedPauseRetiresFirst(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "a"))
	effs := h.cmdOK(evPause{Force: true})
	retireBeforeStop(t, effs, 1, RetireForced)
	h.mustState(PhaseStopping, "a", "")
	if h.st.Inflight != 0 || h.st.Upstream != nil {
		t.Fatalf("after F1: %+v", h.st)
	}
	if i, reason := phaseLog(effs, PhaseStopping); i < 0 || reason != "forced_unload" {
		t.Fatalf("stopping log %d reason %q", i, reason)
	}
	if v, ok := logAttr(effs, "forced unload", "aborted"); !ok || v.Int64() != 2 {
		t.Fatalf("aborted %v ok %v", v, ok)
	}
	before := h.st.clone()
	effs = h.do(evRelease{Gen: 1}) // поздний Release снятого поколения — устаревший (E13)
	if !reflect.DeepEqual(h.st, before) || !hasLog(effs, slog.LevelDebug, "stale event") {
		t.Fatalf("late release: %+v effects %+v", h.st, effs)
	}
}

func TestT32PauseInLoadingUnloadsAfterLoadOK(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	effs := h.cmdOK(evPause{Force: true})
	if len(ofType[effRetireUpstream](effs)) != 0 || len(ofType[effStopModel](effs)) != 0 {
		t.Fatalf("F1 in loading: %+v", effs)
	}
	if err := rejectOf[error](t, effs, 1); !errors.Is(err, ErrPaused) {
		t.Fatalf("waiter: %v", err)
	}
	h.mustState(PhaseLoading, "a", "")
	h.do(evBaseline{Gen: 1, MiB: 900})
	effs = h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	if i, _ := phaseLog(effs, PhaseDraining); i < 0 || len(ofType[effGrant](effs)) != 0 {
		t.Fatalf("LoadOK on pause: %+v", effs)
	}
	retireBeforeStop(t, effs, 1, RetireStop)
	h.mustState(PhaseStopping, "a", "")
}

func TestT33ResumeKeepsUnloadThenLoadsOnDemand(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(xacq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	h.cmdOK(evPause{})
	h.mustState(PhaseDraining, "a", "")
	mark := len(h.all)
	h.cmdOK(evResume{})
	if h.st.Paused || *h.st.Intent != (Intent{Kind: IntentUnload}) {
		t.Fatalf("after resume: %+v", h.st)
	}
	h.mustState(PhaseDraining, "a", "")
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "")
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	if len(ofType[effStartModel](h.all[mark:])) != 0 {
		t.Fatalf("resume loaded something: %+v", ofType[effStartModel](h.all[mark:]))
	}
	effs := h.do(acq(3, "a"))
	h.mustState(PhaseLoading, "a", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "a") {
		t.Fatalf("start %+v", s)
	}
}

func TestT34PausedRejectsLoadsAllowsUnloadAndReconfigure(t *testing.T) {
	h := newHarness(t, testParams())
	h.cmdOK(evPause{})
	h.mustState(PhaseEmpty, "", "")
	if !h.st.Paused || h.st.Intent != nil {
		t.Fatalf("paused empty: %+v", h.st)
	}
	for _, ev := range []Event{evLoad{Model: "a"}, evRestart{Model: "a"}} {
		if err := h.cmdRejected(ev); !errors.Is(err, ErrPaused) {
			t.Fatalf("%T: %v", ev, err)
		}
	}
	if err := h.cmdRejected(evLoad{Model: "zz"}); !errors.Is(err, ErrModelNotFound) { // C2 раньше C3
		t.Fatalf("unknown model on pause: %v", err)
	}
	h.rejectedAcquire(xacq(1, "a"), ErrPaused)
	h.rejectedAcquire(acq(2, "a"), ErrPaused)
	h.cmdOK(evUnload{Model: "a"})
	h.cmdOK(evUnload{Force: true})
	h.cmdOK(evReconfigure{Catalog: testCatalog("a", "b"), Guard: true})
	if !slices.Equal(catalogIDs(h.st), []string{"a", "b"}) || !h.st.Paused {
		t.Fatalf("catalog %v paused %v", catalogIDs(h.st), h.st.Paused)
	}
	h.mustState(PhaseEmpty, "", "")
}

func TestT35ForceUnloadInDrainingKeepsWaiters(t *testing.T) {
	p := testParams()
	p.SwapCooldown = 0
	h := newHarness(t, p)
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	effs := h.cmdOK(evUnload{Force: true})
	retireBeforeStop(t, effs, 1, RetireForced)
	h.mustState(PhaseStopping, "a", "b") // P9 вернул Target = B
	if h.st.Inflight != 0 || len(ofType[effReject](effs)) != 0 || len(h.st.Queue) != 1 {
		t.Fatalf("after force unload: %+v effects %+v", h.st, effs)
	}
	h.do(evStopped{Gen: 1})
	h.mustState(PhaseWaitVRAM, "", "b")
	effs = h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseLoading, "b", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 2, "b") {
		t.Fatalf("start %+v", s)
	}
}

// F1 только в Ready и Draining: в остальных фазах ForceUnload — то же, что мягкая выгрузка.
func TestT36ForceUnloadOutsideServingIsSoft(t *testing.T) {
	empty := newHarness(t, testParams())
	loading := newHarness(t, testParams())
	loading.do(acq(1, "a"))
	stopping := newHarness(t, testParams())
	stopping.ready("a")
	stopping.at(31 * time.Second)
	stopping.do(acq(1, "b"))
	stopping.mustState(PhaseStopping, "a", "b")
	waitVRAM := newHarness(t, testParams())
	waitVRAM.ready("a")
	waitVRAM.at(31 * time.Second)
	waitVRAM.do(acq(1, "b"))
	waitVRAM.do(evStopped{Gen: 1})
	waitVRAM.mustState(PhaseWaitVRAM, "", "b")
	for _, h := range []*harness{empty, loading, stopping, waitVRAM} {
		for _, model := range []string{"", "a"} {
			soft, softEffs := step(h.st, evUnload{Model: model}, h.now, h.p)
			hard, hardEffs := step(h.st, evUnload{Model: model, Force: true}, h.now, h.p)
			if !reflect.DeepEqual(soft, hard) || !reflect.DeepEqual(softEffs, hardEffs) {
				t.Fatalf("%s, model %q: force differs from soft", h.st.Phase, model)
			}
			if len(ofType[effRetireUpstream](hardEffs)) != 0 {
				t.Fatalf("%s: retire on force unload", h.st.Phase)
			}
		}
	}
	// F2-D1 (обзор Ф2): в Ready с незавершёнными ответами ForceUnload чужого имени — та же мягкая выгрузка.
	ready := newHarness(t, testParams())
	ready.ready("a")
	ready.do(acq(1, "a"))
	soft, softEffs := step(ready.st, evUnload{Model: "b"}, ready.now, ready.p)
	hard, hardEffs := step(ready.st, evUnload{Model: "b", Force: true}, ready.now, ready.p)
	if !reflect.DeepEqual(soft, hard) || !reflect.DeepEqual(softEffs, hardEffs) {
		t.Fatalf("ready with inflight %d: force unload of another model differs from soft", ready.st.Inflight)
	}
	if len(ofType[effRetireUpstream](hardEffs)) != 0 {
		t.Fatalf("retire on force unload of another model")
	}
}

func TestT37RestartAppliesNewDefinition(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.cmdOK(evReconfigure{Catalog: []config.Model{mdlCtx("a", 8192), mdl("b"), mdl("c")}, Guard: true})
	if !reflect.DeepEqual(h.st.Running, mdl("a")) || !restartRequired(h.st.Phase, h.st.Current, h.st.Running, h.st.Catalog) {
		t.Fatalf("running %+v", h.st.Running)
	}
	effs := h.cmdOK(evRestart{Model: "a"})
	h.mustState(PhaseDraining, "a", "a")
	if i, reason := phaseLog(effs, PhaseDraining); i < 0 || reason != "restart" {
		t.Fatalf("draining log %d reason %q", i, reason)
	}
	if *h.st.Intent != (Intent{Kind: IntentRestart, Model: "a"}) {
		t.Fatalf("intent %+v", *h.st.Intent)
	}
	if effs = h.do(acq(2, "a")); len(ofType[effGrant](effs)) != 0 || len(h.st.Queue) != 1 {
		t.Fatalf("acquire during restart: %+v queue %+v", effs, h.st.Queue)
	}
	retireBeforeStop(t, h.do(evRelease{Gen: 1}), 1, RetireStop)
	h.mustState(PhaseStopping, "a", "a")
	h.do(evStopped{Gen: 1})
	h.mustState(PhaseWaitVRAM, "", "a")
	if h.st.Running.ID != "" {
		t.Fatalf("running after stop %+v", h.st.Running)
	}
	effs = h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseLoading, "a", "")
	s := ofType[effStartModel](effs)
	if len(s) != 1 || s[0].Gen != 2 || !reflect.DeepEqual(s[0].Model, mdlCtx("a", 8192)) || !reflect.DeepEqual(h.st.Running, mdlCtx("a", 8192)) {
		t.Fatalf("start %+v running %+v", s, h.st.Running)
	}
	if *h.st.Intent != (Intent{Kind: IntentLoad, Model: "a"}) {
		t.Fatalf("intent after P11 %+v", *h.st.Intent)
	}
	h.do(evBaseline{Gen: 2, MiB: 900})
	effs = h.do(evLoadOK{Gen: 2, Upstream: h.up(2, "a")})
	if !slices.Equal(grantIDs(effs), []uint64{2}) || h.st.Intent != nil {
		t.Fatalf("after reload: grants %v intent %+v", grantIDs(effs), h.st.Intent)
	}
	if restartRequired(h.st.Phase, h.st.Current, h.st.Running, h.st.Catalog) {
		t.Fatal("restart still required")
	}
}

func TestT38RestartIsLoadElsewhereAndCancelledByLoad(t *testing.T) {
	h := newHarness(t, testParams())
	effs := h.cmdOK(evRestart{Model: "a"}) // A не загружена: C12 = Load
	h.mustState(PhaseLoading, "a", "")
	if s := ofType[effStartModel](effs); len(s) != 1 || !isStart(s[0], 1, "a") || *h.st.Intent != (Intent{Kind: IntentLoad, Model: "a"}) {
		t.Fatalf("restart of unloaded: %+v intent %+v", s, h.st.Intent)
	}

	h = newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.cmdOK(evRestart{Model: "a"}) // C11 в Loading
	if *h.st.Intent != (Intent{Kind: IntentRestart, Model: "a"}) {
		t.Fatalf("intent %+v", *h.st.Intent)
	}
	h.mustState(PhaseLoading, "a", "")
	h.do(evBaseline{Gen: 1, MiB: 900})
	effs = h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	if !slices.Equal(grantIDs(effs), []uint64{1}) { // P1 раньше P3r
		t.Fatalf("grants %v", grantIDs(effs))
	}
	if i, reason := phaseLog(effs, PhaseDraining); i < 0 || reason != "restart" {
		t.Fatalf("P3r log %d reason %q", i, reason)
	}
	h.mustState(PhaseDraining, "a", "a")

	mark := len(h.all)
	h.cmdOK(evLoad{Model: "a"}) // «оставь A» отменяет перезапуск
	h.mustState(PhaseReady, "a", "")
	if h.st.Intent != nil {
		t.Fatalf("intent %+v", *h.st.Intent)
	}
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseReady, "a", "")
	if len(ofType[effStartModel](h.all[mark:])) != 0 || len(ofType[effStopModel](h.all[mark:])) != 0 {
		t.Fatalf("restart not cancelled: %+v", h.all[mark:])
	}
}

func TestT39ReconfigureRemovesWaitedModel(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(xacq(2, "b"))
	h.do(acq(3, "b"))
	h.mustState(PhaseDraining, "a", "b")
	effs := h.cmdOK(evReconfigure{Catalog: testCatalog("a", "c"), Guard: false})
	for _, id := range []uint64{2, 3} {
		if err := rejectOf[error](t, effs, id); !errors.Is(err, ErrModelNotFound) {
			t.Fatalf("waiter %d: %v", id, err)
		}
	}
	h.mustState(PhaseReady, "a", "") // P7
	if i, reason := phaseLog(effs, PhaseReady); i < 0 || reason != "cancel" || h.st.Intent != nil || len(h.st.Queue) != 0 {
		t.Fatalf("after reconfigure: %+v reason %q", h.st, reason)
	}
	if v, ok := logAttr(effs, "catalog updated", "removed"); !ok || v.Int64() != 1 || !slices.Equal(catalogIDs(h.st), []string{"a", "c"}) {
		t.Fatalf("catalog %v removed %v", catalogIDs(h.st), v)
	}

	h = newHarness(t, testParams()) // только метка Failed[B]
	h.do(acq(1, "b"))
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	h.do(evVRAMFreed{Gen: 1})
	if !h.st.Failed["b"].Active(h.now) {
		t.Fatal("setup: no failed mark")
	}
	h.cmdOK(evReconfigure{Catalog: testCatalog("a", "c"), Guard: false})
	if _, ok := h.st.Failed["b"]; ok {
		t.Fatal("RC2: failed mark of removed model kept")
	}
	h.rejectedAcquire(acq(2, "b"), ErrModelNotFound)
}

func TestT40ReconfigureRemovesCurrentModel(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a")
	h.do(acq(1, "a"))
	h.cmdOK(evReconfigure{Catalog: testCatalog("b", "c"), Guard: false})
	h.mustState(PhaseDraining, "a", "")
	if *h.st.Intent != (Intent{Kind: IntentUnload, Model: "a"}) {
		t.Fatalf("intent %+v", *h.st.Intent)
	}
	h.do(evRelease{Gen: 1})
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	h.rejectedAcquire(acq(2, "a"), ErrModelNotFound)
}

func TestT41ReconfigureChangesLoadingModel(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.cmdOK(evReconfigure{Catalog: []config.Model{mdlCtx("a", 8192), mdl("b"), mdl("c")}, Guard: true})
	if !reflect.DeepEqual(h.st.Running, mdl("a")) {
		t.Fatalf("running %+v", h.st.Running)
	}
	h.do(evBaseline{Gen: 1, MiB: 900})
	h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	if !newSnapshot(h.st, h.p).RestartRequired() {
		t.Fatal("restart not required after LoadOK")
	}
	h.do(evRelease{Gen: 1})
	h.cmdOK(evRestart{Model: "a"})
	h.mustState(PhaseStopping, "a", "a")
	h.do(evStopped{Gen: 1})
	s := ofType[effStartModel](h.do(evVRAMFreed{Gen: 1}))
	if len(s) != 1 || s[0].Gen != 2 || !reflect.DeepEqual(s[0].Model, mdlCtx("a", 8192)) {
		t.Fatalf("start %+v", s)
	}
}

func TestT42RemovedLoadingModelFailsWithoutMark(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	effs := h.cmdOK(evReconfigure{Catalog: testCatalog("b", "c"), Guard: false})
	if err := rejectOf[error](t, effs, 1); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("waiter: %v", err)
	}
	if *h.st.Intent != (Intent{Kind: IntentUnload, Model: "a"}) || h.st.Running.ID != "a" {
		t.Fatalf("intent %+v running %+v", h.st.Intent, h.st.Running)
	}
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	h.mustState(PhaseWaitVRAM, "", "")
	if _, ok := h.st.Failed["a"]; ok || !reflect.DeepEqual(h.st.Running, config.Model{}) {
		t.Fatalf("failed %+v running %+v", h.st.Failed, h.st.Running)
	}
}

func TestT43HaltedCommandsAreSilent(t *testing.T) {
	h := newHarness(t, testParams())
	h.do(acq(1, "a"))
	h.do(evStopFailed{Gen: 1, PID: 1001, Message: "llama-server did not exit after TerminateProcess"})
	if !h.st.Halted {
		t.Fatal("not halted")
	}
	before := h.st.clone()
	for _, ev := range []Event{evLoad{Model: "b"}, evLoad{Model: "zz"}, evUnload{}, evUnload{Model: "a", Force: true},
		evRestart{Model: "a"}, evPause{Force: true}, evResume{}, evReconfigure{Catalog: testCatalog("c"), Guard: true}} {
		if effs := h.do(ev); len(effs) != 0 {
			t.Fatalf("C1: %T gave effects %+v", ev, effs)
		}
		if !reflect.DeepEqual(h.st, before) {
			t.Fatalf("C1: %T changed state", ev)
		}
	}
}

func TestT44LoadedOnlyOnPause(t *testing.T) {
	h := newHarness(t, testParams())
	h.cmdOK(evPause{})
	h.rejectedAcquire(loadedOnly(1, ""), ErrPaused)
	h.rejectedAcquire(loadedOnly(2, "a"), ErrPaused)
	h.cmdOK(evResume{})
	h.ready("a")
	if g := ofType[effGrant](h.do(loadedOnly(3, "a"))); len(g) != 1 || g[0].ID != 3 {
		t.Fatalf("grant %+v", g)
	}
	h.rejectedAcquire(loadedOnly(4, "b"), ErrModelNotLoaded)
}

func TestT45EveryCommandGivesOneResult(t *testing.T) {
	h := newHarness(t, testParams())
	for _, ev := range []Event{evLoad{Model: "zz"}, evRestart{Model: "zz"}, evUnload{Model: "zz"}, evUnload{Model: "zz", Force: true}} {
		err := h.cmdRejected(ev)
		if !errors.Is(err, ErrModelNotFound) || err.Error() != `model is not configured: "zz"` {
			t.Fatalf("%T: %v", ev, err)
		}
	}
	h.ready("a")
	for _, ev := range []Event{evLoad{Model: "a"}, evRestart{Model: "b"}, evUnload{Model: "b"}, evUnload{Model: "a"},
		evLoad{Model: "a"}, evPause{}, evResume{}, evUnload{Force: true}, evReconfigure{Catalog: testCatalog("a", "b", "c")}} {
		h.cmdOK(ev)
	}
	for _, ev := range []Event{evAcquire{ID: 9, Model: "b"}, evRelease{Gen: 77}, evTimer{Kind: TimerCooldown}} {
		if r := ofType[effCmdResult](h.do(ev)); len(r) != 0 {
			t.Fatalf("%T gave CmdResult", ev)
		}
	}
}

// RC0: удаление занятого имени при Guard — отказ с именами в порядке Catalog, состояние прежнее.
func TestT46GuardRejectsBusyNames(t *testing.T) {
	inUse := func(h *harness, keep []string, want ...string) {
		t.Helper()
		err := h.cmdRejected(evReconfigure{Catalog: testCatalog(keep...), Guard: true})
		var iu *InUseError
		if !errors.As(err, &iu) || !slices.Equal(iu.Models, want) {
			t.Fatalf("%s: err %v, want in use %v", h.st.Phase, err, want)
		}
	}
	p := testParams()
	p.SwapCooldown = 0

	h := newHarness(t, p) // Current в Loading (ожидающий ушёл, намерения нет)
	h.do(acq(1, "a"))
	h.do(evWaiterGone{ID: 1})
	inUse(h, []string{"b", "c"}, "a")

	h = newHarness(t, p) // Current в Ready
	h.ready("a")
	inUse(h, []string{"b", "c"}, "a")

	h = newHarness(t, p) // Current в Draining (пересадка к B по ожидающему)
	h.ready("a")
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.mustState(PhaseDraining, "a", "b")
	inUse(h, []string{"b", "c"}, "a")
	inUse(h, []string{"a", "c"}, "b") // Target вместе с ожидающим

	h = newHarness(t, p) // Intent Load
	h.do(acq(1, "a"))
	h.do(evWaiterGone{ID: 1})
	h.cmdOK(evLoad{Model: "b"})
	h.mustState(PhaseLoading, "a", "")
	inUse(h, []string{"a", "c"}, "b")

	h = newHarness(t, p) // Intent Restart
	h.ready("a")
	h.do(acq(1, "a"))
	h.cmdOK(evRestart{Model: "a"})
	inUse(h, []string{"b", "c"}, "a")

	h = newHarness(t, p) // Intent Unload(A) в Stopping
	h.ready("a")
	h.cmdOK(evUnload{Model: "a"})
	h.mustState(PhaseStopping, "a", "")
	inUse(h, []string{"b", "c"}, "a")

	h = newHarness(t, p) // модель ожидающего
	h.do(acq(1, "a"))
	h.do(acq(2, "b"))
	h.mustState(PhaseLoading, "a", "")
	inUse(h, []string{"a", "c"}, "b")

	h = newHarness(t, p) // два имени — в порядке Catalog, не очереди
	h.do(acq(1, "b"))
	h.do(acq(2, "a"))
	inUse(h, []string{"c"}, "a", "b")

	h = newHarness(t, p) // приняты: Current в Stopping без намерения; Failed удалённой снята
	h.do(acq(1, "c"))
	h.do(evLoadFailed{Gen: 1, ExitCode: 1, Message: "boom"})
	h.do(evVRAMFreed{Gen: 1})
	h.ready("a")
	h.do(acq(2, "b"))
	h.mustState(PhaseStopping, "a", "b")
	if h.st.Intent != nil || !h.st.Failed["c"].Active(h.now) {
		t.Fatalf("setup: %+v", h.st)
	}
	h.cmdOK(evReconfigure{Catalog: testCatalog("b"), Guard: true})
	if _, ok := h.st.Failed["c"]; ok || !slices.Equal(catalogIDs(h.st), []string{"b"}) {
		t.Fatalf("accepted: failed %+v catalog %v", h.st.Failed, catalogIDs(h.st))
	}
	h.mustState(PhaseStopping, "a", "b")

	h = newHarness(t, p) // смена флагов занятого имени — принята
	h.ready("a")
	h.cmdOK(evReconfigure{Catalog: []config.Model{mdlCtx("a", 8192), mdl("b"), mdl("c")}, Guard: true})
	h.mustState(PhaseReady, "a", "")
}

func TestT47RollbackRemovesAddedModel(t *testing.T) {
	c := testCatalog("a", "b", "c")
	h := newHarness(t, testParams())
	h.cmdOK(evReconfigure{Catalog: append(testCatalog("a", "b", "c"), mdl("n")), Guard: true})
	h.cmdOK(evLoad{Model: "n"})
	h.mustState(PhaseLoading, "n", "")
	if !reflect.DeepEqual(h.st.Running, mdl("n")) {
		t.Fatalf("running %+v", h.st.Running)
	}
	h.cmdOK(evReconfigure{Catalog: c, Guard: false})
	if !reflect.DeepEqual(h.st.Catalog, c) || *h.st.Intent != (Intent{Kind: IntentUnload, Model: "n"}) {
		t.Fatalf("after rollback: catalog %v intent %+v", catalogIDs(h.st), h.st.Intent)
	}
	h.do(evBaseline{Gen: 1, MiB: 900})
	effs := h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "n")})
	if i, _ := phaseLog(effs, PhaseDraining); i < 0 {
		t.Fatalf("no draining after LoadOK: %+v", effs)
	}
	h.mustState(PhaseStopping, "n", "")
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.mustState(PhaseEmpty, "", "")
	h.rejectedAcquire(acq(1, "n"), ErrModelNotFound)
}

// Приоритет отказов A0/A1/A1p и C2/C3/C9/C10: неизвестное имя проверяется до паузы,
// кроме LoadedOnly, где пауза проверяется первой.
func TestPausedRejectionPriority(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
		want error
	}{
		{"load known", evLoad{Model: "a"}, ErrPaused},
		{"load unknown", evLoad{Model: "zz"}, ErrModelNotFound},
		{"restart known", evRestart{Model: "a"}, ErrPaused},
		{"restart unknown", evRestart{Model: "zz"}, ErrModelNotFound},
		{"unload unknown", evUnload{Model: "zz"}, ErrModelNotFound},
		{"force unload unknown", evUnload{Model: "zz", Force: true}, ErrModelNotFound},
		{"acquire known", acq(1, "a"), ErrPaused},
		{"acquire unknown", acq(1, "zz"), ErrModelNotFound},
		{"explicit known", xacq(1, "a"), ErrPaused},
		{"explicit unknown", xacq(1, "zz"), ErrModelNotFound},
		{"loaded only unknown", loadedOnly(1, "zz"), ErrPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testParams())
			h.cmdOK(evPause{})
			if a, ok := tc.ev.(evAcquire); ok {
				h.rejectedAcquire(a, tc.want)
			} else if err := h.cmdRejected(tc.ev); !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
		})
	}
}

// Отказ команды не продвигает даже созревшую очередь и не устанавливает таймер.
func TestRejectedCommandsDoNotAdvance(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
	}{
		{"load", evLoad{Model: "zz"}},
		{"unload", evUnload{Model: "zz"}},
		{"restart", evRestart{Model: "zz"}},
		{"guard", evReconfigure{Catalog: testCatalog("a", "c"), Guard: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testParams())
			h.ready("a")
			h.do(acq(1, "a"))
			h.do(xacq(2, "b"))
			// Загрузка A отменяет Draining, но B остаётся ждать cooldown.
			h.cmdOK(evLoad{Model: "a"})
			h.do(evRelease{Gen: 1})
			h.mustState(PhaseReady, "a", "")
			h.at(31 * time.Second)
			h.cmdRejected(tc.ev)
			h.mustState(PhaseReady, "a", "")
		})
	}
}

// Выгрузка текущего имени разрешена и после его удаления из каталога (C7/C8).
func TestUnloadRemovedCurrent(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			h := newHarness(t, testParams())
			h.ready("a")
			h.do(acq(1, "a"))
			h.cmdOK(evReconfigure{Catalog: testCatalog("b", "c")})
			effs := h.cmdOK(evUnload{Model: "a", Force: force})
			if force {
				retireBeforeStop(t, effs, 1, RetireForced)
				h.mustState(PhaseStopping, "a", "")
			} else {
				h.mustState(PhaseDraining, "a", "")
				retireBeforeStop(t, h.do(evRelease{Gen: 1}), 1, RetireStop)
			}
		})
	}
}

// E4/E5 снимают Restart до повторной загрузки, как в Stopping, так и в WaitVRAM.
func TestUnloadCancelsRestart(t *testing.T) {
	for _, waitVRAM := range []bool{false, true} {
		t.Run(fmt.Sprintf("wait_vram=%v", waitVRAM), func(t *testing.T) {
			h := newHarness(t, testParams())
			h.ready("a")
			h.cmdOK(evRestart{Model: "a"})
			h.mustState(PhaseStopping, "a", "a")
			if waitVRAM {
				h.do(evStopped{Gen: 1})
			}
			h.cmdOK(evUnload{Model: "a"})
			if !waitVRAM {
				h.do(evStopped{Gen: 1})
			}
			effs := h.do(evVRAMFreed{Gen: 1})
			h.mustState(PhaseEmpty, "", "")
			if len(ofType[effStartModel](effs)) != 0 {
				t.Fatal("unload did not cancel restart")
			}
		})
	}
}
