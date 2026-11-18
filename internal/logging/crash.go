package logging

import (
	"os"
	"path/filepath"
)

// OpenCrashFile открывает <dir>\crash.log на дозапись — туда runtime пишет панику или фатальную
// ошибку со стеком (debug.SetCrashOutput). Рост ограничен: если файл уже больше maxBytes,
// он становится crash.1.log (прежний crash.1.log удаляется), так что файлов не больше двух.
func OpenCrashFile(dir string, maxBytes int64) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "crash.log")
	if st, err := os.Stat(p); err == nil && st.Size() > maxBytes {
		_ = os.Rename(p, filepath.Join(dir, "crash.1.log")) // не вышло — пишем дальше в тот же файл
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
