package scheduler

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

type fuzzStats struct {
	halted, explicitBefore, explicitAfter, grants, starts int
	forced, inUse, pauses, restarts, reconfigs            int
}

// fuzzPool — четыре имени; у каждого два определения (§14.2 v2).
var fuzzPool = []string{"a", "b", "c", "d"}

func fuzzDef(id string, variant int) config.Model { return mdlCtx(id, 4096*(variant+1)) }

// sim — автомат плюс корректная модель исполнителя эффектов (§14.4).
type sim struct {
	t        *testing.T
	seed     uint64
	rng      *rand.Rand
	p        Params
	st       State
	now      time.Time
	nextID   uint64
	chains   [][]Event  // результаты исполнителей; внутри цепочки — строго по порядку
	leases   []effGrant // невозвращённые Lease
	timerAt  time.Time
	timerSet bool
	waiting  map[uint64]string // ожидающие без ответа (I5)
	retired  map[uint64]int    // Gen → число RetireUpstream (I12)
	loadOK   map[uint64]bool   // Gen получил LoadOK
	lastGen  uint64            // Gen последнего StartModel
	exitOK   bool              // выход процесса lastGen подтверждён (I4)
	freedOK  bool              // и после этого пришёл его VRAMFreed (I4)
	fixed    bool              // бэкенд «починен» (финальная проверка)
	halted   bool
	stats    *fuzzStats
}

func newSim(t *testing.T, seed uint64, stats *fuzzStats) *sim {
	p := Params{SwapCooldown: 30 * time.Second, MaxQueue: 3, FailedTTL: 120 * time.Second}
	if seed%2 == 0 {
		p.SwapCooldown = 0
	}
	if seed%3 == 0 {
		p.FailedTTL = 0
	}
	return &sim{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, 0x5eed)), p: p,
		st: initialState(t0, testCatalog("a", "b", "c"), false), now: t0,
		waiting: map[uint64]string{}, retired: map[uint64]int{}, loadOK: map[uint64]bool{}, stats: stats}
}

func (s *sim) fail(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf("seed %d: "+format, append([]any{s.seed}, args...)...)
}

func (s *sim) deliver(ev Event) {
	s.t.Helper()
	in, before := s.st, s.st.clone()
	before.Catalog = cloneCatalog(before.Catalog)
	before.Running.Args = slices.Clone(before.Running.Args)
	next, effs := step(in, ev, s.now, s.p)
	if !reflect.DeepEqual(in, before) {
		s.fail("I11: step mutated its input on %T", ev)
	}
	if before.Halted { // E15 и C1
		if len(effs) != 0 || !reflect.DeepEqual(next, before) {
			s.fail("E15: halted step on %T gave %d effects", ev, len(effs))
		}
		return
	}
	s.noteEvent(before, ev)
	if isCommand(ev) {
		s.checkCommand(ev, before, next, effs)
	}
	s.st = next
	s.apply(before, effs)
	s.check()
}

// checkCommand — I17: ровно один CmdResult; отказ (в том числе по RC0) — единственный эффект, состояние прежнее.
func (s *sim) checkCommand(ev Event, before, next State, effs []Effect) {
	s.t.Helper()
	n, err := 0, error(nil)
	for _, e := range effs {
		if r, ok := e.(effCmdResult); ok {
			n, err = n+1, r.Err
		}
	}
	if n != 1 {
		s.fail("I17: %T gave %d CmdResult", ev, n)
	}
	if err != nil {
		if len(effs) != 1 || !reflect.DeepEqual(next, before) {
			s.fail("rejected %T (%v) gave %d effects or changed state", ev, err, len(effs))
		}
		var iu *InUseError
		if errors.As(err, &iu) {
			s.stats.inUse++
		}
		return
	}
	// F2-D1 (обзор Ф2): принятая принудительная выгрузка чужого имени не снимает поколение (C8).
	if u, ok := ev.(evUnload); ok && u.Force && u.Model != "" && u.Model != before.Current {
		for _, e := range effs {
			if r, ok := e.(effRetireUpstream); ok && r.Reason == RetireForced {
				s.fail("forced unload of %q (current %q) retired the serving gen", u.Model, before.Current)
			}
		}
	}
	// F2-N1 (обзор Ф2): принятая замена с guard не отказывает ожидающим — занятые имена не удаляются (RC0, RC1).
	if rc, ok := ev.(evReconfigure); ok && rc.Guard {
		for _, e := range effs {
			if _, ok := e.(effReject); ok {
				s.fail("accepted guarded Reconfigure rejected a waiter")
			}
		}
	}
	switch ev.(type) {
	case evPause:
		s.stats.pauses++
	case evRestart:
		s.stats.restarts++
	case evReconfigure:
		s.stats.reconfigs++
	}
}

// noteEvent отмечает события, подтверждающие выход процесса и освобождение памяти (I4), и LoadOK (I12).
func (s *sim) noteEvent(before State, ev Event) {
	switch e := ev.(type) {
	case evLoadFailed:
		s.exitOK = s.exitOK || (before.Phase == PhaseLoading && before.Gen == e.Gen)
	case evProcessExited:
		s.exitOK = s.exitOK || (before.serving() && before.Gen == e.Gen)
	case evStopped:
		s.exitOK = s.exitOK || (before.Phase == PhaseStopping && before.Gen == e.Gen)
	case evVRAMFreed:
		s.freedOK = s.freedOK || (s.exitOK && before.Phase == PhaseWaitVRAM && before.Gen == e.Gen)
	case evLoadOK:
		if before.Phase == PhaseLoading && before.Gen == e.Gen {
			s.loadOK[e.Gen] = true
		}
	}
}

func (s *sim) apply(before State, effs []Effect) {
	fatal, forcedAt := false, -1
	for i, e := range effs {
		switch e := e.(type) {
		case effGrant:
			if _, ok := s.waiting[e.ID]; !ok {
				s.fail("I5: grant to unknown or answered waiter %d", e.ID)
			}
			if s.retired[e.Gen] > 0 {
				s.fail("I12: grant for retired gen %d", e.Gen)
			}
			delete(s.waiting, e.ID)
			s.leases = append(s.leases, e)
			s.stats.grants++
		case effReject:
			if _, ok := s.waiting[e.ID]; !ok {
				s.fail("I5: reject to unknown or answered waiter %d", e.ID)
			}
			delete(s.waiting, e.ID)
		case effStartModel:
			if before.Phase != PhaseEmpty && before.Phase != PhaseWaitVRAM {
				s.fail("I4: StartModel from %s", before.Phase)
			}
			if s.lastGen > 0 && !(s.exitOK && s.freedOK) {
				s.fail("I4: StartModel gen %d without confirmed exit and VRAMFreed of gen %d", e.Gen, s.lastGen)
			}
			if s.loadOK[s.lastGen] && s.retired[s.lastGen] != 1 {
				s.fail("I12: gen %d retired %d times before next StartModel", s.lastGen, s.retired[s.lastGen])
			}
			if e.Gen != s.lastGen+1 {
				s.fail("gen jumped from %d to %d", s.lastGen, e.Gen)
			}
			if def, ok := findModel(s.st.Catalog, e.Model.ID); s.st.Paused || !ok || !reflect.DeepEqual(def, e.Model) ||
				!reflect.DeepEqual(s.st.Running, e.Model) {
				s.fail("I14: start %+v, paused %v, running %+v", e.Model, s.st.Paused, s.st.Running)
			}
			s.lastGen, s.exitOK, s.freedOK = e.Gen, false, false
			s.stats.starts++
			s.startChain(e)
		case effRetireUpstream:
			if !s.loadOK[e.Gen] {
				s.fail("I12: retire of gen %d without LoadOK", e.Gen)
			}
			if s.retired[e.Gen]++; s.retired[e.Gen] > 1 {
				s.fail("I12: gen %d retired twice", e.Gen)
			}
			switch e.Reason {
			case RetireStop, RetireExited:
			case RetireForced:
				forcedAt = i
				s.stats.forced++
			default:
				s.fail("retire of gen %d without reason", e.Gen)
			}
		case effStopModel:
			if s.st.Inflight != 0 {
				s.fail("I3: StopModel with inflight %d", s.st.Inflight)
			}
			ch := []Event{evStopped{Gen: e.Gen}}
			if !s.fixed && s.rng.IntN(100) == 0 {
				ch = []Event{evStopFailed{Gen: e.Gen, PID: int(e.Gen), Message: "unconfirmed"}}
			}
			s.chains = append(s.chains, ch)
			if s.rng.IntN(4) == 0 { // ожидаемый выход после TerminateProcess — устаревший ProcessExited
				s.chains = append(s.chains, []Event{evProcessExited{Gen: e.Gen, ExitCode: 1, Message: "terminated"}})
			}
		case effWaitVRAM:
			s.chains = append(s.chains, []Event{evVRAMFreed{Gen: e.Gen, Waited: time.Second, LastMiB: 950, TimedOut: s.rng.IntN(10) == 0}})
		case effSetTimer:
			s.timerAt, s.timerSet = e.At, true
		case effFatal:
			fatal, s.halted = true, true
			s.stats.halted++
		case effLog, effCmdResult:
		default:
			s.fail("unknown effect %T", e)
		}
	}
	if forcedAt >= 0 { // I16
		if si := indexOf[effStopModel](effs); si < forcedAt {
			s.fail("I16: StopModel at %d before forced retire at %d", si, forcedAt)
		}
		if s.st.Inflight != 0 || s.st.Upstream != nil || s.st.Phase != PhaseStopping {
			s.fail("I16: after F1 inflight %d upstream %v phase %s", s.st.Inflight, s.st.Upstream != nil, s.st.Phase)
		}
	}
	if fatal {
		for _, e := range effs {
			switch e.(type) {
			case effLog, effFatal:
			default:
				s.fail("E14: step with Fatal also emitted %T", e)
			}
		}
	}
}

func (s *sim) startChain(e effStartModel) {
	base := int64(900)
	if s.rng.IntN(5) == 0 {
		base = -1
	}
	ch := []Event{evBaseline{Gen: e.Gen, MiB: base}}
	switch r := s.rng.IntN(100); {
	case s.fixed || r < 70:
		ch = append(ch, evLoadOK{Gen: e.Gen, Upstream: &Upstream{Model: e.Model.ID, PID: int(e.Gen)}})
	case r < 99:
		ch = append(ch, evLoadFailed{Gen: e.Gen, ExitCode: 1, Message: "boom"})
	default:
		ch = append(ch, evStopFailed{Gen: e.Gen, PID: int(e.Gen), Message: "unconfirmed"})
	}
	s.chains = append(s.chains, ch)
}

// check — I1, I2, I5, I6, I7, I8, I9, I10, I12 (Upstream и «ноль, пока обслуживается»), I13, I15, I18.
func (s *sim) check() {
	st := s.st
	if v := v2Violation(st); v != "" {
		s.fail("%s", v)
	}
	busy := st.Phase == PhaseLoading || st.Phase == PhaseReady || st.Phase == PhaseDraining || st.Phase == PhaseStopping
	if (st.Current != "") != busy {
		s.fail("I1: current %q in %s", st.Current, st.Phase)
	}
	if st.Inflight < 0 || (st.Inflight > 0 && !st.serving()) {
		s.fail("I2: inflight %d in %s", st.Inflight, st.Phase)
	}
	implicit, queuedTarget, ids := 0, false, map[uint64]bool{}
	for _, w := range st.Queue {
		ids[w.ID] = true
		if !w.Explicit {
			implicit++
		}
		if w.Model == st.Target {
			queuedTarget = true
		}
		if f, ok := st.Failed[w.Model]; ok && f.Active(s.now) {
			s.fail("I6: waiter %d of failed %q", w.ID, w.Model)
		}
		if st.Phase == PhaseReady && w.Model == st.Current {
			s.fail("I8: waiter %d of current %q in ready", w.ID, w.Model)
		}
	}
	if implicit > s.p.MaxQueue {
		s.fail("I7: %d implicit waiters", implicit)
	}
	if st.Phase == PhaseEmpty && (len(st.Queue) > 0 || st.Intent != nil) {
		s.fail("I9: empty with queue %+v intent %v", st.Queue, st.Intent)
	}
	if st.Phase == PhaseDraining && st.Intent == nil && !queuedTarget {
		s.fail("I10: draining to %q without intent or waiter", st.Target)
	}
	if (st.Upstream != nil) != st.serving() {
		s.fail("I12: upstream %v in %s", st.Upstream != nil, st.Phase)
	}
	if st.serving() && s.retired[st.Gen] != 0 {
		s.fail("I12: serving gen %d already retired", st.Gen)
	}
	if len(ids) != len(s.waiting) {
		s.fail("I5: queue has %d waiters, unanswered %d", len(ids), len(s.waiting))
	}
	for id := range s.waiting {
		if !ids[id] {
			s.fail("I5: unanswered waiter %d is not queued", id)
		}
	}
	if st.serving() {
		n := 0
		for _, l := range s.leases {
			if l.Gen == st.Gen {
				n++
			}
		}
		if n != st.Inflight {
			s.fail("inflight %d, live leases %d", st.Inflight, n)
		}
	}
}

// model — чаще имя из текущего каталога, иногда любое из пула (в том числе удалённое) или неизвестное.
func (s *sim) model(unknown bool) string {
	if unknown && s.rng.IntN(20) == 0 {
		return "zz"
	}
	if len(s.st.Catalog) > 0 && s.rng.IntN(5) > 0 {
		return s.st.Catalog[s.rng.IntN(len(s.st.Catalog))].ID
	}
	return fuzzPool[s.rng.IntN(len(fuzzPool))]
}

// catalog — случайное подмножество пула; у каждого имени одно из двух определений.
func (s *sim) catalog() []config.Model {
	var c []config.Model
	for _, id := range fuzzPool {
		if s.rng.IntN(4) > 0 {
			c = append(c, fuzzDef(id, s.rng.IntN(2)))
		}
	}
	return c
}

func (s *sim) acquire(model string, explicit bool) {
	s.nextID++
	if !s.halted {
		s.waiting[s.nextID] = model
	}
	s.deliver(evAcquire{ID: s.nextID, Model: model, Explicit: explicit})
}

func (s *sim) deliverChain(inject bool) {
	if len(s.chains) == 0 {
		return
	}
	i := 0
	if inject {
		i = s.rng.IntN(len(s.chains))
	}
	ev := s.chains[i][0]
	if s.chains[i] = s.chains[i][1:]; len(s.chains[i]) == 0 {
		s.chains = slices.Delete(s.chains, i, i+1)
	}
	if lf, ok := ev.(evLoadFailed); ok && inject && !s.fixed && !s.halted && s.st.Phase == PhaseLoading && lf.Gen == s.st.Gen {
		model := s.st.Current
		switch s.rng.IntN(3) {
		case 0: // явный Acquire до LoadFailed той же модели
			s.stats.explicitBefore++
			s.acquire(model, true)
			s.deliver(ev)
			return
		case 1: // явный Acquire после LoadFailed той же модели
			s.deliver(ev)
			s.stats.explicitAfter++
			s.acquire(model, true)
			return
		}
	}
	s.deliver(ev)
}

func (s *sim) release(i int) {
	l := s.leases[i]
	s.leases = slices.Delete(s.leases, i, i+1)
	s.deliver(evRelease{Gen: l.Gen})
}

func (s *sim) fireTimer() {
	if !s.timerSet {
		return
	}
	if s.timerAt.After(s.now) {
		s.now = s.timerAt
	}
	s.timerSet = false
	s.deliver(evTimer{Kind: TimerCooldown})
}

func (s *sim) randomStep() {
	s.now = s.now.Add(time.Duration(s.rng.IntN(40_000)) * time.Millisecond)
	switch r := s.rng.IntN(100); {
	case r < 28:
		s.deliverChain(true)
	case r < 41:
		s.acquire(s.model(true), false)
	case r < 44: // F2-D2 (обзор Ф2): LoadedOnly (GET /props) — ответ в том же шаге
		m := s.model(false)
		if s.rng.IntN(2) == 0 {
			m = ""
		}
		s.nextID++
		if !s.halted {
			s.waiting[s.nextID] = m
		}
		s.deliver(evAcquire{ID: s.nextID, Model: m, LoadedOnly: true})
		if _, queued := s.waiting[s.nextID]; queued {
			s.fail("LoadedOnly acquire %d of %q was not answered in its step", s.nextID, m)
		}
	case r < 49:
		s.acquire(s.model(false), true)
	case r < 58:
		if len(s.leases) > 0 {
			s.release(s.rng.IntN(len(s.leases)))
		}
	case r < 62:
		if len(s.waiting) > 0 {
			ids := make([]uint64, 0, len(s.waiting))
			for id := range s.waiting {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			id := ids[s.rng.IntN(len(ids))]
			delete(s.waiting, id)
			s.deliver(evWaiterGone{ID: id})
		}
	case r < 68:
		s.deliver(evLoad{Model: s.model(true)})
	case r < 72:
		m := s.model(true)
		if s.rng.IntN(3) == 0 {
			m = ""
		}
		s.deliver(evUnload{Model: m, Force: s.rng.IntN(3) == 0})
	case r < 75:
		s.deliver(evRestart{Model: s.model(true)})
	case r < 77:
		s.deliver(evPause{Force: s.rng.IntN(2) == 0})
	case r < 81:
		s.deliver(evResume{})
	case r < 85:
		s.deliver(evReconfigure{Catalog: s.catalog(), Guard: s.rng.IntN(2) == 0})
	case r < 89:
		s.fireTimer()
	case r < 93:
		if !s.fixed && s.st.serving() && !s.halted {
			s.deliver(evProcessExited{Gen: s.st.Gen, ExitCode: 3, Message: "crash"})
		}
	default:
		g := s.st.Gen + 1000
		evs := []Event{evRelease{Gen: g}, evBaseline{Gen: g, MiB: 1}, evLoadOK{Gen: g}, evLoadFailed{Gen: g, ExitCode: 1, Message: "x"},
			evProcessExited{Gen: g, ExitCode: 1, Message: "x"}, evStopped{Gen: g}, evStopFailed{Gen: g, PID: 1, Message: "x"}, evVRAMFreed{Gen: g}}
		before := s.st.clone()
		s.deliver(evs[s.rng.IntN(len(evs))])
		if !reflect.DeepEqual(s.st, before) {
			s.fail("E13: stale event changed state")
		}
	}
}

// drain — бэкенд починен, все Lease освобождаются: каждый ожидающий должен разрешиться.
func (s *sim) drain() {
	s.fixed = true
	for i := 0; ; i++ {
		if s.halted {
			return
		}
		if i > 5000 {
			s.fail("drain did not converge: %d waiters, state %+v", len(s.waiting), s.st)
		}
		switch {
		case len(s.leases) > 0:
			s.release(0)
		case len(s.chains) > 0:
			s.deliverChain(false)
		case s.timerSet:
			s.fireTimer()
		default:
			if len(s.waiting) > 0 {
				s.fail("%d waiters unresolved with nothing pending: %+v", len(s.waiting), s.st)
			}
			return
		}
	}
}

func TestRandomizedInvariants(t *testing.T) {
	stats := &fuzzStats{}
	for seed := uint64(1); seed <= 500; seed++ {
		s := newSim(t, seed, stats)
		for i := 0; i < 200; i++ {
			s.randomStep()
		}
		s.drain()
	}
	t.Logf("stats %+v", *stats)
	if stats.halted == 0 || stats.explicitBefore == 0 || stats.explicitAfter == 0 || stats.grants == 0 ||
		stats.forced == 0 || stats.inUse == 0 || stats.pauses == 0 || stats.restarts == 0 || stats.reconfigs == 0 {
		t.Fatalf("coverage too low: %+v", *stats)
	}
}
