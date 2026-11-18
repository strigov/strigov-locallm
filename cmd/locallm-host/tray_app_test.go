package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/tray"
)

type fakeCtl struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeCtl) rec(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeCtl) Load(a control.Actor, m string) error {
	return f.rec(fmt.Sprintf("load %s %s", a.Kind, m))
}
func (f *fakeCtl) Unload(a control.Actor, m string, force bool) error {
	return f.rec(fmt.Sprintf("unload %s %q force=%v", a.Kind, m, force))
}
func (f *fakeCtl) Pause(a control.Actor, force bool) (bool, error) {
	return true, f.rec(fmt.Sprintf("pause %s force=%v", a.Kind, force))
}
func (f *fakeCtl) Resume(a control.Actor) (bool, error) { return true, f.rec("resume " + a.Kind) }

type fakeAuto struct {
	on    bool
	err   error
	calls []string
}

func (f *fakeAuto) Enabled() (bool, error) { return f.on, nil }
func (f *fakeAuto) Enable() error {
	f.calls = append(f.calls, "enable")
	if f.err == nil {
		f.on = true
	}
	return f.err
}
func (f *fakeAuto) Disable() error {
	f.calls = append(f.calls, "disable")
	if f.err == nil {
		f.on = false
	}
	return f.err
}

type fakeMenu struct {
	shown []bool
	langs []i18n.Lang
}

func (f *fakeMenu) SetAutostart(on bool) { f.shown = append(f.shown, on) }
func (f *fakeMenu) SetLang(l i18n.Lang)  { f.langs = append(f.langs, l) }

// trayRig — trayApp на фейках: контроллер, автозапуск, меню, окна и действия записываются.
type trayRig struct {
	app      *trayApp
	ctl      *fakeCtl
	auto     *fakeAuto
	menu     *fakeMenu
	snap     *scheduler.Snapshot
	answer   bool
	confirms []string
	dialogs  []string
	events   []string
	log      *bytes.Buffer
}

func newTrayRig() *trayRig {
	r := &trayRig{ctl: &fakeCtl{}, auto: &fakeAuto{}, menu: &fakeMenu{}, snap: &scheduler.Snapshot{}, log: &bytes.Buffer{}}
	r.app = &trayApp{
		ctl:       r.ctl,
		snapshot:  func() *scheduler.Snapshot { return r.snap },
		confirm:   func(s string) bool { r.confirms = append(r.confirms, s); return r.answer },
		dialog:    func(s string) { r.dialogs = append(r.dialogs, s) },
		openPanel: func() { r.events = append(r.events, "panel") },
		openLog:   func() error { r.events = append(r.events, "log"); return nil },
		openDir:   func() error { r.events = append(r.events, "folder"); return nil },
		auto:      r.auto,
		menu:      r.menu,
		quit:      func() { r.events = append(r.events, "quit") },
		log:       slog.New(slog.NewTextHandler(r.log, nil)),
	}
	return r
}

// §11.1: команды меню — от имени трея (actor=tray); «Выгрузить» — Unload("") без обрыва.
func TestTrayCommandsAsTray(t *testing.T) {
	r := newTrayRig()
	for _, c := range []tray.Click{{Action: tray.ActLoad, Model: "b"}, {Action: tray.ActUnload}, {Action: tray.ActPause},
		{Action: tray.ActResume}, {Action: tray.ActPanel}, {Action: tray.ActMore}, {Action: tray.ActLog}, {Action: tray.ActFolder}} {
		r.app.handle(c)
	}
	if want := []string{"load tray b", `unload tray "" force=false`, "pause tray force=false", "resume tray"}; !slices.Equal(r.ctl.calls, want) {
		t.Fatalf("controller calls %q", r.ctl.calls)
	}
	if want := []string{"panel", "panel", "log", "folder"}; !slices.Equal(r.events, want) || len(r.confirms) != 0 {
		t.Fatalf("events %q, confirms %q", r.events, r.confirms)
	}
}

// §11.1: «Прервать запросы и выгрузить» при Inflight > 0 спрашивает подтверждение; «Нет» — ничего не происходит.
func TestTrayForceConfirm(t *testing.T) {
	r := newTrayRig()
	r.snap = &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Inflight: 2}
	r.app.handle(tray.Click{Action: tray.ActForce})
	if len(r.ctl.calls) != 0 || !slices.Equal(r.confirms, []string{"Будут прерваны незаконченные ответы: 2. Продолжить?"}) {
		t.Fatalf("answer no: calls %q, confirms %q", r.ctl.calls, r.confirms)
	}
	r.answer = true
	r.app.handle(tray.Click{Action: tray.ActForce})
	r.snap = &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a"}
	r.app.handle(tray.Click{Action: tray.ActForce})
	if want := []string{`unload tray "" force=true`, `unload tray "" force=true`}; !slices.Equal(r.ctl.calls, want) || len(r.confirms) != 2 {
		t.Fatalf("calls %q, confirms %q", r.ctl.calls, r.confirms)
	}
}

// §11.6 п. 1: «Выход» при незаконченных ответах (Inflight и очередь) спрашивает подтверждение; «Нет» — ничего.
func TestTrayQuitConfirm(t *testing.T) {
	r := newTrayRig()
	r.snap = &scheduler.Snapshot{Phase: scheduler.PhaseReady, Current: "a", Inflight: 1,
		Queue: []scheduler.QueueEntry{{Model: "b", Waiters: 2}}}
	r.app.handle(tray.Click{Action: tray.ActQuit})
	if len(r.events) != 0 || !slices.Equal(r.confirms, []string{"Прервать незаконченные ответы (3) и выйти?"}) {
		t.Fatalf("answer no: events %q, confirms %q", r.events, r.confirms)
	}
	r.answer = true
	r.app.handle(tray.Click{Action: tray.ActQuit})
	r.snap = &scheduler.Snapshot{}
	r.app.handle(tray.Click{Action: tray.ActQuit})
	if !slices.Equal(r.events, []string{"quit", "quit"}) || len(r.confirms) != 2 {
		t.Fatalf("events %q, confirms %q", r.events, r.confirms)
	}
}

// §11.4, §13: флажок переключает автозапуск, меню показывает прочитанное состояние; ошибка — окно и строка журнала.
func TestTrayAutostartToggle(t *testing.T) {
	r := newTrayRig()
	r.app.handle(tray.Click{Action: tray.ActAutostart, Checked: false})
	r.app.handle(tray.Click{Action: tray.ActAutostart, Checked: true})
	if !slices.Equal(r.auto.calls, []string{"enable", "disable"}) || !slices.Equal(r.menu.shown, []bool{true, false}) {
		t.Fatalf("calls %q, shown %v", r.auto.calls, r.menu.shown)
	}
	if log := r.log.String(); !strings.Contains(log, `msg="autostart changed" enabled=true`) || !strings.Contains(log, `msg="autostart changed" enabled=false`) {
		t.Fatalf("log:\n%s", log)
	}
	r.auto.err = errors.New("access denied")
	r.app.handle(tray.Click{Action: tray.ActAutostart, Checked: false})
	if len(r.dialogs) != 1 || r.dialogs[0] != "Не удалось изменить автозапуск: access denied" ||
		!strings.Contains(r.log.String(), `msg="autostart was not changed" enabled=true err="access denied"`) ||
		r.menu.shown[len(r.menu.shown)-1] {
		t.Fatalf("dialogs %q, shown %v, log:\n%s", r.dialogs, r.menu.shown, r.log.String())
	}
}

// Ошибка команды — строка журнала с действием и моделью; окна нет.
func TestTrayCommandErrorLogged(t *testing.T) {
	r := newTrayRig()
	r.ctl.err = scheduler.ErrPaused
	r.app.handle(tray.Click{Action: tray.ActLoad, Model: "b"})
	if log := r.log.String(); !strings.Contains(log, `msg="tray command failed" action=load model=b`) || len(r.dialogs) != 0 {
		t.Fatalf("dialogs %q, log:\n%s", r.dialogs, log)
	}
}

// Подменю «Язык»: язык хоста и меню меняются, выбор сохраняется; окна дальше — на выбранном языке. Ошибка
// сохранения — строка журнала, язык всё равно меняется.
func TestTrayLanguage(t *testing.T) {
	defer i18n.Set(i18n.Current())
	r := newTrayRig()
	var saved []i18n.Lang
	r.app.saveLang = func(l i18n.Lang) error { saved = append(saved, l); return nil }
	r.app.handle(tray.Click{Action: tray.ActLang, Lang: i18n.EN})
	if i18n.Current() != i18n.EN || !slices.Equal(r.menu.langs, []i18n.Lang{i18n.EN}) || !slices.Equal(saved, []i18n.Lang{i18n.EN}) {
		t.Fatalf("current %q, menu %q, saved %q", i18n.Current(), r.menu.langs, saved)
	}
	r.snap = &scheduler.Snapshot{Phase: scheduler.PhaseReady, Inflight: 2}
	r.app.handle(tray.Click{Action: tray.ActForce})
	if !slices.Equal(r.confirms, []string{"Unfinished responses will be aborted: 2. Continue?"}) {
		t.Fatalf("confirms %q", r.confirms)
	}
	r.app.saveLang = func(i18n.Lang) error { return errors.New("disk full") }
	r.app.handle(tray.Click{Action: tray.ActLang, Lang: i18n.ZH})
	if i18n.Current() != i18n.ZH || !strings.Contains(r.log.String(), "disk full") {
		t.Fatalf("current %q, log %s", i18n.Current(), r.log)
	}
}
