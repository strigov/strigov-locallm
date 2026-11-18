package tray

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// fakeDriver — драйвер в памяти: пункты, их состояние и журнал вызовов.
type fakeDriver struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	top     []*fakeItem // верхний уровень по порядку; разделитель — пункт "---"
	tooltip string
}

type fakeItem struct {
	d        *fakeDriver
	title    string
	enabled  bool
	checked  bool
	visible  bool
	checkbox bool
	sub      []*fakeItem
	ch       chan struct{}
}

func newFakeDriver(t *testing.T) *fakeDriver { return &fakeDriver{t: t} }

func (d *fakeDriver) newItem(title string) *fakeItem {
	return &fakeItem{d: d, title: title, enabled: true, visible: true, ch: make(chan struct{})}
}

func (d *fakeDriver) record(format string, a ...any) {
	d.calls = append(d.calls, fmt.Sprintf(format, a...))
}

func (d *fakeDriver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
}

func (d *fakeDriver) log() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *fakeDriver) Run(func()) error { return nil }
func (d *fakeDriver) Quit()            {}

func (d *fakeDriver) SetIcon(c Color) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record("icon %d", c)
}

func (d *fakeDriver) SetTooltip(text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tooltip = text
	d.record("tooltip %s", text)
}

func (d *fakeDriver) AddItem(title string) Item {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.newItem(title)
	d.top = append(d.top, it)
	d.record("add %s", title)
	return it
}

func (d *fakeDriver) AddCheckbox(title string, checked bool) Item {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.newItem(title)
	it.checkbox, it.checked = true, checked
	d.top = append(d.top, it)
	d.record("add %s", title)
	return it
}

func (d *fakeDriver) AddSeparator() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.top = append(d.top, &fakeItem{title: "---", enabled: true, visible: true})
	d.record("separator")
}

// change — изменение пункта. У fyne.io/systray на Windows любое изменение скрытого пункта, кроме Show, снова
// вставляет его в меню, поэтому фейк считает такое изменение ошибкой.
func (i *fakeItem) change(op string, f func()) {
	i.d.mu.Lock()
	defer i.d.mu.Unlock()
	if !i.visible && op != "show" {
		i.d.t.Errorf("%s on hidden item %q: fyne.io/systray would show it again", op, i.title)
	}
	f()
	i.d.record("%s %s", op, i.title)
}

func (i *fakeItem) SetTitle(title string)    { i.change("title", func() { i.title = title }) }
func (i *fakeItem) Enable()                  { i.change("enable", func() { i.enabled = true }) }
func (i *fakeItem) Disable()                 { i.change("disable", func() { i.enabled = false }) }
func (i *fakeItem) Check()                   { i.change("check", func() { i.checked = true }) }
func (i *fakeItem) Uncheck()                 { i.change("uncheck", func() { i.checked = false }) }
func (i *fakeItem) Show()                    { i.change("show", func() { i.visible = true }) }
func (i *fakeItem) Hide()                    { i.change("hide", func() { i.visible = false }) }
func (i *fakeItem) Clicked() <-chan struct{} { return i.ch }

func (i *fakeItem) AddSubCheckbox(title string, checked bool) Item {
	i.d.mu.Lock()
	defer i.d.mu.Unlock()
	it := i.d.newItem(title)
	it.checkbox, it.checked = true, checked
	i.sub = append(i.sub, it)
	i.d.record("sub %s", title)
	return it
}

func (i *fakeItem) AddSubItem(title string) Item {
	i.d.mu.Lock()
	defer i.d.mu.Unlock()
	it := i.d.newItem(title)
	i.sub = append(i.sub, it)
	i.d.record("sub %s", title)
	return it
}

// snapshotItem — копия состояния пункта под мьютексом драйвера.
func (d *fakeDriver) item(get func() *fakeItem) fakeItem {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *get()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 2 s")
		}
	}
}

// §11.1: пункты верхнего уровня и их порядок; строка состояния неактивна.
func TestMenuLayout(t *testing.T) {
	d := newFakeDriver(t)
	NewMenu(d, i18n.RU, func(Click) {})
	var got []string
	for _, it := range d.top {
		s := it.title
		if !it.enabled {
			s += " (off)"
		}
		if it.checkbox {
			s += " [ ]"
		}
		got = append(got, s)
	}
	want := []string{"locallm-host (off)", "---", ru(i18n.MenuLoad), ru(i18n.MenuUnload), ru(i18n.MenuForce), ru(i18n.MenuPause), "---",
		ru(i18n.MenuPanel), ru(i18n.MenuLog), ru(i18n.MenuFolder), ru(i18n.MenuAutostart) + " [ ]", i18n.RU.T(i18n.MenuLanguage), "---", ru(i18n.MenuQuit)}
	if !slices.Equal(got, want) {
		t.Fatalf("menu:\n%s", strings.Join(got, "\n"))
	}
}

// §11.2: драйверу передаются только изменения; значок — только при смене цвета.
func TestMenuAppliesOnlyChanges(t *testing.T) {
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(Click) {})
	s := scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Catalog: catalog("a", "b")}
	m.Apply(View(&s, -1, now, i18n.RU))
	d.reset()
	m.Apply(View(&s, -1, now, i18n.RU))
	if calls := d.log(); len(calls) != 0 {
		t.Fatalf("same view: %v", calls)
	}
	busy := s
	busy.Inflight = 1
	m.Apply(View(&busy, -1, now, i18n.RU))
	if calls, want := d.log(), []string{"tooltip locallm: a · запросов: 1", "title Готова: a · отвечает (1)"}; !slices.Equal(calls, want) {
		t.Fatalf("inflight change: %v", calls)
	}
	icons := func() int {
		n := 0
		for _, c := range d.log() {
			if strings.HasPrefix(c, "icon ") {
				n++
			}
		}
		return n
	}
	d.reset()
	draining := scheduler.Snapshot{Phase: scheduler.PhaseDraining, Current: "a", Target: "b", Catalog: s.Catalog}
	m.Apply(View(&draining, -1, now, i18n.RU))
	stopping := scheduler.Snapshot{Phase: scheduler.PhaseStopping, Current: "a", Catalog: s.Catalog}
	m.Apply(View(&stopping, -1, now, i18n.RU))
	if n := icons(); n != 1 {
		t.Fatalf("green → yellow → yellow: %d icon changes", n)
	}
}

// §11.2: пункты подменю не удаляются — переименовываются, лишние скрываются, недостающие добавляются; «Остальные —
// в панели» всегда последний; у скрытых пунктов меняется только видимость (проверяет фейк).
func TestMenuLoadSlots(t *testing.T) {
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(Click) {})
	load := d.top[2]
	apply := func(ids ...string) { m.Apply(View(&scheduler.Snapshot{Catalog: catalog(ids...)}, -1, now, i18n.RU)) }
	apply("a", "b", "c")
	apply("a", "b")
	if len(load.sub) != 3 || load.sub[2].visible {
		t.Fatalf("after shrink: %d items, third visible %v", len(load.sub), load.sub[2].visible)
	}
	d.reset()
	apply("x", "y", "z", "w")
	if calls, want := d.log(), []string{"title x", "title y", "show c", "title z", "sub w"}; !slices.Equal(calls, want) {
		t.Fatalf("rename and grow: %v", calls)
	}
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%02d", i)
	}
	apply(ids...)
	if len(load.sub) != MaxLoadItems+1 || load.sub[MaxLoadItems].title != ru(i18n.MenuMore) || !load.sub[MaxLoadItems].visible {
		t.Fatalf("45 models: %d items, last %q", len(load.sub), load.sub[len(load.sub)-1].title)
	}
	apply("a")
	visible := 0
	for _, it := range load.sub {
		if it.visible {
			visible++
		}
	}
	if visible != 1 || load.sub[MaxLoadItems].visible {
		t.Fatalf("one model: %d visible items, more visible %v", visible, load.sub[MaxLoadItems].visible)
	}
}

// §11.2: щелчок — Click с действием по показанному меню; на каждый щелчок onClick в новой горутине.
func TestMenuClicks(t *testing.T) {
	clicks := make(chan Click, 8)
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(c Click) { clicks <- c })
	m.Apply(View(&scheduler.Snapshot{Catalog: catalog("a", "b")}, -1, now, i18n.RU))
	click := func(it *fakeItem) Click {
		t.Helper()
		it.ch <- struct{}{}
		select {
		case c := <-clicks:
			return c
		case <-time.After(2 * time.Second):
			t.Fatal("no click")
			return Click{}
		}
	}
	load := d.top[2]
	if c := click(load.sub[1]); c != (Click{Action: ActLoad, Model: "b"}) {
		t.Fatalf("load b: %+v", c)
	}
	for i, want := range map[int]Action{3: ActUnload, 4: ActForce, 5: ActPause, 7: ActPanel, 8: ActLog, 9: ActFolder, 13: ActQuit} {
		if c := click(d.top[i]); c.Action != want {
			t.Fatalf("item %d %q: %+v, want %v", i, d.top[i].title, c, want)
		}
	}
	m.Apply(View(&scheduler.Snapshot{Paused: true, Catalog: catalog("a", "b")}, -1, now, i18n.RU))
	if d.item(func() *fakeItem { return d.top[5] }).title != ru(i18n.MenuResume) || click(d.top[5]).Action != ActResume {
		t.Fatal("paused: the pause item must say «Продолжить» and resume")
	}
	m.SetAutostart(true)
	if !d.item(func() *fakeItem { return d.top[10] }).checked {
		t.Fatal("autostart checkbox not checked")
	}
	if c := click(d.top[10]); c != (Click{Action: ActAutostart, Checked: true}) {
		t.Fatalf("autostart: %+v", c)
	}
	d.reset()
	m.SetAutostart(true)
	if calls := d.log(); len(calls) != 0 {
		t.Fatalf("same autostart state: %v", calls)
	}
	m.Apply(View(&scheduler.Snapshot{Catalog: catalog("a")}, -1, now, i18n.RU))
	load.sub[1].ch <- struct{}{} // скрытый пункт: щелчок, пришедший с опозданием, ничего не делает
	select {
	case c := <-clicks:
		t.Fatalf("hidden item clicked: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
	if ActForce.String() != "force_unload" || ActLoad.String() != "load" || ActQuit.String() != "quit" {
		t.Fatal("action names")
	}
}

// §11.2: зависший источник VRAM не задерживает обновление по Snapshot; Follow останавливается по ctx.
func TestFollowDoesNotWaitForVRAM(t *testing.T) {
	prev := [4]time.Duration{snapshotEvery, vramEvery, vramTimeout, autostartEvery}
	t.Cleanup(func() { snapshotEvery, vramEvery, vramTimeout, autostartEvery = prev[0], prev[1], prev[2], prev[3] })
	snapshotEvery, vramEvery, vramTimeout, autostartEvery = 5*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(Click) {})
	var snap atomic.Pointer[scheduler.Snapshot]
	snap.Store(&scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a"})
	hung := make(chan struct{})
	defer close(hung)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Follow(ctx, Source{
			Snapshot:  snap.Load,
			VRAM:      func(context.Context) (int64, error) { <-hung; return 0, nil }, // не смотрит на ctx
			Autostart: func() (bool, error) { return true, nil },
		})
		close(done)
	}()
	status := func() string { return d.item(func() *fakeItem { return d.top[0] }).title }
	waitFor(t, func() bool {
		return status() == "Готова: a" && d.item(func() *fakeItem { return d.top[10] }).checked
	})
	snap.Store(&scheduler.Snapshot{Phase: scheduler.PhaseEmpty})
	waitFor(t, func() bool { return status() == "Модель не загружена" })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not stop")
	}
}

// §11.1: VRAM из источника попадает в подсказку.
func TestFollowVRAMTooltip(t *testing.T) {
	prev := [2]time.Duration{snapshotEvery, vramEvery}
	t.Cleanup(func() { snapshotEvery, vramEvery = prev[0], prev[1] })
	snapshotEvery, vramEvery = 5*time.Millisecond, 5*time.Millisecond
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(Click) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Follow(ctx, Source{
		Snapshot: func() *scheduler.Snapshot { return &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a"} },
		VRAM:     func(context.Context) (int64, error) { return 2048, nil },
	})
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.tooltip == "locallm: a · запросов: 0 · VRAM 2,0 ГБ"
	})
}

// Подменю «Язык»: по пункту на язык с галочкой у текущего; щелчок — ActLang; SetLang переименовывает пункты.
func TestMenuLanguage(t *testing.T) {
	clicks := make(chan Click, 8)
	d := newFakeDriver(t)
	m := NewMenu(d, i18n.RU, func(c Click) { clicks <- c })
	lang := d.top[11]
	if lang.title != i18n.RU.T(i18n.MenuLanguage) || len(lang.sub) != 3 {
		t.Fatalf("language item %q with %d sub items", lang.title, len(lang.sub))
	}
	for i, l := range i18n.All {
		if s := lang.sub[i]; s.title != l.Native() || !s.checkbox || s.checked != (l == i18n.RU) {
			t.Fatalf("sub item %d: %+v", i, *s)
		}
	}
	lang.sub[1].ch <- struct{}{}
	select {
	case c := <-clicks:
		if c != (Click{Action: ActLang, Lang: i18n.EN}) {
			t.Fatalf("click: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no click")
	}
	m.Apply(View(&scheduler.Snapshot{Paused: true, Catalog: catalog("a")}, -1, now, i18n.RU))
	m.SetLang(i18n.EN)
	if d.top[2].title != "Load" || d.top[5].title != "Resume" || d.top[12].title != "---" || d.top[13].title != "Exit" ||
		d.top[10].title != "Start when signing in to Windows" || lang.title != "Language" {
		t.Fatalf("titles after SetLang: %q %q %q %q", d.top[2].title, d.top[5].title, d.top[13].title, lang.title)
	}
	if lang.sub[0].checked || !lang.sub[1].checked || lang.sub[2].checked {
		t.Fatal("check marks after SetLang")
	}
	d.reset()
	m.SetLang(i18n.EN)
	if calls := d.log(); len(calls) != 0 {
		t.Fatalf("same language: %v", calls)
	}
	m.Apply(View(&scheduler.Snapshot{Paused: true, Catalog: catalog("a")}, -1, now, m.Lang()))
	if d.top[0].title != "Paused · No model loaded" {
		t.Fatalf("status: %q", d.top[0].title)
	}
}

func ru(k i18n.Key) string { return i18n.RU.T(k) }
