//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/strigov/strigov-locallm/internal/desktop"
	"github.com/strigov/strigov-locallm/internal/i18n"
	"github.com/strigov/strigov-locallm/internal/tray"
)

// §11.2: главная горутина закреплена за главным потоком ОС; в нём runTray крутит цикл сообщений трея.
func init() { runtime.LockOSThread() }

// runTray — шаг 10 §4.7: значок и меню в главном потоке. Возвращается после Driver.Quit (выход хоста) или с ошибкой
// создания значка (хост работает дальше без него).
func runTray(h *host) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}
	d := tray.NewFyneDriver(h.log)
	if !h.setTrayQuit(d.Quit) {
		return nil // хост уже выходит
	}
	auto := desktop.DefaultAutostart(exe, h.cfgAbs)
	return d.Run(func() {
		h.log.Info("tray icon created")
		app := h.trayApp(auto)
		ready := make(chan struct{})
		m := tray.NewMenu(d, i18n.Current(), func(c tray.Click) { <-ready; app.handle(c) })
		app.menu = m
		close(ready)
		go m.Follow(h.ctx, tray.Source{Snapshot: h.sched.Snapshot, VRAM: h.probe.UsageMiB, Autostart: auto.Enabled})
	})
}

// trayApp — действия меню от имени хоста. С --no-dialog окон нет, а подтверждение считается полученным.
func (h *host) trayApp(auto autostarter) *trayApp {
	confirm, dialog := h.sys.confirm, h.sys.dialog
	if h.noDialog {
		confirm, dialog = func(string) bool { return true }, func(string) {}
	}
	return &trayApp{
		ctl:       h.ctl,
		snapshot:  h.sched.Snapshot,
		confirm:   confirm,
		dialog:    dialog,
		openPanel: h.openPanel,
		openLog: func() error {
			p, err := filepath.Abs(h.logPath) // logging.dir в конфиге может быть относительным
			if err != nil {
				return err
			}
			return h.sys.openFile(h.log, p)
		},
		openDir:  func() error { return h.sys.openFolder(h.log, filepath.Dir(h.cfgAbs)) },
		auto:     auto,
		saveLang: h.sys.saveLang,
		quit:     func() { h.shutdown("tray") },
		log:      h.log,
	}
}
