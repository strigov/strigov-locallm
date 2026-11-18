//go:build windows

package gguf

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// openShared открывает файл на чтение с общим доступом на чтение, запись и удаление (§8.1): работающему
// llama-server чтение не мешает. os.Open не даёт FILE_SHARE_DELETE. Ошибка не содержит пути.
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return os.NewFile(uintptr(h), "gguf"), nil
}

// reparse — точка повторной обработки: символическая ссылка, junction, заглушка облачного хранилища (§8.2). Поиск её
// не обходит и в выдачу не берёт. Признак — FILE_ATTRIBUTE_REPARSE_POINT; без атрибутов — биты режима Go.
func reparse(fi fs.FileInfo) bool {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// checkDiskFile отклоняет каналы и устройства, сохраняя поддержку облачных файлов с ModeIrregular.
func checkDiskFile(f *os.File) error {
	t, err := windows.GetFileType(windows.Handle(f.Fd()))
	if err != nil {
		return fmt.Errorf("file type: %w", err)
	}
	if t != windows.FILE_TYPE_DISK {
		return errors.New("not a regular disk file")
	}
	return nil
}
