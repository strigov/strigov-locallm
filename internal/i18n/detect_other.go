//go:build !windows

package i18n

// Detect — вне Windows язык интерфейса не определяется: английский.
func Detect() Lang { return EN }
