//go:build windows

// Package desktop — интеграция с Windows (§11.3–11.5 спецификации v2): браузер, Блокнот и Проводник вне Job Object
// хоста, автозапуск, один экземпляр.
package desktop

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/sys/windows"
)

// startProcess — запуск exe по полному пути с флагами создания, без ожидания процесса (Process.Release); тесты
// подменяют. Ошибка exec содержит путь exe, но не аргументы.
var startProcess = func(exe string, args []string, flags uint32) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

// Системные каталоги из Windows API, а не из %SystemRoot%: окружение их не подменит. Тесты подменяют.
var (
	systemDir  = windows.GetSystemDirectory        // C:\Windows\System32
	windowsDir = windows.GetSystemWindowsDirectory // C:\Windows
)

// OpenURL — браузер по умолчанию (§11.3): rundll32.exe url.dll,FileProtocolHandler <url>. Принимается только адрес
// http://, который собрал хост. В адресе панели — одноразовый код, поэтому его нет ни в тексте ошибки, ни в журнале.
func OpenURL(log *slog.Logger, rawURL string) error {
	if !safeURL(rawURL) {
		return errors.New("refusing to open the address: only http:// addresses without spaces or quotes are allowed")
	}
	dir, err := systemDir()
	if err != nil {
		return fmt.Errorf("system directory: %w", err)
	}
	_, _, err = launch(log, "browser", filepath.Join(dir, "rundll32.exe"), "url.dll,FileProtocolHandler", rawURL)
	return err
}

// OpenFile — файл в Блокноте (§11.3).
func OpenFile(log *slog.Logger, path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("refusing to open a file: the path is not absolute")
	}
	dir, err := systemDir()
	if err != nil {
		return fmt.Errorf("system directory: %w", err)
	}
	_, _, err = launch(log, "notepad", filepath.Join(dir, "notepad.exe"), path)
	return err
}

// OpenFolder — каталог в Проводнике (§11.3).
func OpenFolder(log *slog.Logger, dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("refusing to open a folder: the path is not absolute")
	}
	win, err := windowsDir()
	if err != nil {
		return fmt.Errorf("windows directory: %w", err)
	}
	_, _, err = launch(log, "explorer", filepath.Join(win, "explorer.exe"), dir)
	return err
}

// launch — запуск вне job хоста (CREATE_BREAKAWAY_FROM_JOB, §7.1), чтобы программа не закрылась вместе с хостом.
// ERROR_ACCESS_DENIED — внешний job (Windows Terminal, IDE) не разрешает выход: повтор без флага и предупреждение
// в журнале. Ошибка и журнал содержат только what и путь exe, без аргументов.
func launch(log *slog.Logger, what, exe string, args ...string) (pid int, breakaway bool, err error) {
	pid, err = startProcess(exe, args, windows.CREATE_BREAKAWAY_FROM_JOB)
	breakaway = err == nil
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		log.Warn("launch outside the job was refused; starting inside it", "what", what, "breakaway", false)
		pid, err = startProcess(exe, args, 0)
	}
	if err != nil {
		log.Warn("launch failed", "what", what, "err", err)
		return 0, false, fmt.Errorf("start %s: %w", what, err)
	}
	log.Debug("launched", "what", what, "breakaway", breakaway)
	return pid, breakaway, nil
}

// safeURL — §11.3: схема http, есть хост, нет пробелов, кавычек и управляющих символов.
func safeURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' })
}
