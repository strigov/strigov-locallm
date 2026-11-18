package main

import (
	"log/slog"

	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/tray"
)

// trayControl — команды контроллера, которые вызывает трей; *control.Controller их реализует.
type trayControl interface {
	Load(a control.Actor, model string) error
	Unload(a control.Actor, model string, force bool) error
	Pause(a control.Actor, force bool) (bool, error)
	Resume(a control.Actor) (bool, error)
}

var _ trayControl = (*control.Controller)(nil)

// autostarter — флажок «Запускать при входе в Windows»; desktop.Autostart его реализует.
type autostarter interface {
	Enabled() (bool, error)
	Enable() error
	Disable() error
}

// trayActor — источник команд трея (§13: actor=tray).
var trayActor = control.Actor{Kind: control.KindTray}

// trayApp — действия пунктов меню (§11.1). handle вызывается в своей горутине на каждый щелчок (§11.2): здесь можно
// ждать окно подтверждения, контроллер и диск.
type trayApp struct {
	ctl       trayControl
	snapshot  func() *scheduler.Snapshot
	confirm   func(text string) bool
	dialog    func(text string)
	openPanel func()
	openLog   func() error
	openDir   func() error
	auto      autostarter
	menu      interface {
		SetAutostart(on bool)
		SetLang(l i18n.Lang)
	}
	saveLang func(l i18n.Lang) error // nil — выбор языка не сохраняется
	quit     func()
	log      *slog.Logger
}

func (a *trayApp) handle(c tray.Click) {
	var err error
	switch c.Action {
	case tray.ActLoad:
		err = a.ctl.Load(trayActor, c.Model)
	case tray.ActUnload:
		err = a.ctl.Unload(trayActor, "", false)
	case tray.ActForce:
		if n := a.snapshot().Inflight; n > 0 && !a.confirm(i18n.Current().F(i18n.ConfirmForce, n)) {
			return
		}
		err = a.ctl.Unload(trayActor, "", true)
	case tray.ActPause:
		_, err = a.ctl.Pause(trayActor, false)
	case tray.ActResume:
		_, err = a.ctl.Resume(trayActor)
	case tray.ActPanel, tray.ActMore:
		a.openPanel()
	case tray.ActLog:
		err = a.openLog()
	case tray.ActFolder:
		err = a.openDir()
	case tray.ActAutostart:
		a.toggleAutostart(!c.Checked)
	case tray.ActQuit:
		s := a.snapshot()
		n := s.Inflight
		for _, q := range s.Queue {
			n += q.Waiters
		}
		if n > 0 && !a.confirm(i18n.Current().F(i18n.ConfirmQuit, n)) {
			return
		}
		a.quit()
	case tray.ActLang:
		a.setLang(c.Lang)
	}
	if err != nil {
		a.log.Warn("tray command failed", "action", c.Action.String(), "model", c.Model, "err", err)
	}
}

// toggleAutostart — §11.4: включить или выключить; ошибка — окно и строка журнала; флажок показывает то, что
// прочитано из реестра после попытки.
func (a *trayApp) toggleAutostart(on bool) {
	var err error
	if on {
		err = a.auto.Enable()
	} else {
		err = a.auto.Disable()
	}
	if err != nil {
		a.log.Warn("autostart was not changed", "enabled", on, "err", err)
		a.dialog(i18n.Current().F(i18n.AutostartFailed, err.Error()))
	} else {
		a.log.Info("autostart changed", "enabled", on)
	}
	if cur, rerr := a.auto.Enabled(); rerr == nil {
		a.menu.SetAutostart(cur)
	}
}

// setLang — подменю «Язык»: язык окон и меню меняется сразу; выбор сохраняется для следующего запуска (ошибка —
// строка журнала).
func (a *trayApp) setLang(l i18n.Lang) {
	i18n.Set(l)
	a.menu.SetLang(l)
	if a.saveLang == nil {
		return
	}
	if err := a.saveLang(l); err != nil {
		a.log.Warn("language choice was not saved", "language", string(l), "err", err)
		return
	}
	a.log.Info("language changed", "language", string(l))
}
