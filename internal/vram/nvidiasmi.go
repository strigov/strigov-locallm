package vram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

var smiArgs = []string{"--query-gpu=memory.used", "--format=csv,noheader,nounits"}

type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type nvidiaSMI struct {
	run     runFunc
	timeout time.Duration
}

func newNvidiaSMI(run runFunc) *nvidiaSMI {
	if run == nil {
		run = runHidden
	}
	return &nvidiaSMI{run: run, timeout: 3 * time.Second}
}

// openNvidiaSMI — источник годен, если пробный замер прошёл (код 0, вывод разобран).
func openNvidiaSMI(ctx context.Context) (Probe, error) {
	p := newNvidiaSMI(nil)
	if _, err := p.UsageMiB(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (n *nvidiaSMI) Name() string { return "nvidia-smi" }

// UsageMiB — §8.3: сумма memory.used по всем GPU, таймаут 3 с.
func (n *nvidiaSMI) UsageMiB(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	out, err := n.run(ctx, "nvidia-smi", smiArgs...)
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseSMI(out)
}

func parseSMI(out []byte) (int64, error) {
	var sum int64
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.ParseInt(line, 10, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("nvidia-smi: unexpected output %q", line)
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, errors.New("nvidia-smi: empty output")
	}
	return sum, nil
}

// runHidden запускает процесс без окна (§8.3); дочерний процесс наследует Job Object хоста (§7.1).
func runHidden(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String() + " " + string(out)); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}
