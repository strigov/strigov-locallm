package tray

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func catalog(ids ...string) []config.Model {
	m := make([]config.Model, len(ids))
	for i, id := range ids {
		m[i] = config.Model{ID: id}
	}
	return m
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// §11.1: строка состояния, цвет значка и активные пункты по таблице «фаза × пауза × Inflight».
func TestViewStatus(t *testing.T) {
	failed := map[string]scheduler.FailInfo{
		"a": {At: now.Add(-2 * time.Minute), Until: now.Add(time.Minute)},
		"b": {At: now.Add(-time.Minute), Until: now.Add(time.Minute)},
		"c": {At: now.Add(-30 * time.Second), Until: now.Add(-time.Second)}, // метка истекла
	}
	for _, tc := range []struct {
		name          string
		snap          scheduler.Snapshot
		status        string
		icon          Color
		unload, force bool
	}{
		{"empty", scheduler.Snapshot{Phase: scheduler.PhaseEmpty}, "Модель не загружена", Gray, false, false},
		{"empty failed", scheduler.Snapshot{Phase: scheduler.PhaseEmpty, Failed: failed}, "Ошибка загрузки: b", Gray, false, false},
		{"loading", scheduler.Snapshot{Phase: scheduler.PhaseLoading, Current: "a"}, "Загружается: a…", Yellow, true, false},
		{"ready", scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a"}, "Готова: a", Green, true, true},
		{"ready busy", scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Inflight: 2}, "Готова: a · отвечает (2)", Green, true, true},
		{"draining swap", scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a", Target: "b", Inflight: 1}, "Заканчивает ответы: a → b", Yellow, true, true},
		{"draining unload", scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a", Inflight: 1}, "Заканчивает ответы: a → выгрузка", Yellow, true, true},
		{"draining restart", scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a", Target: "a", Inflight: 1}, "Заканчивает ответы: a → перезапуск", Yellow, true, true},
		{"stopping", scheduler.Snapshot{Phase: scheduler.PhaseStopping, Current: "a"}, "Выгружается: a…", Yellow, true, false},
		{"wait vram", scheduler.Snapshot{Phase: scheduler.PhaseWaitVRAM}, "Освобождается видеопамять…", Yellow, false, false},
		{"wait vram target", scheduler.Snapshot{Phase: scheduler.PhaseWaitVRAM, Target: "b"}, "Освобождается видеопамять…", Yellow, true, false},
		{"paused empty", scheduler.Snapshot{Phase: scheduler.PhaseEmpty, Paused: true}, "Пауза · Модель не загружена", Blue, false, false},
		{"paused draining", scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a", Inflight: 2, Paused: true}, "Пауза · Заканчивает ответы: a → выгрузка", Blue, true, true},
		{"paused stopping", scheduler.Snapshot{Phase: scheduler.PhaseStopping, Current: "a", Paused: true}, "Пауза · Выгружается: a…", Blue, true, false},
		{"paused wait vram", scheduler.Snapshot{Phase: scheduler.PhaseWaitVRAM, Paused: true}, "Пауза · Освобождается видеопамять…", Blue, false, false},
		{"halted", scheduler.Snapshot{Phase: scheduler.PhaseStopping, Current: "a", Halted: true}, "Ошибка: хост завершается", Yellow, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.snap
			s.Catalog = catalog("a", "b")
			v := View(&s, -1, now, i18n.RU)
			if v.Status != tc.status || v.Icon != tc.icon || v.CanUnload != tc.unload || v.CanForce != tc.force || v.Paused != s.Paused {
				t.Fatalf("view %+v", v)
			}
			if v.CanLoad == s.Paused {
				t.Fatalf("CanLoad %v with paused %v", v.CanLoad, s.Paused)
			}
			if n := utf16Len(v.Tooltip); n > maxTooltip {
				t.Fatalf("tooltip %d UTF-16 units", n)
			}
		})
	}
}

// §14.2 «Трей»: каждая фаза × пауза × Inflight — строка состояния, значок, активные пункты, подсказка.
func TestViewMatrix(t *testing.T) {
	base := map[scheduler.Phase]struct {
		status string
		icon   Color
		force  bool
	}{
		scheduler.PhaseEmpty:    {"Модель не загружена", Gray, false},
		scheduler.PhaseLoading:  {"Загружается: a…", Yellow, false},
		scheduler.PhaseReady:    {"Готова: a", Green, true},
		scheduler.PhaseDraining: {"Заканчивает ответы: a → b", Yellow, true},
		scheduler.PhaseStopping: {"Выгружается: a…", Yellow, false},
		scheduler.PhaseWaitVRAM: {"Освобождается видеопамять…", Yellow, false},
	}
	for ph, want := range base {
		for _, paused := range []bool{false, true} {
			for _, inflight := range []int{0, 2} {
				s := scheduler.Snapshot{Phase: ph, Paused: paused, Inflight: inflight, Catalog: catalog("a", "b")}
				switch ph {
				case scheduler.PhaseLoading, scheduler.PhaseReady, scheduler.PhaseStopping:
					s.Current = "a"
				case scheduler.PhaseDraining:
					s.Current, s.Target = "a", "b"
				}
				v := View(&s, 3072, now, i18n.RU)
				status, icon := want.status, want.icon
				if ph == scheduler.PhaseReady && inflight > 0 {
					status += " · отвечает (2)"
				}
				if paused {
					status, icon = "Пауза · "+status, Blue
				}
				model := s.Current
				if model == "" {
					model = "пусто"
				}
				tip := fmt.Sprintf("locallm: %s · запросов: %d · VRAM 3,0 ГБ", model, inflight)
				if paused {
					tip += " · пауза"
				}
				if v.Status != status || v.Icon != icon || v.Tooltip != tip || v.CanForce != want.force ||
					v.CanUnload != (s.Current != "" || s.Target != "") || v.CanLoad == paused || v.Paused != paused {
					t.Errorf("phase %v paused %v inflight %d: %+v", ph, paused, inflight, v)
				}
			}
		}
	}
}

// §11.1: подменю — имена в порядке каталога с метками; больше 40 — «Остальные — в панели»; на паузе и с пустым
// каталогом «Загрузить» неактивен.
func TestViewLoadItems(t *testing.T) {
	s := &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Catalog: catalog("a", "b", "c"),
		Failed: map[string]scheduler.FailInfo{"c": {At: now, Until: now.Add(time.Minute)}}}
	v := View(s, -1, now, i18n.RU)
	if want := []LoadItem{{"a", "✓ a"}, {"b", "b"}, {"c", "✗ c"}}; !slices.Equal(v.Load, want) || v.More || !v.CanLoad {
		t.Fatalf("load %+v, more %v, can %v", v.Load, v.More, v.CanLoad)
	}
	s = &scheduler.Snapshot{Phase: scheduler.PhaseLoading, Current: "b", Catalog: catalog("a", "b")}
	if v := View(s, -1, now, i18n.RU); v.Load[1].Title != "… b" {
		t.Fatalf("loading mark: %+v", v.Load)
	}
	if v := View(&scheduler.Snapshot{Paused: true, Catalog: catalog("a")}, -1, now, i18n.RU); v.CanLoad {
		t.Fatal("paused: load must be disabled")
	}
	if v := View(&scheduler.Snapshot{}, -1, now, i18n.RU); v.CanLoad || len(v.Load) != 0 || v.More {
		t.Fatalf("empty catalog: %+v", v)
	}
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%02d", i)
	}
	v = View(&scheduler.Snapshot{Catalog: catalog(ids...)}, -1, now, i18n.RU)
	if len(v.Load) != MaxLoadItems || !v.More || v.Load[39].ID != "m39" {
		t.Fatalf("45 models: %d items, more %v", len(v.Load), v.More)
	}
}

// §11.1: подсказка — модель или «пусто», запросы, VRAM в ГБ с запятой, пауза; не длиннее 127 символов UTF-16,
// обрезается по границе символа с «…».
func TestTooltip(t *testing.T) {
	s := &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Inflight: 1}
	if got := View(s, 5734, now, i18n.RU).Tooltip; got != "locallm: a · запросов: 1 · VRAM 5,6 ГБ" {
		t.Fatalf("tooltip %q", got)
	}
	if got := View(&scheduler.Snapshot{Paused: true}, -1, now, i18n.RU).Tooltip; got != "locallm: пусто · запросов: 0 · пауза" {
		t.Fatalf("tooltip %q", got)
	}
	s = &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: strings.Repeat("😀", 100)} // 200 символов UTF-16
	got := View(s, 1024, now, i18n.RU).Tooltip
	if n := utf16Len(got); n > maxTooltip || n < maxTooltip-1 || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("clipped tooltip: %d UTF-16 units, %q", n, got)
	}
}

// Строка состояния и подсказка на английском и китайском; число VRAM — с точкой.
func TestViewLanguages(t *testing.T) {
	s := &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Inflight: 1, Paused: true}
	for _, tc := range []struct {
		lang            i18n.Lang
		status, tooltip string
	}{
		{i18n.EN, "Paused · Ready: a · answering (1)", "locallm: a · requests: 1 · VRAM 5.6 GB · paused"},
		{i18n.ZH, "已暂停 · 就绪：a · 正在应答（1）", "locallm: a · 请求: 1 · 显存 5.6 GB · 已暂停"},
	} {
		v := View(s, 5734, now, tc.lang)
		if v.Status != tc.status || v.Tooltip != tc.tooltip {
			t.Errorf("%s: %q / %q", tc.lang, v.Status, v.Tooltip)
		}
	}
	d := &scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a"}
	if got := View(d, -1, now, i18n.EN).Status; got != "Finishing responses: a → unload" {
		t.Errorf("draining en: %q", got)
	}
	if got := View(&scheduler.Snapshot{}, -1, now, i18n.ZH).Tooltip; got != "locallm: 无 · 请求: 0" {
		t.Errorf("empty zh: %q", got)
	}
}
