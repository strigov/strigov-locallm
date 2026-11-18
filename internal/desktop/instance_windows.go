//go:build windows

package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// InstanceName — §11.5: "locallm-host-" и первые 16 hex SHA-256 от strings.ToLower(filepath.Clean(path)), где path —
// абсолютный путь конфига. Разные конфиги — разные экземпляры.
func InstanceName(configPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(configPath))))
	return "locallm-host-" + hex.EncodeToString(sum[:8])
}

// Instance — именованный мьютекс Local\<name> и события с автосбросом Local\<name>-show и Local\<name>-shown (§11.5).
// Пространство Local\ — своё у каждого сеанса Windows. Хэндлы не наследуются.
type Instance struct {
	mutex, show, shown, stop windows.Handle

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// AcquireInstance — первый экземпляр (first == true) создаёт мьютекс и держит его до Close (или до выхода процесса);
// второй получает ERROR_ALREADY_EXISTS (first == false) и может попросить первого через Ask.
func AcquireInstance(name string) (*Instance, bool, error) {
	i := &Instance{}
	stop, err := windows.CreateEvent(nil, 1, 0, nil) // ручной сброс, без имени: сигнал Close для Serve
	if stop == 0 {
		return nil, false, fmt.Errorf("CreateEvent: %w", err)
	}
	i.stop = stop
	mname, err := windows.UTF16PtrFromString(`Local\` + name)
	if err != nil {
		i.Close()
		return nil, false, err
	}
	m, err := windows.CreateMutex(nil, false, mname) // существующий: хэндл и ERROR_ALREADY_EXISTS
	if m == 0 {
		i.Close()
		return nil, false, fmt.Errorf("CreateMutex: %w", err)
	}
	i.mutex = m
	first := !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
	if i.show, err = namedEvent(`Local\` + name + "-show"); err != nil {
		i.Close()
		return nil, false, err
	}
	if i.shown, err = namedEvent(`Local\` + name + "-shown"); err != nil {
		i.Close()
		return nil, false, err
	}
	return i, first, nil
}

// namedEvent — событие с автосбросом. Уже созданное другим экземпляром открывается: CreateEvent тогда возвращает
// хэндл вместе с ERROR_ALREADY_EXISTS, и это не ошибка.
func namedEvent(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateEvent(nil, 0, 0, p)
	if h == 0 {
		return 0, fmt.Errorf("CreateEvent: %w", err)
	}
	return h, nil
}

// Serve — первый экземпляр (§11.5): ждёт show, вызывает onShow и ставит shown; возвращается после Close.
func (i *Instance) Serve(onShow func()) {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return
	}
	i.wg.Add(1)
	i.mu.Unlock()
	defer i.wg.Done()
	for {
		ev, err := windows.WaitForMultipleObjects([]windows.Handle{i.stop, i.show}, false, windows.INFINITE)
		if err != nil || ev != windows.WAIT_OBJECT_0+1 {
			return
		}
		onShow()
		_ = windows.SetEvent(i.shown)
	}
}

// Ask — второй экземпляр (§11.5): просит первый открыть панель и ждёт ответа не дольше wait. Ответ на прошлую
// просьбу, которого никто не дождался, сбрасывается.
func (i *Instance) Ask(wait time.Duration) bool {
	_ = windows.ResetEvent(i.shown)
	if err := windows.SetEvent(i.show); err != nil {
		return false
	}
	ev, err := windows.WaitForSingleObject(i.shown, uint32(wait.Milliseconds()))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// Close — остановить Serve и закрыть хэндлы; мьютекс освобождается вместе с последним хэндлом. Повторный вызов
// ничего не делает.
func (i *Instance) Close() {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return
	}
	i.closed = true
	i.mu.Unlock()
	if i.stop != 0 {
		_ = windows.SetEvent(i.stop)
	}
	i.wg.Wait()
	for _, h := range []windows.Handle{i.stop, i.shown, i.show, i.mutex} {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}
