package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// HostState — содержимое state.json (§5.10 спецификации v2): пауза переживает перезапуск хоста.
type HostState struct {
	Paused    bool
	ChangedAt time.Time
	By        string // "tray" или "admin_api"
}

// maxStateBytes — предел размера state.json; больше — файл испорчен.
const maxStateBytes = 64 << 10

type stateFile struct {
	Version   *int    `json:"version"`
	Paused    *bool   `json:"paused"`
	ChangedAt *string `json:"changed_at"`
	By        *string `json:"by"`
}

// ReadState читает state.json строго (§5.10): неизвестное поле, version ≠ 1, нет paused, время не RFC 3339, данные
// после объекта, больше 64 КиБ — ошибка (файл испорчен; хост тогда стартует на паузе). Нет файла — exists == false
// без ошибки.
func ReadState(path string) (st HostState, exists bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return HostState{}, false, nil
	}
	if err != nil {
		return HostState{}, false, fmt.Errorf("state file %s: %w", path, err)
	}
	defer f.Close()
	fail := func(format string, a ...any) (HostState, bool, error) {
		return HostState{}, true, fmt.Errorf("state file %s: %s", path, fmt.Sprintf(format, a...))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return fail("%v", err)
	}
	if len(data) > maxStateBytes {
		return fail("larger than %d bytes", maxStateBytes)
	}
	var sf stateFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sf); err != nil {
		return fail("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fail("unexpected data after the JSON object")
	}
	if sf.Version == nil || *sf.Version != 1 {
		return fail("version must be 1")
	}
	if sf.Paused == nil {
		return fail("paused is required")
	}
	st.Paused = *sf.Paused
	if sf.ChangedAt != nil {
		if st.ChangedAt, err = time.Parse(time.RFC3339, *sf.ChangedAt); err != nil {
			return fail("changed_at is not an RFC 3339 time")
		}
	}
	if sf.By != nil {
		st.By = *sf.By
	}
	return st, true, nil
}

// WriteState записывает state.json атомарно (§5.7 без If-Match и без .bak): {"version":1,"paused":…,"changed_at":…,"by":…}
// и перевод строки; время — UTC, RFC 3339 с секундами. Каталог создаётся при необходимости.
func WriteState(path string, st HostState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	version, at := 1, st.ChangedAt.UTC().Format(time.RFC3339)
	data, err := json.Marshal(stateFile{Version: &version, Paused: &st.Paused, ChangedAt: &at, By: &st.By})
	if err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	if _, _, err := writeAtomic(path, append(data, '\n'), false, "", false); err != nil {
		return fmt.Errorf("state file %s: %w", path, err)
	}
	return nil
}
