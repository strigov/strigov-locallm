//go:build hw && windows

package vram

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/strigov/strigov-locallm/internal/hwtest"
)

func smiNow(t *testing.T) int64 {
	t.Helper()
	v, err := newNvidiaSMI(nil).UsageMiB(context.Background())
	if err != nil {
		t.Fatalf("nvidia-smi: %v", err)
	}
	return v
}

func logInstances(t *testing.T, label string, inst []instance) {
	t.Helper()
	for _, i := range inst {
		t.Logf("S3 %s: instance %q = %d MiB", label, i.Name, i.Bytes>>20)
	}
}

// TestS3EnglishCounter — PdhAddEnglishCounterW при русской локали; первый замер после открытия
// валиден (счётчик мгновенный).
func TestS3EnglishCounter(t *testing.T) {
	hwtest.LlamaExe(t)
	hwtest.NoOrphans(t)
	p, err := openPDHWith(procPdhAddEnglishCounterW)
	if err != nil {
		t.Fatalf("PdhAddEnglishCounterW: %v", err)
	}
	defer p.Close()
	inst, err := p.instances()
	if err != nil || len(inst) == 0 {
		t.Fatalf("instances: %v (%d)", err, len(inst))
	}
	logInstances(t, "idle", inst)
	t.Logf("S3: %d instance(s); more than one means an extra adapter (integrated graphics or Basic Render Driver)", len(inst))
}

// TestS3LocalizedAddCounterControl — контроль: PdhAddCounterW с английским путём (ожидается отказ на русской локали).
func TestS3LocalizedAddCounterControl(t *testing.T) {
	hwtest.LlamaExe(t)
	hwtest.NoOrphans(t)
	p, err := openPDHWith(procPdhAddCounterW)
	if err != nil {
		t.Logf("S3 control: PdhAddCounterW with the English path failed (expected on a localized system): %v", err)
		return
	}
	defer p.Close()
	inst, _ := p.instances()
	t.Logf("S3 control: PdhAddCounterW with the English path worked (%d instances)", len(inst))
}

func startLlama(t *testing.T, exe, model string, port int) {
	t.Helper()
	args := append(append([]string{}, hwtest.DefaultArgs...), "-m", model, "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: 2 * time.Second}
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
	t.Fatal("llama-server not ready within 5 minutes")
}

// TestS3CompareWithNvidiaSMI — прирост usage после загрузки малой модели по PDH и nvidia-smi.
func TestS3CompareWithNvidiaSMI(t *testing.T) {
	exe, model := hwtest.LlamaExe(t), hwtest.SmallModel(t)
	hwtest.NoOrphans(t)
	p, err := openPDHWith(procPdhAddEnglishCounterW)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	pdhIdle, err := p.UsageMiB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	smiIdle := smiNow(t)
	startLlama(t, exe, model, 8081)
	time.Sleep(2 * time.Second)
	inst, _ := p.instances()
	logInstances(t, "loaded", inst)
	pdhLoaded, err := p.UsageMiB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	smiLoaded := smiNow(t)
	dPDH, dSMI := pdhLoaded-pdhIdle, smiLoaded-smiIdle
	t.Logf("S3: idle pdh=%d smi=%d offset=%d MiB; loaded pdh=%d smi=%d offset=%d MiB; delta pdh=%d smi=%d MiB",
		pdhIdle, smiIdle, pdhIdle-smiIdle, pdhLoaded, smiLoaded, pdhLoaded-smiLoaded, dPDH, dSMI)
	allowed := max(int64(math.Round(0.05*float64(dSMI))), 128)
	diff := dPDH - dSMI
	if diff < 0 {
		diff = -diff
	}
	if diff > allowed {
		t.Fatalf("delta differs by %d MiB > %d MiB", diff, allowed)
	}
}
