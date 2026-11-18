// Package tray — значок и меню в трее (§11 спецификации v2): вид меню из Snapshot (чистая функция View), значки,
// меню над узким интерфейсом Driver и драйвер fyne.io/systray (решение S8).
package tray

import (
	"time"
	"unicode/utf16"

	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// Color — цвет точки состояния рядом с эмодзи ламы (§11.1).
type Color int

const (
	Gray   Color = iota // Empty без паузы
	Yellow              // Loading, Draining, Stopping, WaitVRAM
	Green               // Ready
	Blue                // пауза в любой фазе
)

// MaxLoadItems — имён в подменю «Загрузить» не больше 40 (§11.1).
const MaxLoadItems = 40

// maxTooltip — подсказка не длиннее 127 символов UTF-16 (§11.1).
const maxTooltip = 127

// LoadItem — пункт подменю «Загрузить»: имя модели и заголовок с меткой.
type LoadItem struct {
	ID    string
	Title string
}

// MenuView — то, что показывает трей (§11.1).
type MenuView struct {
	Status    string
	Icon      Color
	Tooltip   string
	CanLoad   bool
	Load      []LoadItem
	More      bool
	CanUnload bool
	CanForce  bool
	Paused    bool
}

// View — чистая функция §11.1: Snapshot, VRAM (vramMiB < 0 — неизвестна), время и язык → вид меню. Каталог — из
// Snapshot.Catalog.
func View(snap *scheduler.Snapshot, vramMiB int64, now time.Time, lang i18n.Lang) MenuView {
	v := MenuView{
		Status:    status(snap, now, lang),
		Icon:      icon(snap),
		Tooltip:   tooltip(snap, vramMiB, lang),
		CanUnload: snap.Current != "" || snap.Target != "",
		CanForce:  snap.Phase == scheduler.PhaseReady || snap.Phase == scheduler.PhaseDraining,
		Paused:    snap.Paused,
	}
	for i, m := range snap.Catalog {
		if i == MaxLoadItems {
			v.More = true
			break
		}
		v.Load = append(v.Load, LoadItem{ID: m.ID, Title: mark(snap.ModelStatus(m.ID, now)) + m.ID})
	}
	v.CanLoad = !snap.Paused && len(v.Load) > 0
	return v
}

// mark — метка имени в подменю (§11.1).
func mark(status string) string {
	switch status {
	case scheduler.StatusLoaded:
		return "✓ "
	case scheduler.StatusLoading:
		return "… "
	case scheduler.StatusFailed:
		return "✗ "
	}
	return ""
}

// status — строка состояния (§11.1). Draining с Target == Current — перезапуск (T37).
func status(s *scheduler.Snapshot, now time.Time, l i18n.Lang) string {
	if s.Halted {
		return l.T(i18n.StatusHalted)
	}
	var text string
	switch s.Phase {
	case scheduler.PhaseLoading:
		text = l.F(i18n.StatusLoading, s.Current)
	case scheduler.PhaseReady:
		text = l.F(i18n.StatusReady, s.Current)
		if s.Inflight > 0 {
			text += l.F(i18n.StatusAnswering, s.Inflight)
		}
	case scheduler.PhaseDraining:
		target := s.Target
		switch target {
		case "":
			target = l.T(i18n.StatusDrainUnload)
		case s.Current:
			target = l.T(i18n.StatusDrainRestart)
		}
		text = l.F(i18n.StatusDraining, s.Current, target)
	case scheduler.PhaseStopping:
		text = l.F(i18n.StatusStopping, s.Current)
	case scheduler.PhaseWaitVRAM:
		text = l.T(i18n.StatusWaitVRAM)
	default: // PhaseEmpty
		text = l.T(i18n.StatusEmpty)
		if m := freshestFailed(s, now); m != "" {
			text = l.F(i18n.StatusLoadFailed, m)
		}
	}
	if s.Paused {
		text = l.F(i18n.StatusPaused, text)
	}
	return text
}

// freshestFailed — модель с действующей меткой failed и самым поздним At; при равенстве — первая по алфавиту.
func freshestFailed(s *scheduler.Snapshot, now time.Time) string {
	best, at := "", time.Time{}
	for id, f := range s.Failed {
		if f.Active(now) && (best == "" || f.At.After(at) || (f.At.Equal(at) && id < best)) {
			best, at = id, f.At
		}
	}
	return best
}

// icon — цвет значка (§11.1): синий — пауза в любой фазе; серый — Empty; зелёный — Ready; иначе жёлтый.
func icon(s *scheduler.Snapshot) Color {
	switch {
	case s.Paused:
		return Blue
	case s.Phase == scheduler.PhaseEmpty:
		return Gray
	case s.Phase == scheduler.PhaseReady:
		return Green
	default:
		return Yellow
	}
}

// tooltip — «locallm: <модель или пусто> · запросов: N[ · VRAM N,N ГБ][ · пауза]» (§11.1) на языке l.
func tooltip(s *scheduler.Snapshot, vramMiB int64, l i18n.Lang) string {
	model := s.Current
	if model == "" {
		model = l.T(i18n.TipNone)
	}
	t := l.F(i18n.TipMain, model, s.Inflight)
	if vramMiB >= 0 {
		t += l.F(i18n.TipVRAM, l.Num1(float64(vramMiB)/1024))
	}
	if s.Paused {
		t += l.T(i18n.TipPaused)
	}
	return clipUTF16(t, maxTooltip)
}

// clipUTF16 — не длиннее limit символов UTF-16; лишнее заменяется на «…» (один символ UTF-16). Режется по границе
// руны, поэтому суррогатная пара не разрывается.
func clipUTF16(s string, limit int) string {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	if n <= limit {
		return s
	}
	n = 0
	for i, r := range s {
		if n+utf16.RuneLen(r) > limit-1 {
			return s[:i] + "…"
		}
		n += utf16.RuneLen(r)
	}
	return s
}
