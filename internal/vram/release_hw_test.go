//go:build hw && windows

package vram_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/hwtest"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/vram"
)

const s4Port = 8081

type cycle struct {
	baseline, readyMiB, residualMiB int64
	load, exit, release, portFree   time.Duration // release == -1: не освободилось за 60 с
}

func ceilSec(d time.Duration) time.Duration { return (d + time.Second - 1).Truncate(time.Second) }

// TestS4ReleaseAfterTerminate — для каждой большой модели 5 циклов (§14.1 S4).
func TestS4ReleaseAfterTerminate(t *testing.T) {
	exe := hwtest.LlamaExe(t)
	models := hwtest.BigModels(t)
	hwtest.NoOrphans(t)
	ctx := context.Background()
	probe, err := vram.Detect(ctx, "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Name() == "none" {
		t.Fatal("S4 needs a VRAM source (pdh or nvidia-smi)")
	}
	if closer, ok := probe.(interface{ Close() }); ok {
		defer closer.Close()
	}
	t.Logf("S4: vram source %s", probe.Name())
	cfg := hwtest.Config(t, exe, s4Port)
	cfg.Llama.LoadTimeout = 10 * time.Minute
	cfg.Llama.StopTimeout = 30 * time.Second

	var maxLoad, maxExit, maxRelease time.Duration
	var maxResidual int64
	for i, bm := range models {
		m := config.Model{ID: fmt.Sprintf("big%d", i+1), Path: bm.Path, Args: bm.Args}
		composite := strings.Contains(bm.Path, "-00001-of-")
		for n := 1; n <= 5; n++ {
			r, le := runCycle(t, ctx, cfg, probe, m)
			if le != nil {
				switch {
				case n == 1 && composite:
					t.Logf("S4 %s (%s): does not fit — reference OOM case; exit=%d tail:\n%s", m.ID, bm.Path, le.ExitCode, strings.Join(le.Tail, "\n"))
				case n == 1:
					t.Errorf("S4 %s (%s): load failed: %v\n%s", m.ID, bm.Path, le, strings.Join(le.Tail, "\n"))
				default:
					t.Errorf("S4 %s cycle %d: start after release failed (OOM?): %v\n%s", m.ID, n, le, strings.Join(le.Tail, "\n"))
				}
				break
			}
			t.Logf("S4 %s cycle %d: baseline=%d MiB load=%s ready_usage=%d MiB (model %d MiB) exit=%s release=%s residual60s=%d MiB port_free=%s",
				m.ID, n, r.baseline, r.load, r.readyMiB, r.readyMiB-r.baseline, r.exit, r.release, r.residualMiB, r.portFree)
			if r.release < 0 || r.release > 10*time.Second {
				t.Errorf("S4 %s cycle %d: release %s > 10s", m.ID, n, r.release)
			}
			if r.residualMiB > 512 {
				t.Errorf("S4 %s cycle %d: residual %d MiB > 512", m.ID, n, r.residualMiB)
			}
			if r.portFree > time.Second {
				t.Errorf("S4 %s cycle %d: port free after %s > 1s", m.ID, n, r.portFree)
			}
			maxLoad, maxExit = max(maxLoad, r.load), max(maxExit, r.exit)
			maxRelease, maxResidual = max(maxRelease, r.release), max(maxResidual, r.residualMiB)
		}
	}
	t.Logf("S4 max: load=%s exit=%s release=%s residual=%d MiB", maxLoad, maxExit, maxRelease, maxResidual)
	t.Logf("S4 calibration: vram_free_timeout=%s vram_margin_mib=%d stop_timeout=%s load_timeout>=%s wait_timeout>=%s",
		ceilSec(max(5*time.Second, 2*maxRelease)), max(256, maxResidual+256), ceilSec(max(5*time.Second, 3*maxExit)),
		ceilSec(3*maxLoad), ceilSec(maxExit+maxRelease+maxLoad+60*time.Second))
}

func runCycle(t *testing.T, ctx context.Context, cfg *config.Config, probe vram.Probe, m config.Model) (cycle, *supervisor.LoadError) {
	t.Helper()
	var r cycle
	var err error
	if r.baseline, err = probe.UsageMiB(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	s := supervisor.New(cfg, nil)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	loadCtx, cancel := context.WithTimeout(ctx, cfg.Llama.LoadTimeout)
	defer cancel()
	start := time.Now()
	up, err := s.Start(loadCtx, m)
	r.load = time.Since(start)
	if err != nil {
		var le *supervisor.LoadError
		if errors.As(err, &le) {
			return r, le
		}
		t.Fatalf("%s: %v", m.ID, err)
	}
	t.Cleanup(func() { up.Retire(scheduler.RetireStop) })
	time.Sleep(time.Second)
	if r.readyMiB, err = probe.UsageMiB(ctx); err != nil {
		t.Fatalf("ready usage: %v", err)
	}

	stopAt := time.Now() // TerminateProcess
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("%s: Stop: %v", m.ID, err)
	}
	r.exit = time.Since(stopAt)

	exitAt := time.Now()
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s4Port))
		if err == nil {
			ln.Close()
			break
		}
		if time.Since(exitAt) > 10*time.Second {
			t.Errorf("%s: port %d still busy 10s after exit: %v", m.ID, s4Port, err)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.portFree = time.Since(exitAt)

	r.release = -1
	for time.Since(stopAt) < 60*time.Second {
		if u, err := probe.UsageMiB(ctx); err == nil && u <= r.baseline+512 {
			r.release = time.Since(stopAt)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rest := 60*time.Second - time.Since(stopAt); rest > 0 {
		time.Sleep(rest)
	}
	after, err := probe.UsageMiB(ctx)
	if err != nil {
		t.Fatalf("residual: %v", err)
	}
	r.residualMiB = after - r.baseline
	return r, nil
}
