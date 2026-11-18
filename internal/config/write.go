package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// maxBackups — сколько копий .bak одного файла остаётся (§5.7 шаг 3 спецификации v2).
const maxBackups = 10

// ETag — "sha256:<64 hex>" от байтов файла, отсутствующий файл — "absent" (§5.7); с кавычками, как в заголовке HTTP.
func ETag(data []byte, exists bool) string {
	if !exists {
		return `"absent"`
	}
	sum := sha256.Sum256(data)
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// ChangedError — файл изменился после чтения: его ETag не равен If-Match (§5.7 шаг 1, ответ 412).
type ChangedError struct {
	Path string
	ETag string // ETag файла на диске
}

func (e *ChangedError) Error() string {
	return fmt.Sprintf("%s has changed on disk (etag %s)", e.Path, e.ETag)
}

// Точки подмены для тестов.
var (
	writeNow   = time.Now
	syncWrite  = func(f *os.File) error { return f.Sync() }
	moveFileEx = func(from, to string) error {
		f, err := windows.UTF16PtrFromString(from)
		if err != nil {
			return err
		}
		t, err := windows.UTF16PtrFromString(to)
		if err != nil {
			return err
		}
		return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	}
	tmpSuffix = func() string {
		var b [4]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
)

// WriteFileAtomic — атомарная запись §5.7: ETag текущего файла сверяется с ifMatch (иначе *ChangedError, ничего не
// записано); байты пишутся во временный <имя>.tmp-<8 hex> в том же каталоге (O_EXCL, Sync); прежние байты — в копию
// <имя>.<ГГГГММДДTччммссZ>.bak (UTC; при совпадении имени -1, -2, ...), затем MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH);
// после замены остаются 10 самых новых копий этого файла. При ошибке временный файл удаляется, исходный не тронут.
// Возвращает ETag записанных байтов и путь копии ("" — файла не было).
func WriteFileAtomic(path string, data []byte, ifMatch string) (etag, backup string, err error) {
	return writeAtomic(path, data, true, ifMatch, true)
}

// writeAtomic — WriteFileAtomic с выбором шагов: checkETag — сверка с ifMatch, keepBackup — копия .bak и чистка копий
// (state.json пишется без обоих, §5.10).
func writeAtomic(path string, data []byte, checkETag bool, ifMatch string, keepBackup bool) (etag, backup string, err error) {
	var old []byte
	exists := false
	if checkETag || keepBackup {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			old, exists = b, true
		case !errors.Is(err, fs.ErrNotExist):
			return "", "", err
		}
	}
	if checkETag {
		if cur := ETag(old, exists); cur != ifMatch {
			return "", "", &ChangedError{Path: path, ETag: cur}
		}
	}
	tmp, err := createTemp(path)
	if err != nil {
		return "", "", err
	}
	name := tmp.Name()
	moved := false
	defer func() {
		if !moved {
			_ = os.Remove(name)
		}
	}()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = syncWrite(tmp)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", "", werr
	}
	if exists && keepBackup {
		if backup, err = writeBackup(path, old); err != nil {
			return "", "", err
		}
	}
	if err := moveFileEx(name, path); err != nil {
		if backup != "" {
			_ = os.Remove(backup) // копия этого вызова (O_EXCL): исходный файл не изменился (F5-D3)
		}
		return "", "", fmt.Errorf("replace %s: %w", path, err)
	}
	moved = true
	if backup != "" {
		pruneBackups(path, backup)
	}
	return ETag(data, true), backup, nil
}

// createTemp — <имя>.tmp-<8 hex> рядом с path (O_EXCL); имя занято (остаток сбоя) — другое имя, до 16 попыток.
func createTemp(path string) (*os.File, error) {
	var err error
	for range 16 {
		var f *os.File
		f, err = os.OpenFile(path+".tmp-"+tmpSuffix(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, err
}

// writeBackup — копия прежних байтов <имя>.<ГГГГММДДTччммссZ>.bak (O_EXCL; имя занято — суффикс -1, -2, ...), Sync.
func writeBackup(path string, old []byte) (string, error) {
	stamp := writeNow().UTC().Format("20060102T150405Z")
	for n := 0; n < 1000; n++ {
		name := path + "." + stamp + ".bak"
		if n > 0 {
			name = fmt.Sprintf("%s.%s-%d.bak", path, stamp, n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(old)
		if err == nil {
			err = syncWrite(f)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(name) // файл создан этим вызовом (O_EXCL)
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("backup of %s: too many copies with time %s", path, stamp)
}

// pruneBackups оставляет копию keep (только что созданную этим вызовом) и maxBackups-1 самых новых других копий файла
// path (по времени в имени, затем по номеру), остальные удаляет (F5-D4 ledger первого плана: копия этого вызова не
// удаляется, даже если её метка времени старше чужих). Трогает только обычные файлы с именем копии именно этого
// файла; ошибки удаления запись не отменяют.
func pruneBackups(path, keep string) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	keepName := filepath.Base(keep)
	re := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(base) + `\.(\d{8}T\d{6}Z)(?:-(\d+))?\.bak$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type copyFile struct {
		name, stamp string
		n           int
	}
	var copies []copyFile
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() || strings.EqualFold(e.Name(), keepName) {
			continue
		}
		n, _ := strconv.Atoi(m[2]) // нет суффикса — 0
		copies = append(copies, copyFile{e.Name(), strings.ToUpper(m[1]), n})
	}
	slices.SortFunc(copies, func(a, b copyFile) int { // новые впереди
		if c := strings.Compare(b.stamp, a.stamp); c != 0 {
			return c
		}
		return b.n - a.n
	})
	for _, c := range copies[min(len(copies), maxBackups-1):] {
		_ = os.Remove(filepath.Join(dir, c.name))
	}
}
