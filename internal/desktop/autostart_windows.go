//go:build windows

package desktop

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Autostart — §11.4: значение Name (REG_SZ) в HKCU\<RunKey> и отметка Диспетчера задач (REG_BINARY) в
// HKCU\<ApprovedKey>. Пути разделов — параметры: тесты работают с HKCU\Software\locallm-test\…
type Autostart struct {
	RunKey      string
	ApprovedKey string
	Name        string
	Exe         string // абсолютный путь exe
	Config      string // абсолютный путь locallm.yaml
}

// DefaultAutostart — настоящий автозапуск текущего пользователя.
func DefaultAutostart(exe, config string) Autostart {
	return Autostart{
		RunKey:      `Software\Microsoft\Windows\CurrentVersion\Run`,
		ApprovedKey: `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`,
		Name:        "locallm-host",
		Exe:         exe,
		Config:      config,
	}
}

// Command — строка значения Run: "<Exe>" --config "<Config>".
func (a Autostart) Command() string { return `"` + a.Exe + `" --config "` + a.Config + `"` }

// Enabled — флажок (§11.4): значение Run есть, путь exe в нём (первая строка в кавычках) совпадает с Exe без учёта
// регистра, а отметки StartupApproved нет или она «включено» (младший бит первого байта 0).
func (a Autostart) Enabled() (bool, error) {
	run, err := readString(a.RunKey, a.Name)
	if err != nil || run == "" {
		return false, err
	}
	exe, ok := quotedExe(run)
	if !ok || !strings.EqualFold(exe, a.Exe) {
		return false, nil
	}
	approved, err := readBinary(a.ApprovedKey, a.Name)
	if err != nil {
		return false, err
	}
	return len(approved) == 0 || approved[0]&1 == 0, nil
}

// Enable — записать значение Run и удалить нашу отметку StartupApproved (§11.4). Другие значения не трогаются.
func (a Autostart) Enable() error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, a.RunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = k.SetStringValue(a.Name, a.Command())
	_ = k.Close()
	if err != nil {
		return err
	}
	return deleteValue(a.ApprovedKey, a.Name)
}

// Disable — удалить значение Run и нашу отметку StartupApproved; отсутствующее значение — не ошибка. Значение Run
// удаляется первым: если его удалить не удалось, StartupApproved не трогаем — иначе отметка снялась бы, а команда
// осталась бы в Run, и «выключить» на деле включило бы автозапуск снова. Разделы и другие значения не трогаются.
func (a Autostart) Disable() error {
	if err := deleteValue(a.RunKey, a.Name); err != nil {
		return err
	}
	return deleteValue(a.ApprovedKey, a.Name)
}

// quotedExe — первая строка в кавычках.
func quotedExe(s string) (string, bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", false
	}
	end := strings.IndexByte(s[1:], '"')
	if end < 0 {
		return "", false
	}
	return s[1 : 1+end], true
}

// readString — строковое значение; раздела или значения нет — "".
func readString(key, name string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return v, err
}

// readBinary — двоичное значение; раздела или значения нет — nil.
func readBinary(key, name string) ([]byte, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer k.Close()
	v, _, err := k.GetBinaryValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	return v, err
}

// deleteValue — удалить одно значение; раздела или значения нет — не ошибка.
func deleteValue(key, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
