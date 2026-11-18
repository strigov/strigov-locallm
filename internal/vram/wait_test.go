package vram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type step struct {
	mib int64
	err error
}

// fakeProbe отдаёт значения по порядку; после конца списка повторяет последнее.
type fakeProbe struct {
	mu    sync.Mutex
	steps []step
	calls int
}

func (f *fakeProbe) Name() string { return "fake" }

func (f *fakeProbe) UsageMiB(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := min(f.calls, len(f.steps)-1)
	f.calls++
	return f.steps[i].mib, f.steps[i].err
}

const (
	tPoll  = 10 * time.Millisecond
	tPause = 100 * time.Millisecond
)

var errBoom = errors.New("boom")

func TestWaitReleasedDefaults(t *testing.T) {
	if pollInterval != 250*time.Millisecond || unknownPause != 2*time.Second || maxErrors != 3 {
		t.Fatalf("defaults %s %s %d", pollInterval, unknownPause, maxErrors)
	}
}

func TestWaitReleasedImmediately(t *testing.T) {
	p := &fakeProbe{steps: []step{{mib: 5100}}}
	r := waitReleased(context.Background(), p, 5000, 512, time.Second, tPoll, tPause)
	if r.TimedOut || r.Unknown || r.LastMiB != 5100 || p.calls != 1 || r.Waited >= tPause {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestWaitReleasedWhenUsageDrops(t *testing.T) {
	p := &fakeProbe{steps: []step{{mib: 9000}, {mib: 7000}, {mib: 5512}}}
	r := waitReleased(context.Background(), p, 5000, 512, time.Second, tPoll, tPause)
	if r.TimedOut || r.Unknown || r.LastMiB != 5512 || p.calls != 3 || r.Waited < 2*tPoll {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestWaitReleasedTimesOut(t *testing.T) {
	p := &fakeProbe{steps: []step{{mib: 9000}}}
	r := waitReleased(context.Background(), p, 5000, 512, 200*time.Millisecond, tPoll, tPause)
	if !r.TimedOut || r.Unknown || r.LastMiB != 9000 || r.Waited < 200*time.Millisecond || r.Waited > time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestWaitReleasedThreeErrorsInARow(t *testing.T) {
	p := &fakeProbe{steps: []step{{err: errBoom}}}
	r := waitReleased(context.Background(), p, 5000, 512, time.Second, tPoll, tPause)
	if !r.Unknown || r.TimedOut || p.calls != 3 || r.LastMiB != -1 || r.Waited < tPause {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestWaitReleasedErrorCounterResets(t *testing.T) {
	p := &fakeProbe{steps: []step{{err: errBoom}, {err: errBoom}, {mib: 9000}, {err: errBoom}, {err: errBoom}, {mib: 5000}}}
	r := waitReleased(context.Background(), p, 5000, 512, time.Second, tPoll, tPause)
	if r.Unknown || r.TimedOut || r.LastMiB != 5000 || p.calls != 6 {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestWaitReleasedUnknownBaselineAndNoneSource(t *testing.T) {
	p := &fakeProbe{steps: []step{{mib: 1}}}
	r := waitReleased(context.Background(), p, -1, 512, time.Second, tPoll, tPause)
	if !r.Unknown || p.calls != 0 || r.LastMiB != -1 || r.Waited < tPause {
		t.Fatalf("baseline -1: %+v calls=%d", r, p.calls)
	}
	r = waitReleased(context.Background(), none{}, 5000, 512, time.Second, tPoll, tPause)
	if !r.Unknown || r.TimedOut || r.Waited < tPause {
		t.Fatalf("none: %+v", r)
	}
}

func TestWaitReleasedCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	p := &fakeProbe{steps: []step{{mib: 9000}}}
	r := waitReleased(ctx, p, 5000, 512, 10*time.Second, tPoll, tPause)
	if !r.Unknown || r.TimedOut || r.Waited > time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestWaitReleasedKeepsLastUsageWhenProbeFails(t *testing.T) {
	p := &fakeProbe{steps: []step{{mib: 9000}, {err: errBoom}, {err: errBoom}, {err: errBoom}}}
	r := waitReleased(context.Background(), p, 5000, 512, time.Second, tPoll, tPause)
	if !r.Unknown || r.TimedOut || r.LastMiB != 9000 || p.calls != 4 || r.Waited < tPause {
		t.Fatalf("%+v calls=%d", r, p.calls)
	}
}

func TestWaitReleasedNilProbe(t *testing.T) {
	r := waitReleased(context.Background(), nil, 5000, 512, time.Second, tPoll, tPause)
	if !r.Unknown || r.TimedOut || r.LastMiB != -1 || r.Waited < tPause {
		t.Fatalf("%+v", r)
	}
}

func TestWaitReleasedAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakeProbe{steps: []step{{mib: 5000}}}
	r := WaitReleased(ctx, p, 5000, 512, 10*time.Second)
	if !r.Unknown || r.TimedOut || r.LastMiB != -1 || r.Waited > time.Second {
		t.Fatalf("%+v", r)
	}
}

func TestWaitReleasedCancellationInterruptsUnknownPause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	r := waitReleased(ctx, nil, -1, 512, time.Second, tPoll, 10*time.Second)
	if !r.Unknown || r.TimedOut || r.LastMiB != -1 || r.Waited > time.Second {
		t.Fatalf("%+v", r)
	}
}
