package gateway

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
)

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// usageMiB — прямой замер Probe с таймаутом 1 с; nil при ошибке или источнике none (§9.5).
func (s *server) usageMiB(ctx context.Context) any {
	if s.probe == nil || s.probe.Name() == "none" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	v, err := s.probe.UsageMiB(ctx)
	if err != nil {
		return nil
	}
	return v
}

// apiStatus — GET /api/status (§9.5).
func (s *server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusBody(r.Context(), s.sched.Snapshot(), time.Now()))
}

// statusBody — поля /api/status (§9.5); с них же начинается ответ /api/admin/state (§9.5 v2). Список failed — по
// каталогу автомата (Snapshot.Catalog, §6.8 v2), а не по live.
func (s *server) statusBody(ctx context.Context, sn *scheduler.Snapshot, now time.Time) map[string]any {
	queue := make([]any, 0, len(sn.Queue))
	for _, q := range sn.Queue {
		queue = append(queue, map[string]any{"model": q.Model, "waiters": q.Waiters, "oldest_wait_ms": now.Sub(q.OldestSince).Milliseconds()})
	}
	var intent any
	if sn.Intent != nil {
		intent = map[string]any{"action": sn.Intent.Kind.String(), "model": nullString(sn.Intent.Model)}
	}
	failed := []any{}
	for _, m := range sn.Catalog {
		if f, ok := sn.Failed[m.ID]; ok && f.Active(now) {
			failed = append(failed, map[string]any{"model": m.ID, "exit_code": f.ExitCode, "message": f.Message,
				"retry_in_ms": f.Until.Sub(now).Milliseconds()})
		}
	}
	var pid, baseline any
	if sn.PID != 0 {
		pid = sn.PID
	}
	if sn.Baseline >= 0 {
		baseline = sn.Baseline
	}
	vr := map[string]any{"source": "none", "usage_mib": s.usageMiB(ctx), "baseline_mib": baseline,
		"margin_mib": s.cfg.Swap.VRAMMarginMiB, "last_wait_ms": nil, "last_wait_timed_out": nil}
	if s.probe != nil {
		vr["source"] = s.probe.Name()
	}
	if lw := sn.LastVRAMWait; lw != nil {
		vr["last_wait_ms"], vr["last_wait_timed_out"] = lw.Waited.Milliseconds(), lw.TimedOut
	}
	return map[string]any{
		"version": s.cfg.Version, "state": sn.Phase.String(), "model": nullString(sn.Current), "target": nullString(sn.Target),
		"pid": pid, "state_since": sn.PhaseSince.UTC().Format(time.RFC3339), "inflight": sn.Inflight, "queue": queue,
		"intent": intent, "cooldown_remaining_ms": sn.CooldownRemaining(now).Milliseconds(), "failed": failed, "vram": vr,
		"paused": sn.Paused,
	}
}

// apiSwap — POST /api/swap: явная синхронная пересадка (§9.5); cooldown и max_queue не действуют (A1a).
// Обрыв клиента намерение не отменяет: WaiterGone снимает только ожидающего.
func (s *server) apiSwap(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.requestModel(w, r)
	if !ok {
		return
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), s.cfg.Swap.WaitTimeout)
	lease, err := s.sched.Acquire(waitCtx, model, true)
	cancel()
	if err != nil {
		s.reject(w, r, model, err)
		return
	}
	lease.Release()
	noteWait(r, lease.Waited)
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "state": "ready", "waited_ms": lease.Waited.Milliseconds()})
}

// apiUnload — POST /api/unload: тело не читается как команда (пустое или {}), но проходит проверки §9.1
// до выгрузки: больше 32 МиБ — 413, сжатое — 400, и тогда Unload не вызывается; 202 после обработки Unload("").
func (s *server) apiUnload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readBody(w, r); !ok {
		return // ответ (400 или 413) уже записан
	}
	if err := s.sched.Unload(""); err != nil {
		writeAPIError(w, errInternal(err))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "state": s.sched.Snapshot().Phase.String()})
}

// apiLogs — GET /api/logs?model=&tail= (§9.5): model только из конфига, tail ограничен llama.log_lines.
func (s *server) apiLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	model := q.Get("model")
	if model == "" {
		model = s.sched.Snapshot().Current // "" — LogSource берёт последнюю запускавшуюся модель
	} else {
		cfg := s.live.Load()
		if _, ok := cfg.Model(model); !ok {
			writeAPIError(w, errModelNotFound(model, cfg.ModelIDs()))
			return
		}
	}
	tail := 200
	if n, err := strconv.Atoi(q.Get("tail")); err == nil && n > 0 {
		tail = n
	}
	if limit := s.cfg.Llama.LogLines; limit > 0 && tail > limit {
		tail = limit
	}
	var lines []string
	if s.logs != nil {
		lines, _ = s.logs.Logs(model, tail)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
}
