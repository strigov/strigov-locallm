package scheduler

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// Phase — фаза автомата (§6.2).
type Phase int

const (
	PhaseEmpty Phase = iota
	PhaseLoading
	PhaseReady
	PhaseDraining
	PhaseStopping
	PhaseWaitVRAM
)

// String — значение поля state в /api/status (§9.5).
func (p Phase) String() string {
	switch p {
	case PhaseEmpty:
		return "empty"
	case PhaseLoading:
		return "loading"
	case PhaseReady:
		return "ready"
	case PhaseDraining:
		return "draining"
	case PhaseStopping:
		return "stopping"
	case PhaseWaitVRAM:
		return "wait_vram"
	}
	return fmt.Sprintf("phase(%d)", int(p))
}

type IntentKind int

const (
	IntentLoad IntentKind = iota + 1
	IntentUnload
	IntentRestart // §6.4 v2 C11: перезапуск текущей модели с def(m)
)

// String — значение поля intent.action в /api/status.
func (k IntentKind) String() string {
	switch k {
	case IntentLoad:
		return "load"
	case IntentRestart:
		return "restart"
	}
	return "unload"
}

// Intent — единственное явное намерение (§6.1). При IntentUnload Model "" — выгрузить всё.
type Intent struct {
	Kind  IntentKind
	Model string
}

type Waiter struct {
	ID       uint64
	Model    string
	Explicit bool
	Since    time.Time
}

// FailInfo — метка failed-sticky (§6.7.3).
type FailInfo struct {
	ExitCode int
	Message  string
	At       time.Time
	Until    time.Time
}

// Active — метка действует (проверка «лениво» по now).
func (f FailInfo) Active(now time.Time) bool { return now.Before(f.Until) }

// VRAMWait — итог последнего WaitVRAM (для Snapshot и /api/status).
type VRAMWait struct {
	Waited   time.Duration
	LastMiB  int64
	TimedOut bool
	Unknown  bool
}

// State — состояние автомата (§6.1; v2 — §6.1 спецификации v2). PID, StartedAt и LastWait нужны только Snapshot (§6.8).
type State struct {
	Phase        Phase
	Gen          uint64
	Current      string
	Upstream     *Upstream
	Target       string
	Inflight     int
	Queue        []Waiter
	Intent       *Intent
	Failed       map[string]FailInfo
	LastActivity time.Time
	Baseline     int64
	PhaseSince   time.Time
	Halted       bool
	PID          int
	StartedAt    time.Time
	LastWait     *VRAMWait
	Catalog      []config.Model // развёрнутый каталог; срез не изменяется, а заменяется целиком (Reconfigure)
	Paused       bool
	Running      config.Model // определение текущего поколения (P11); пустое, когда Current == ""
}

func initialState(now time.Time, catalog []config.Model, paused bool) State {
	return State{Phase: PhaseEmpty, Baseline: -1, PhaseSince: now, Failed: map[string]FailInfo{}, Catalog: catalog, Paused: paused}
}

// findModel — def(id): элемент каталога с этим ID.
func findModel(catalog []config.Model, id string) (config.Model, bool) {
	for _, m := range catalog {
		if m.ID == id {
			return m, true
		}
	}
	return config.Model{}, false
}

// cloneCatalog — глубокая копия каталога (Args): автомат не делит память с вызывающим.
func cloneCatalog(ms []config.Model) []config.Model {
	if ms == nil {
		return nil
	}
	out := make([]config.Model, len(ms))
	for i, m := range ms {
		m.Args = slices.Clone(m.Args)
		out[i] = m
	}
	return out
}

// restartRequired — §6.8: текущая модель (Loading, Ready, Draining) есть в каталоге и запущена не с def(Current).
func restartRequired(ph Phase, current string, running config.Model, catalog []config.Model) bool {
	if current == "" || (ph != PhaseLoading && ph != PhaseReady && ph != PhaseDraining) {
		return false
	}
	d, ok := findModel(catalog, current)
	return ok && !running.SameLaunch(d)
}

// clone — глубокая копия: step работает только с копией (I11). Catalog и Running.Args не изменяются и делятся.
func (s State) clone() State {
	c := s
	c.Queue = slices.Clone(s.Queue)
	c.Failed = make(map[string]FailInfo, len(s.Failed))
	maps.Copy(c.Failed, s.Failed)
	if s.Intent != nil {
		i := *s.Intent
		c.Intent = &i
	}
	if s.LastWait != nil {
		w := *s.LastWait
		c.LastWait = &w
	}
	return c
}

// serving — поколение Gen обслуживает запросы (Ready или Draining).
func (s State) serving() bool { return s.Phase == PhaseReady || s.Phase == PhaseDraining }

// Params — глобальные параметры автомата из конфига (§6.1). О моделях автомат знает только из State.Catalog.
type Params struct {
	SwapCooldown time.Duration
	MaxQueue     int
	FailedTTL    time.Duration
}

type TimerKind int

const TimerCooldown TimerKind = 1

func (k TimerKind) String() string { return "cooldown" }

// Event — вход автомата (§6.3).
type Event interface{ eventName() string }

type evAcquire struct {
	ID         uint64
	Model      string
	Explicit   bool
	LoadedOnly bool // только GET /props (F5-D1): см. machine.acquire
}
type evWaiterGone struct{ ID uint64 }
type evRelease struct{ Gen uint64 }
type evLoad struct{ Model string }
type evUnload struct {
	Model string
	Force bool // ForceUnload: F1 в Ready и Draining (C8)
}
type evRestart struct{ Model string }
type evPause struct{ Force bool }
type evResume struct{}
type evReconfigure struct {
	Catalog []config.Model
	Guard   bool // RC0: отказ, если удаляемое имя занято
}
type evBaseline struct {
	Gen uint64
	MiB int64
}
type evLoadOK struct {
	Gen      uint64
	Upstream *Upstream
}
type evLoadFailed struct {
	Gen      uint64
	ExitCode int
	Message  string
}
type evProcessExited struct {
	Gen      uint64
	ExitCode int
	Message  string
}
type evStopped struct{ Gen uint64 }
type evStopFailed struct {
	Gen     uint64
	PID     int
	Message string
}
type evVRAMFreed struct {
	Gen      uint64
	Waited   time.Duration
	LastMiB  int64
	TimedOut bool
	Unknown  bool
}
type evTimer struct{ Kind TimerKind }

func (evAcquire) eventName() string       { return "Acquire" }
func (evWaiterGone) eventName() string    { return "WaiterGone" }
func (evRelease) eventName() string       { return "Release" }
func (evLoad) eventName() string          { return "Load" }
func (evUnload) eventName() string        { return "Unload" }
func (evBaseline) eventName() string      { return "Baseline" }
func (evLoadOK) eventName() string        { return "LoadOK" }
func (evLoadFailed) eventName() string    { return "LoadFailed" }
func (evProcessExited) eventName() string { return "ProcessExited" }
func (evStopped) eventName() string       { return "Stopped" }
func (evStopFailed) eventName() string    { return "StopFailed" }
func (evVRAMFreed) eventName() string     { return "VRAMFreed" }
func (evTimer) eventName() string         { return "Timer" }
func (evRestart) eventName() string       { return "Restart" }
func (evPause) eventName() string         { return "Pause" }
func (evResume) eventName() string        { return "Resume" }
func (evReconfigure) eventName() string   { return "Reconfigure" }

// isCommand — событие-команда: при !Halted на него ровно один CmdResult (I17).
func isCommand(ev Event) bool {
	switch ev.(type) {
	case evLoad, evUnload, evRestart, evPause, evResume, evReconfigure:
		return true
	}
	return false
}

// Effect — выход автомата (§6.3); исполняет цикл-владелец.
type Effect interface{ isEffect() }

type effGrant struct {
	ID       uint64
	Model    string
	Gen      uint64
	Upstream *Upstream
	Waited   time.Duration
}
type effReject struct {
	ID  uint64
	Err error
}
type effStartModel struct {
	Gen   uint64
	Model config.Model // копия def(m) на момент P11 (§6.2 v2)
}
type effRetireUpstream struct {
	Gen      uint64
	Upstream *Upstream
	Reason   RetireReason
}
type effCmdResult struct{ Err error } // результат команды; nil — принята
type effStopModel struct{ Gen uint64 }
type effFatal struct{ Err error }
type effWaitVRAM struct {
	Gen      uint64
	Baseline int64
}
type effSetTimer struct {
	Kind TimerKind
	At   time.Time
}
type effLog struct {
	Level slog.Level
	Msg   string
	Attrs []slog.Attr
}

func (effGrant) isEffect()          {}
func (effReject) isEffect()         {}
func (effStartModel) isEffect()     {}
func (effRetireUpstream) isEffect() {}
func (effStopModel) isEffect()      {}
func (effFatal) isEffect()          {}
func (effWaitVRAM) isEffect()       {}
func (effSetTimer) isEffect()       {}
func (effLog) isEffect()            {}
func (effCmdResult) isEffect()      {}
