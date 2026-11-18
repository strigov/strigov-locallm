//go:build windows

package config

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// Флаги GetFinalPathNameByHandle; в golang.org/x/sys/windows v0.49.0 их нет.
const (
	fileNameNormalized = 0x0 // FILE_NAME_NORMALIZED
	volumeNameDOS      = 0x0 // VOLUME_NAME_DOS
)

// Canonical — окончательный путь файла или каталога (§5.6 спецификации v2, PV1): CreateFile с FILE_READ_ATTRIBUTES,
// общим доступом на чтение, запись и удаление и FILE_FLAG_BACKUP_SEMANTICS (открывает и каталоги), затем
// GetFinalPathNameByHandle(FILE_NAME_NORMALIZED, VOLUME_NAME_DOS). Ссылки, junction, "..", имена 8.3 и регистр раскрыты.
// Ошибка не содержит пути: он может прийти из значения аргумента (§5.5 п. 14).
func Canonical(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", fmt.Errorf("GetFinalPathNameByHandle: %w", err)
		}
		if int(n) < len(buf) {
			return stripFinalPathPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1) // n — нужный размер с завершающим нулём
	}
}

// stripFinalPathPrefix снимает префикс \\?\, а \\?\UNC\ заменяет на \\ (§5.6, PV1).
func stripFinalPathPrefix(s string) string {
	if rest, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(s, `\\?\`)
}
