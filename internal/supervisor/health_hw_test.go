//go:build hw && windows

package supervisor

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/hwtest"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// TestS2HealthSequence — опрос /health каждые 50 мс с метками времени (§14.1 S2).
func TestS2HealthSequence(t *testing.T) {
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	args := append(append([]string{}, hwtest.DefaultArgs...), "-m", model, "--host", "127.0.0.1", "--port", "8081")
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	var seq []string
	last := ""
	for time.Since(start) < 5*time.Minute {
		state := "other"
		resp, err := client.Get("http://127.0.0.1:8081/health")
		switch {
		case err != nil && errors.Is(err, wsaeconnrefused):
			state = "refused"
		case err != nil:
			state = "error:" + err.Error()
		default:
			resp.Body.Close()
			state = http.StatusText(resp.StatusCode)
			if resp.StatusCode == 503 {
				state = "503"
			} else if resp.StatusCode == 200 {
				state = "200"
			}
		}
		if state != last {
			seq = append(seq, time.Since(start).Truncate(time.Millisecond).String()+" "+state)
			last = state
		}
		if state == "200" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("S2 sequence: %s", strings.Join(seq, " -> "))
	if last != "200" {
		t.Fatalf("never ready: %v", seq)
	}
}

func TestS2FailuresShowCauseInTail(t *testing.T) {
	exe := hwtest.LlamaExe(t)
	hwtest.NoOrphans(t)
	cases := map[string]config.Model{
		"missing model file": {ID: "missing", Path: `C:\models\does-not-exist\nothing.gguf`},
		"unknown flag":       {ID: "badflag", Path: hwtest.SmallModel(t), Args: []string{"--definitely-not-a-flag"}},
	}
	for name, m := range cases {
		s := New(hwtest.Config(t, exe, 8081), nil)
		t.Cleanup(func() { _ = s.Stop(context.Background()) })
		_, err := s.Start(context.Background(), m)
		var le *LoadError
		if !errors.As(err, &le) || le.Reason != "exited" || le.ExitCode == 0 || len(le.Tail) == 0 {
			t.Fatalf("%s: err=%v", name, err)
		}
		t.Logf("S2 %s: exit=%d tail:\n%s", name, le.ExitCode, strings.Join(le.Tail, "\n"))
	}
}

func TestS2DefaultArgsAccepted(t *testing.T) {
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	s := New(hwtest.Config(t, exe, 8081), nil)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	start := time.Now()
	up, err := s.Start(context.Background(), config.Model{ID: "tiny", Path: model})
	if err != nil {
		t.Fatalf("default_args rejected: %v", err)
	}
	t.Logf("S2 default_args accepted; load %s; pid %d", time.Since(start), up.PID)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if testutil.ProcessAlive(up.PID) {
		t.Fatal("process alive after Stop")
	}
}

func TestS2InheritedAPIKeyDoesNotEnableAuth(t *testing.T) {
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	t.Setenv("LLAMA_API_KEY", "inherited-test-key-must-not-reach-server")
	cfg := hwtest.Config(t, exe, testutil.FreePort(t))
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Llama.LoadTimeout)
	defer cancel()
	up, err := s.Start(ctx, config.Model{ID: "tiny", Path: model})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Retire(scheduler.RetireStop) })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, up.URL.String()+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: up.Transport, Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models without Authorization returned %d, want 200", resp.StatusCode)
	}
	t.Log("S2: GET /v1/models without Authorization returned 200 with LLAMA_API_KEY set in the host")
}
