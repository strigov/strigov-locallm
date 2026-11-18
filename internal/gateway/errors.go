package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Конструкторы ошибок §9.6 (формат и запись — apierror.go, план 1).

func errInvalidRequest() *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "invalid_request", Type: "invalid_request_error",
		Message: `request body must be a JSON object with a string "model" field`}
}

func errUnauthorized() *apiError {
	return &apiError{Status: http.StatusUnauthorized, Code: "invalid_api_key", Type: "authentication_error",
		Message: "missing or invalid API key", Headers: map[string]string{"WWW-Authenticate": "Bearer"}}
}

func errModelNotFound(model string, ids []string) *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "model_not_found", Type: "invalid_request_error",
		Message: fmt.Sprintf("model %q is not configured; available: %s", model, strings.Join(ids, ", "))}
}

func errNotFound(method, path string) *apiError {
	return &apiError{Status: http.StatusNotFound, Code: "not_found", Type: "invalid_request_error",
		Message: fmt.Sprintf("no route for %s %s", method, path)}
}

func errMethodNotAllowed(method, path, allow string) *apiError {
	return &apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Type: "invalid_request_error",
		Message: fmt.Sprintf("method %s not allowed for %s", method, path), Headers: map[string]string{"Allow": allow}}
}

func errTooLarge() *apiError {
	return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Type: "invalid_request_error",
		Message: "request body exceeds 32 MiB"}
}

func errCooldown(e *scheduler.CooldownError) *apiError {
	return &apiError{Status: http.StatusConflict, Code: "model_cooldown", Type: "conflict_error", Message: e.Error(),
		Extra:   map[string]any{"loaded_model": e.Loaded, "retry_after_s": e.RetryAfter},
		Headers: map[string]string{"Retry-After": strconv.Itoa(e.RetryAfter)}}
}

func errQueueFull(e *scheduler.QueueFullError) *apiError {
	return &apiError{Status: http.StatusTooManyRequests, Code: "queue_full", Type: "rate_limit_error", Message: e.Error(),
		Headers: map[string]string{"Retry-After": "10"}}
}

func errLoadFailed(e *scheduler.LoadFailedError) *apiError {
	return &apiError{Status: http.StatusServiceUnavailable, Code: "model_load_failed", Type: "server_error", Message: e.Error(),
		Extra:   map[string]any{"model": e.Model, "exit_code": e.ExitCode, "cached": e.Cached},
		Headers: map[string]string{"x-should-retry": "false"}}
}

func errSwapTimeout(model string, wait time.Duration, phase string) *apiError {
	return &apiError{Status: http.StatusGatewayTimeout, Code: "swap_wait_timeout", Type: "server_error",
		Message: fmt.Sprintf("model %q was not ready within %d s (state: %s)", model, int(wait/time.Second), phase)}
}

// errHostPaused — 503 host_paused: пауза администратора (§9.3, §9.6 v2).
func errHostPaused() *apiError {
	return &apiError{Status: http.StatusServiceUnavailable, Code: "host_paused", Type: "server_error",
		Message: scheduler.ErrPaused.Error(), Headers: map[string]string{"x-should-retry": "false"}}
}

// cmdError — ответ на ошибку команды Load или Unload: пауза — 503; модели уже нет в каталоге автомата (гонка
// с удалением) — 404 со списком из live; ErrHalted и остановленный цикл — 500.
func (s *server) cmdError(model string, err error) *apiError {
	switch {
	case errors.Is(err, scheduler.ErrPaused):
		return errHostPaused()
	case errors.Is(err, scheduler.ErrModelNotFound):
		return errModelNotFound(model, s.live.Load().ModelIDs())
	}
	return errInternal(err)
}

// errInternal — только если цикл планировщика остановлен (в работе хоста не возникает).
func errInternal(err error) *apiError {
	return &apiError{Status: http.StatusInternalServerError, Code: "internal_error", Type: "server_error", Message: err.Error()}
}

// schedError переводит ошибку Acquire в ответ §9.6; nil — клиент ушёл, ответ не пишется (§6.9 п. 4).
func (s *server) schedError(r *http.Request, model string, err error) *apiError {
	var ce *scheduler.CooldownError
	var qe *scheduler.QueueFullError
	var le *scheduler.LoadFailedError
	switch {
	case errors.Is(err, scheduler.ErrModelNotFound):
		return errModelNotFound(model, s.live.Load().ModelIDs())
	case errors.Is(err, scheduler.ErrPaused): // A0, A1p
		return errHostPaused()
	case errors.As(err, &ce):
		return errCooldown(ce)
	case errors.As(err, &qe):
		return errQueueFull(qe)
	case errors.As(err, &le):
		return errLoadFailed(le)
	case r.Context().Err() != nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return errSwapTimeout(model, s.cfg.Swap.WaitTimeout, s.sched.Snapshot().Phase.String())
	}
	return errInternal(err)
}

// reject пишет ответ на ошибку планировщика; 409, 429, 504 и 503 из кэша — строка info в журнал (§13).
func (s *server) reject(w http.ResponseWriter, r *http.Request, model string, err error) {
	e := s.schedError(r, model, err)
	if e == nil {
		return
	}
	cached, _ := e.Extra["cached"].(bool)
	if e.Status == http.StatusConflict || e.Status == http.StatusTooManyRequests || e.Status == http.StatusGatewayTimeout ||
		(e.Status == http.StatusServiceUnavailable && cached) {
		s.log.Info("request rejected", "model", model, "code", e.Code, "retry_after_s", e.Headers["Retry-After"])
	}
	writeAPIError(w, e)
}
