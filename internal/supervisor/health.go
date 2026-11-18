package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// wsaeconnrefused — WSAECONNREFUSED (10061): порт ещё не слушает.
const wsaeconnrefused = syscall.Errno(10061)

// waitReady опрашивает /health (§7.3): отказ и 503 — продолжать; 200 — готово; иное — продолжать,
// записав в журнал один раз; выход процесса — LoadError{exited}; истёк ctx — Stop и LoadError{timeout}
// либо ErrStopUnconfirmed.
func (s *Supervisor) waitReady(ctx context.Context, p *proc, u *url.URL) error {
	healthURL := u.String() + "/health"
	tick := time.NewTicker(s.pollInterval)
	defer tick.Stop()
	logged := false
	for {
		select {
		case <-p.exited:
			s.log.Error("llama-server exited during load", "model", p.model, "pid", p.pid, "gen", p.gen,
				"exit_code", p.info.Code, "tail", p.info.Tail)
			return &LoadError{Reason: "exited", ExitCode: p.info.Code, Tail: p.info.Tail}
		case <-ctx.Done():
			s.log.Error("llama-server not ready within load_timeout; stopping", "model", p.model, "pid", p.pid, "gen", p.gen)
			if err := s.stopProc(context.Background(), p); err != nil {
				return err
			}
			return &LoadError{Reason: "timeout", ExitCode: p.info.Code, Tail: p.info.Tail}
		case <-tick.C:
		}
		status, err := s.probeHealth(ctx, healthURL)
		switch {
		case err == nil && status == http.StatusOK:
			return nil
		case err == nil && status == http.StatusServiceUnavailable:
			s.log.Debug("llama-server loading", "model", p.model)
		case err != nil && (errors.Is(err, wsaeconnrefused) || ctx.Err() != nil):
		default:
			if !logged {
				s.log.Warn("unexpected /health answer", "model", p.model, "status", status, "err", err)
				logged = true
			}
		}
	}
}

func (s *Supervisor) probeHealth(ctx context.Context, healthURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.health.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}
