//go:build hw && windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gateway"
	"github.com/strigov/strigov-locallm/internal/hwtest"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/testutil"
	"github.com/strigov/strigov-locallm/internal/vram"
)

const hwKey = "hw-swap-test-key-0123456789abcdef0123456789"

// exampleWaitTimeout — swap.wait_timeout из locallm.example.yaml (значение, принятое по S4/S7).
func exampleWaitTimeout(t *testing.T) time.Duration {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "locallm.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Swap struct {
			WaitTimeout string `yaml:"wait_timeout"`
		} `yaml:"swap"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	d, err := time.ParseDuration(doc.Swap.WaitTimeout)
	if err != nil {
		t.Fatalf("locallm.example.yaml swap.wait_timeout %q: %v", doc.Swap.WaitTimeout, err)
	}
	return d
}

type phaseStep struct {
	phase string
	at    time.Time
}

// tracePhases опрашивает Snapshot каждые 50 мс и записывает смены фаз до отмены ctx.
func tracePhases(ctx context.Context, sched *scheduler.Scheduler) <-chan []phaseStep {
	out := make(chan []phaseStep, 1)
	go func() {
		var steps []phaseStep
		last := ""
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if p := sched.Snapshot().Phase.String(); p != last {
				steps = append(steps, phaseStep{phase: p, at: time.Now()})
				last = p
			}
			select {
			case <-ctx.Done():
				out <- steps
				return
			case <-tick.C:
			}
		}
	}()
	return out
}

// formatPhases — "ready=0s draining=0s stopping=1.2s wait_vram=2.1s loading=19.8s ready=0s".
func formatPhases(steps []phaseStep, end time.Time) string {
	parts := make([]string, 0, len(steps))
	for i, s := range steps {
		next := end
		if i+1 < len(steps) {
			next = steps[i+1].at
		}
		parts = append(parts, fmt.Sprintf("%s=%s", s.phase, next.Sub(s.at).Round(10*time.Millisecond)))
	}
	return strings.Join(parts, " ")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func hwPost(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+hwKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// TestSwapHW — §14.5: хост в процессе теста (настоящие supervisor, vram, scheduler, gateway),
// 1 начальная загрузка и 10 пересадок через POST /api/swap между двумя первыми моделями LOCALLM_BIG_MODELS.
// Критерий: все 11 вызовов — 200 и Ready, ни одной метки failed, каждый вызов короче wait_timeout;
// длительности фаз — в журнале теста.
func TestSwapHW(t *testing.T) {
	exe := hwtest.LlamaExe(t)
	big := hwtest.BigModels(t)
	if len(big) < 2 {
		t.Skip("LOCALLM_BIG_MODELS needs at least two models")
	}
	hwtest.NoOrphans(t)
	if err := supervisor.EnterJob(); err != nil { // упадёт тест — Windows завершит llama-server
		t.Fatalf("EnterJob: %v", err)
	}
	cfg := hwtest.Config(t, exe, testutil.FreePort(t),
		config.Model{ID: "big1", Path: big[0].Path, Args: big[0].Args},
		config.Model{ID: "big2", Path: big[1].Path, Args: big[1].Args})
	cfg.Swap.WaitTimeout = exampleWaitTimeout(t)

	var logBuf lockedBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe, err := vram.Detect(ctx, cfg.Vram.Source, log)
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(cfg, log)
	fatal := make(chan error, 1)
	sched := scheduler.New(cfg, sup, probe, scheduler.Init{}, func(err error) { fatal <- err }, log)
	go sched.Run(ctx)
	srv := httptest.NewServer(gateway.New(config.NewLive(cfg), sched, sup, probe, [][]byte{[]byte(hwKey)}, log))
	t.Cleanup(func() {
		srv.Close()
		_ = sup.Stop(context.Background())
		if t.Failed() {
			t.Logf("host log tail:\n%s", lastLines(logBuf.String(), 200))
		}
	})
	t.Logf("vram source %s, wait_timeout %s, big1=%s, big2=%s", probe.Name(), cfg.Swap.WaitTimeout, big[0].Path, big[1].Path)

	order := []string{"big1"}
	for i := 0; i < 10; i++ {
		order = append(order, []string{"big2", "big1"}[i%2])
	}
	var longest time.Duration
	for i, model := range order {
		trCtx, trStop := context.WithCancel(ctx)
		trace := tracePhases(trCtx, sched)
		start := time.Now()
		resp, body := hwPost(t, srv.URL+"/api/swap", `{"model":"`+model+`"}`)
		took := time.Since(start)
		trStop()
		steps := <-trace
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("swap %d -> %s: %d %s\nhost log tail:\n%s", i, model, resp.StatusCode, body, lastLines(logBuf.String(), 60))
		}
		var got struct {
			Model    string `json:"model"`
			State    string `json:"state"`
			WaitedMs int64  `json:"waited_ms"`
		}
		if err := json.Unmarshal(body, &got); err != nil || got.Model != model || got.State != "ready" {
			t.Fatalf("swap %d -> %s: %v %s", i, model, err, body)
		}
		sn := sched.Snapshot()
		if sn.Phase != scheduler.PhaseReady || sn.Current != model {
			t.Fatalf("swap %d -> %s: phase %s, current %q", i, model, sn.Phase, sn.Current)
		}
		for id, f := range sn.Failed {
			if f.Active(time.Now()) {
				t.Fatalf("swap %d: model %s is marked failed: %s", i, id, f.Message)
			}
		}
		select {
		case err := <-fatal:
			t.Fatalf("swap %d: fatal: %v", i, err)
		default:
		}
		if took >= cfg.Swap.WaitTimeout {
			t.Errorf("swap %d -> %s took %s, not shorter than wait_timeout %s", i, model, took, cfg.Swap.WaitTimeout)
		}
		longest = max(longest, took)
		vw := "-"
		if lw := sn.LastVRAMWait; lw != nil {
			vw = fmt.Sprintf("%s (timed_out=%v, last %d MiB)", lw.Waited.Round(10*time.Millisecond), lw.TimedOut, lw.LastMiB)
		}
		t.Logf("swap %2d -> %s: %s (waited_ms=%d); phases: %s; vram wait: %s",
			i, model, took.Round(10*time.Millisecond), got.WaitedMs, formatPhases(steps, start.Add(took)), vw)
	}
	t.Logf("TestSwapHW: %d/%d ready (1 initial load + 10 swaps); longest %s; wait_timeout %s",
		len(order), len(order), longest.Round(time.Second), cfg.Swap.WaitTimeout)
	for _, bad := range []string{"did not exit within stop_timeout", "exit not confirmed", "model failed"} {
		if strings.Contains(logBuf.String(), bad) {
			t.Errorf("host log contains %q", bad)
		}
	}

	resp, body := hwPost(t, srv.URL+"/api/unload", "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/api/unload: %d %s", resp.StatusCode, body)
	}
	schedtest.Eventually(t, 2*time.Minute, func() bool { return sched.Snapshot().Phase == scheduler.PhaseEmpty })
	hwtest.NoOrphans(t) // после выгрузки ни одного llama-server.exe
}
