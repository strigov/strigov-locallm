package scheduler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrModelNotFound — модели нет в конфиге; оборачивается как fmt.Errorf("%w: %q", ErrModelNotFound, m).
var ErrModelNotFound = errors.New("model is not configured")

// ErrModelNotLoaded — отказ AcquireLoaded: модель не в Ready (GET /props, F5-D1).
var ErrModelNotLoaded = errors.New("model is not loaded")

// ErrPaused — пауза (A0, A1p, C3, C10): 503 host_paused (§9.6 спецификации v2).
var ErrPaused = errors.New("host is paused by the administrator; requests are not accepted until it is resumed")

// ErrHalted — команда пришла после Fatal (C1): автомат остановлен и CmdResult не выдаёт.
var ErrHalted = errors.New("scheduler is halted")

// InUseError — отказ Reconfigure по RC0 (C15g): удаляемые имена заняты, в порядке Catalog; 409 model_in_use.
type InUseError struct{ Models []string }

func (e *InUseError) Error() string {
	q := make([]string, len(e.Models))
	for i, m := range e.Models {
		q[i] = strconv.Quote(m)
	}
	if len(q) == 1 {
		return fmt.Sprintf("model %s is in use (loaded, loading or requested); unload it first", q[0])
	}
	return fmt.Sprintf("models %s are in use (loaded, loading or requested); unload them first", strings.Join(q, ", "))
}

// CooldownError — A4: 409 model_cooldown.
type CooldownError struct {
	Loaded, Requested string
	RetryAfter        int // секунды, ≥ 1
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("model %q is in use; switching to %q is blocked for %d s (swap_cooldown)", e.Loaded, e.Requested, e.RetryAfter)
}

// QueueFullError — A5: 429 queue_full.
type QueueFullError struct{ MaxQueue int }

func (e *QueueFullError) Error() string {
	return fmt.Sprintf("swap queue is full (%d waiting requests)", e.MaxQueue)
}

// LoadFailedError — E8, E9, A2: 503 model_load_failed. Cached — ответ из метки failed (A2).
type LoadFailedError struct {
	Model    string
	ExitCode int
	Message  string
	Cached   bool
	RetryIn  time.Duration
}

func (e *LoadFailedError) Error() string {
	s := fmt.Sprintf("model %q failed to load (exit code %d): %s", e.Model, e.ExitCode, e.Message)
	if e.Cached {
		s += fmt.Sprintf("; cached for %d s, load it explicitly to retry", ceilSeconds(e.RetryIn))
	}
	return s
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}
