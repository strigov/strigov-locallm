//go:build windows

package main

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procMessageBoxW = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")

const (
	mbYesNo         = 0x00000004
	mbIconError     = 0x00000010
	mbIconWarning   = 0x00000030
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
	idYes           = 6
)

// showDialog — модальное окно ошибки: у сборки -H=windowsgui нет консоли (§4.7).
func showDialog(text string) {
	t, err := windows.UTF16PtrFromString(strings.ReplaceAll(text, "\x00", ""))
	if err != nil {
		return
	}
	c, _ := windows.UTF16PtrFromString("locallm-host")
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)),
		mbIconError|mbSetForeground|mbTopmost)
}

// confirmDialog — окно подтверждения из трея (§11.2): MB_YESNO, MB_ICONWARNING, MB_SETFOREGROUND, MB_TOPMOST;
// вызывается из горутины действия. «Да» — true.
func confirmDialog(text string) bool {
	t, err := windows.UTF16PtrFromString(strings.ReplaceAll(text, "\x00", ""))
	if err != nil {
		return false
	}
	c, _ := windows.UTF16PtrFromString("locallm-host")
	r, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)),
		mbYesNo|mbIconWarning|mbSetForeground|mbTopmost)
	return r == idYes
}
