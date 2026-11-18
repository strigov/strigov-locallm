//go:build hw && windows

package supervisor

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/hwtest"
)

func nvidiaUsedMiB(t *testing.T) int64 {
	t.Helper()
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatalf("nvidia-smi: %v", err)
	}
	var sum int64
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		v, err := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
		if err != nil {
			t.Fatalf("nvidia-smi output %q", out)
		}
		sum += v
	}
	return sum
}

// TestS1KillHostKillsLlamaServer — S1, вариант 1 (из теста).
func TestS1KillHostKillsLlamaServer(t *testing.T) {
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	before := nvidiaUsedMiB(t)
	host, child := startJobHost(t, exe, model, hwtest.DefaultArgs, 8081, 5*time.Minute)
	loaded := nvidiaUsedMiB(t)
	t.Logf("S1: baseline=%d MiB, loaded=%d MiB, host=%d, llama-server=%d", before, loaded, host, child)
	killHostAndWaitChild(t, host, child)
	time.Sleep(2 * time.Second)
	out, _ := exec.Command("tasklist", "/fi", "imagename eq llama-server.exe", "/fo", "csv", "/nh").Output()
	if strings.Contains(strings.ToLower(string(out)), "llama-server.exe") {
		t.Fatalf("llama-server.exe survived:\n%s", out)
	}
	var after int64
	deadline := time.Now().Add(30 * time.Second)
	for {
		after = nvidiaUsedMiB(t)
		if after <= before+512 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("S1: after kill=%d MiB (baseline %d, margin 512)", after, before)
	if after > before+512 {
		t.Fatalf("VRAM not returned: %d > %d+512", after, before)
	}
}
