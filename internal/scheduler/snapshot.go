package scheduler

import (
	"maps"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// QueueEntry — агрегат очереди по модели (§6.8).
type QueueEntry struct {
	Model       string
	Waiters     int
	OldestSince time.Time
}

// Snapshot — неизменяемая копия, которую владелец публикует после каждого события (§6.8).
type Snapshot struct {
	Phase        Phase
	PhaseSince   time.Time
	Gen          uint64
	Current      string
	Target       string
	Inflight     int
	Intent       *Intent
	Queue        []QueueEntry
	LastActivity time.Time
	SwapCooldown time.Duration
	Baseline     int64
	LastVRAMWait *VRAMWait
	Failed       map[string]FailInfo
	PID          int
	StartedAt    time.Time
	Halted       bool
	Paused       bool
	Running      config.Model   // определение текущего поколения (§6.8 v2)
	Catalog      []config.Model // тот же неизменяемый срез, что в состоянии автомата
}

const (
	StatusLoaded   = "loaded"
	StatusLoading  = "loading"
	StatusUnloaded = "unloaded"
	StatusFailed   = "failed"
)

func newSnapshot(st State, p Params) *Snapshot {
	s := &Snapshot{Phase: st.Phase, PhaseSince: st.PhaseSince, Gen: st.Gen, Current: st.Current, Target: st.Target,
		Inflight: st.Inflight, LastActivity: st.LastActivity, SwapCooldown: p.SwapCooldown, Baseline: st.Baseline,
		Failed: make(map[string]FailInfo, len(st.Failed)), PID: st.PID, StartedAt: st.StartedAt, Halted: st.Halted,
		Paused: st.Paused, Running: st.Running, Catalog: st.Catalog}
	maps.Copy(s.Failed, st.Failed)
	if st.Intent != nil {
		i := *st.Intent
		s.Intent = &i
	}
	if st.LastWait != nil {
		w := *st.LastWait
		s.LastVRAMWait = &w
	}
	idx := map[string]int{}
	for _, w := range st.Queue {
		if i, ok := idx[w.Model]; ok {
			s.Queue[i].Waiters++
			continue
		}
		idx[w.Model] = len(s.Queue)
		s.Queue = append(s.Queue, QueueEntry{Model: w.Model, Waiters: 1, OldestSince: w.Since})
	}
	return s
}

// ModelStatus — статус модели для /models и /v1/models (§6.8, правила по порядку).
func (s *Snapshot) ModelStatus(id string, now time.Time) string {
	switch {
	case id == s.Current && (s.Phase == PhaseReady || s.Phase == PhaseDraining): // 1
		return StatusLoaded
	case id == s.Current && s.Phase == PhaseLoading: // 2
		return StatusLoading
	case id == s.Target && (s.Phase == PhaseDraining || s.Phase == PhaseStopping || s.Phase == PhaseWaitVRAM): // 3
		return StatusLoading
	case s.Intent != nil && s.Intent.Kind != IntentUnload && s.Intent.Model == id && s.Phase == PhaseLoading: // 4: Load или Restart
		return StatusLoading
	}
	if f, ok := s.Failed[id]; ok && f.Active(now) { // 5
		return StatusFailed
	}
	return StatusUnloaded // 6
}

// RestartRequired — §6.8 v2: текущая модель в Loading, Ready или Draining есть в каталоге и запущена не с её
// текущим определением. Перезапуска может требовать только текущая модель.
func (s *Snapshot) RestartRequired() bool {
	return restartRequired(s.Phase, s.Current, s.Running, s.Catalog)
}

// CooldownRemaining — остаток cooldown в момент чтения (§9.5): при Inflight > 0 — полный swap_cooldown;
// вне Ready и при swap_cooldown 0 — 0.
func (s *Snapshot) CooldownRemaining(now time.Time) time.Duration {
	if s.Phase != PhaseReady || s.SwapCooldown <= 0 {
		return 0
	}
	if s.Inflight > 0 {
		return s.SwapCooldown
	}
	if r := s.LastActivity.Add(s.SwapCooldown).Sub(now); r > 0 {
		return r
	}
	return 0
}
