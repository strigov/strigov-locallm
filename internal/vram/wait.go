package vram

import (
	"context"
	"time"
)

// WaitResult — итог ожидания освобождения VRAM (§8.4).
type WaitResult struct {
	Waited   time.Duration
	LastMiB  int64 // -1 — неизвестно
	TimedOut bool
	Unknown  bool
}

const (
	pollInterval = 250 * time.Millisecond
	unknownPause = 2 * time.Second
	maxErrors    = 3
)

// WaitReleased ждёт usage ≤ baseline + margin (§8.4). baseline == -1 или источник none — пауза 2 с,
// Unknown. Три ошибки замера подряд — пауза 2 с, Unknown. Истёк timeout — TimedOut.
// Отмена ctx — немедленный возврат с Unknown (решение плана; нужно при остановке хоста).
func WaitReleased(ctx context.Context, p Probe, baselineMiB, marginMiB int64, timeout time.Duration) WaitResult {
	return waitReleased(ctx, p, baselineMiB, marginMiB, timeout, pollInterval, unknownPause)
}

func waitReleased(ctx context.Context, p Probe, baseline, margin int64, timeout, poll, pause time.Duration) WaitResult {
	start := time.Now()
	res := WaitResult{LastMiB: -1}
	unknown := func(doPause bool) WaitResult {
		if doPause {
			sleepCtx(ctx, pause)
		}
		res.Unknown, res.Waited = true, time.Since(start)
		return res
	}
	if baseline < 0 || p == nil || p.Name() == "none" {
		return unknown(true)
	}
	deadline := start.Add(timeout)
	errs := 0
	for {
		usage, err := p.UsageMiB(ctx)
		if ctx.Err() != nil {
			return unknown(false)
		}
		if err != nil {
			if errs++; errs >= maxErrors {
				return unknown(true)
			}
		} else {
			errs = 0
			res.LastMiB = usage
			if usage <= baseline+margin {
				res.Waited = time.Since(start)
				return res
			}
		}
		if !time.Now().Before(deadline) {
			res.TimedOut, res.Waited = true, time.Since(start)
			return res
		}
		if !sleepCtx(ctx, poll) {
			return unknown(false)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
