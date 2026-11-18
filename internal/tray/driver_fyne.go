//go:build windows

package tray

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procFindWindowW      = user32.NewProc("FindWindowW")
)

// taskbarWait — сколько ждать панель задач перед созданием значка (автозапуск при входе в Windows).
var taskbarWait = 60 * time.Second

// fyneDriver — Driver на fyne.io/systray v1.12.2 (решение S8, §11.2).
type fyneDriver struct {
	log *slog.Logger

	mu         sync.Mutex
	quitting   bool
	registered bool

	initErr atomic.Pointer[string] // первая строка об ошибке создания окна или меню
	icons   [4]string              // файлы значков по цветам в своём временном каталоге (F1-D3)
}

// NewFyneDriver — драйвер трея. logger — журнал хоста: ошибки библиотеки, которые она пишет только в стандартный
// log, уходят туда строкой "tray error" (у сборки -H=windowsgui нет stderr).
func NewFyneDriver(logger *slog.Logger) Driver { return &fyneDriver{log: logger} }

func (d *fyneDriver) isQuitting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quitting
}

// Run — §11.2: вызывается из главной горутины, закреплённой за главным потоком ОС (runtime.LockOSThread в init
// пакета main). Пишет значки в свой временный каталог, ждёт панель задач, создаёт значок (systray.Register — синхронно)
// и тут же ставит первую картинку: Register добавляет значок в трей без изображения, и если ошибку его установки
// проглотить, в трее останется пустая ячейка, а Run о ней не сообщит. Поэтому первая картинка ставится до цикла
// сообщений, её ошибка возвращается из Run, а значок до возврата убран (systray.Quit постит WM_CLOSE, а NIM_DELETE
// выполняет wndProc по WM_DESTROY — очередь прокачиваем здесь). Пользовательский onReady до успеха не вызывается.
// Дальше Run крутит цикл сообщений до Quit. systray.Run не используется: при ошибке создания окна или меню он только
// пишет строку в log и навсегда зависает в цикле, а Register позволяет вернуть ошибку.
func (d *fyneDriver) Run(onReady func()) error {
	if d.isQuitting() {
		return nil
	}
	dir, err := os.MkdirTemp("", "locallm-tray-")
	if err != nil {
		return fmt.Errorf("tray icons: %w", err)
	}
	defer d.removeIcons(dir)
	for c := Gray; c <= Blue; c++ {
		p := filepath.Join(dir, fmt.Sprintf("%d.ico", c))
		if err := os.WriteFile(p, ICO(c), 0o600); err != nil {
			return fmt.Errorf("tray icons: %w", err)
		}
		d.icons[c] = p
	}
	d.waitTaskbar()
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetFlags(0)
	log.SetOutput(logBridge{d})
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	d.mu.Lock()
	if d.quitting {
		d.mu.Unlock()
		return nil
	}
	iconReady := make(chan error, 1) // результат первой установки значка: обёртка onReady ждёт его
	systray.Register(d.gatedOnReady(iconReady, onReady), nil)
	if p := d.initErr.Load(); p != nil {
		iconReady <- errors.New(*p) // обёртка завершится без onReady
		d.mu.Unlock()
		return errors.New(*p)
	}
	if err := systray.SetIconFromFilePath(d.icons[Gray]); err != nil {
		iconReady <- err // onReady не вызывается
		systray.Quit()
		d.mu.Unlock()
		_ = messageLoop() // WM_CLOSE → WM_DESTROY → NIM_DELETE → WM_QUIT: значок удалён до возврата ошибки
		return fmt.Errorf("tray icon: %w", err)
	}
	iconReady <- nil
	d.registered = true
	d.mu.Unlock()
	return messageLoop()
}

// gatedOnReady — обёртка onReady для systray.Register (F2-B1): пользовательский onReady не стартует раньше первого
// значка — Run шлёт её результат в iconReady; при ошибке значка или при Quit onReady не вызывается.
func (d *fyneDriver) gatedOnReady(iconReady <-chan error, onReady func()) func() {
	return func() {
		if err := <-iconReady; err == nil && !d.isQuitting() {
			onReady()
		}
	}
}

// Quit — убрать значок (NIM_DELETE сразу) и завершить цикл сообщений (WM_CLOSE → WM_DESTROY → WM_QUIT). До Register
// systray.Quit не вызывается: у библиотеки ещё нет окна и обработчика выхода.
func (d *fyneDriver) Quit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quitting {
		return
	}
	d.quitting = true
	if d.registered {
		systray.Quit()
	}
}

func (d *fyneDriver) SetIcon(c Color) {
	if err := systray.SetIconFromFilePath(d.icons[c]); err != nil {
		d.log.Warn("tray error", "err", err)
	}
}

func (d *fyneDriver) SetTooltip(text string) { systray.SetTooltip(text) }

func (d *fyneDriver) AddItem(title string) Item { return fyneItem{systray.AddMenuItem(title, "")} }

func (d *fyneDriver) AddCheckbox(title string, checked bool) Item {
	return fyneItem{systray.AddMenuItemCheckbox(title, "", checked)}
}

func (d *fyneDriver) AddSeparator() { systray.AddSeparator() }

// removeIcons удаляет только файлы значков, созданные Run, и свой временный каталог (os.Remove, не RemoveAll).
func (d *fyneDriver) removeIcons(dir string) {
	for _, p := range d.icons {
		if p != "" {
			_ = os.Remove(p)
		}
	}
	_ = os.Remove(dir)
}

// waitTaskbar — при входе в Windows хост может стартовать раньше панели задач; тогда NIM_ADD не проходит, и
// fyne.io/systray повторно не пробует. Ждём окно Shell_TrayWnd не дольше taskbarWait (или до Quit).
func (d *fyneDriver) waitTaskbar() {
	name, _ := windows.UTF16PtrFromString("Shell_TrayWnd")
	for deadline := time.Now().Add(taskbarWait); time.Now().Before(deadline) && !d.isQuitting(); time.Sleep(500 * time.Millisecond) {
		if h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(name)), 0); h != 0 {
			return
		}
	}
}

// msg — MSG Win32 (x64).
type msg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// messageLoop — цикл сообщений потока трея; systray.Register оставляет его вызывающему. WM_QUIT — выход.
func messageLoop() error {
	var m msg
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			return fmt.Errorf("GetMessageW: %w", err)
		case 0:
			return nil
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// logBridge — стандартный log на время Run: fyne.io/systray пишет ошибки только туда (F1-D3). Каждая строка — warn
// "tray error"; первая ошибка создания окна или меню запоминается для Run.
type logBridge struct{ d *fyneDriver }

func (b logBridge) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	text := strings.TrimPrefix(line, "systray error: ")
	if isInitFailure(line) {
		b.d.initErr.CompareAndSwap(nil, &text)
	}
	b.d.log.Warn("tray error", "err", text)
	return len(p), nil
}

// isInitFailure — строка registerSystray: значок или меню не созданы, onReady не будет вызван.
func isInitFailure(line string) bool {
	return strings.Contains(line, "unable to init instance") || strings.Contains(line, "unable to create menu")
}

// fyneItem — Item поверх *systray.MenuItem.
type fyneItem struct{ mi *systray.MenuItem }

func (i fyneItem) SetTitle(title string)        { i.mi.SetTitle(title) }
func (i fyneItem) Enable()                      { i.mi.Enable() }
func (i fyneItem) Disable()                     { i.mi.Disable() }
func (i fyneItem) Check()                       { i.mi.Check() }
func (i fyneItem) Uncheck()                     { i.mi.Uncheck() }
func (i fyneItem) Show()                        { i.mi.Show() }
func (i fyneItem) Hide()                        { i.mi.Hide() }
func (i fyneItem) Clicked() <-chan struct{}     { return i.mi.ClickedCh }
func (i fyneItem) AddSubItem(title string) Item { return fyneItem{i.mi.AddSubMenuItem(title, "")} }

func (i fyneItem) AddSubCheckbox(title string, checked bool) Item {
	return fyneItem{i.mi.AddSubMenuItemCheckbox(title, "", checked)}
}
