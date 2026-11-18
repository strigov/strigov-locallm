//go:build windows

package i18n

import "golang.org/x/sys/windows"

var procGetUserDefaultUILanguage = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")

// Detect — язык по языку интерфейса Windows текущего пользователя: русский, китайский, иначе английский.
func Detect() Lang {
	r, _, _ := procGetUserDefaultUILanguage.Call()
	return fromLangID(uint16(r))
}
