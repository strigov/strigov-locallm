package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

func TestSnapshotCopiesAndAggregatesQueue(t *testing.T) {
	st := initialState(t0, testCatalog("a", "b", "c"), false)
	st.Queue = []Waiter{{ID: 1, Model: "b", Since: t0}, {ID: 2, Model: "c", Since: t0.Add(time.Second)}, {ID: 3, Model: "b", Since: t0.Add(2 * time.Second)}}
	st.Intent = &Intent{Kind: IntentLoad, Model: "c"}
	st.Failed["a"] = FailInfo{ExitCode: 1, Until: t0.Add(time.Minute)}
	st.LastWait = &VRAMWait{Waited: time.Second}
	s := newSnapshot(st, testParams())
	if len(s.Queue) != 2 || s.Queue[0] != (QueueEntry{Model: "b", Waiters: 2, OldestSince: t0}) ||
		s.Queue[1] != (QueueEntry{Model: "c", Waiters: 1, OldestSince: t0.Add(time.Second)}) {
		t.Fatalf("queue %+v", s.Queue)
	}
	if s.SwapCooldown != 30*time.Second {
		t.Fatalf("cooldown %v", s.SwapCooldown)
	}
	s.Intent.Model = "x"
	s.Failed["z"] = FailInfo{}
	s.LastVRAMWait.Waited = 0
	if st.Intent.Model != "c" || len(st.Failed) != 1 || st.LastWait.Waited != time.Second {
		t.Fatal("snapshot shares memory with state")
	}
}

func TestSnapshotModelStatusRules(t *testing.T) {
	h := newHarness(t, testParams())
	status := func(id string) string { return newSnapshot(h.st, h.p).ModelStatus(id, h.now) }
	h.ready("a")
	if status("a") != StatusLoaded || status("b") != StatusUnloaded {
		t.Fatalf("ready: a=%s b=%s", status("a"), status("b"))
	}
	h.do(acq(1, "a"))
	h.do(evLoad{Model: "b"})
	if status("a") != StatusLoaded || status("b") != StatusLoading { // правила 1 и 3
		t.Fatalf("draining: a=%s b=%s", status("a"), status("b"))
	}
	h.do(evRelease{Gen: 1})
	h.mustState(PhaseStopping, "a", "b")
	if status("a") != StatusUnloaded || status("b") != StatusLoading {
		t.Fatalf("stopping: a=%s b=%s", status("a"), status("b"))
	}
	h.do(evStopped{Gen: 1})
	h.do(evVRAMFreed{Gen: 1})
	h.do(evLoadFailed{Gen: 2, ExitCode: 1, Message: "boom"})
	if status("b") != StatusFailed { // правило 5
		t.Fatalf("failed: b=%s", status("b"))
	}
	h.at(121 * time.Second)
	if status("b") != StatusUnloaded { // метка истекла — лениво по now
		t.Fatalf("expired: b=%s", status("b"))
	}
}

func TestSnapshotCooldownRemaining(t *testing.T) {
	h := newHarness(t, testParams())
	h.ready("a") // LastActivity = t0
	s := newSnapshot(h.st, h.p)
	if r := s.CooldownRemaining(t0.Add(10 * time.Second)); r != 20*time.Second {
		t.Fatalf("remaining %v", r)
	}
	if r := s.CooldownRemaining(t0.Add(40 * time.Second)); r != 0 {
		t.Fatalf("after cooldown %v", r)
	}
	h.do(acq(1, "a"))
	if r := newSnapshot(h.st, h.p).CooldownRemaining(t0.Add(time.Hour)); r != 30*time.Second {
		t.Fatalf("with inflight %v", r)
	}
	p := testParams()
	p.SwapCooldown = 0
	if r := newSnapshot(h.st, p).CooldownRemaining(t0); r != 0 {
		t.Fatalf("cooldown off %v", r)
	}
	if r := newSnapshot(initialState(t0, nil, false), h.p).CooldownRemaining(t0); r != 0 {
		t.Fatalf("empty %v", r)
	}
}

// RestartRequired — §6.8 v2: только текущая модель в Loading, Ready, Draining, запущенная не с def(Current).
func TestSnapshotRestartRequired(t *testing.T) {
	h := newHarness(t, testParams())
	rr := func() bool { return newSnapshot(h.st, h.p).RestartRequired() }
	if rr() {
		t.Fatal("empty: restart required")
	}
	h.do(evLoad{Model: "a"})
	h.do(evReconfigure{Catalog: []config.Model{mdlCtx("a", 8192), mdl("b"), mdl("c")}, Guard: true})
	if !rr() { // Loading с прежним определением
		t.Fatal("loading with old args: restart not required")
	}
	sn := newSnapshot(h.st, h.p)
	if !reflect.DeepEqual(sn.Running, mdl("a")) || len(sn.Catalog) != 3 || sn.Paused {
		t.Fatalf("snapshot running %+v catalog %d paused %v", sn.Running, len(sn.Catalog), sn.Paused)
	}
	h.do(evBaseline{Gen: 1, MiB: 900})
	h.do(evLoadOK{Gen: 1, Upstream: h.up(1, "a")})
	if !rr() {
		t.Fatal("ready with old args: restart not required")
	}
	h.do(evReconfigure{Catalog: testCatalog("a", "b", "c"), Guard: true}) // вернули прежние флаги
	if rr() {
		t.Fatal("same launch: restart required")
	}
	h.do(evReconfigure{Catalog: []config.Model{mdl("a"), mdlCtx("b", 8192), mdl("c")}, Guard: true})
	if rr() {
		t.Fatal("changed b (not current): restart required")
	}
}
