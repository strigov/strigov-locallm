// Package ui — файлы веб-панели (§10.1), встроенные в exe.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// Files — файлы каталога static; корень — сам каталог (index.html, app.js, …).
func Files() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // имя каталога — константа: ошибка возможна только при сломанной сборке
	}
	return sub
}
