package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// dirNames — имена в каталоге dir по возрастанию.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func readText(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestETag(t *testing.T) {
	for _, tc := range []struct {
		data   []byte
		exists bool
		want   string
	}{
		{[]byte("abc"), true, `"sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"`},
		{nil, true, `"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`},
		{nil, false, `"absent"`},
	} {
		if got := ETag(tc.data, tc.exists); got != tc.want {
			t.Errorf("ETag(%q, %v) = %s, want %s", tc.data, tc.exists, got, tc.want)
		}
	}
}

func TestWriteFileAtomicNewFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	etag, backup, err := WriteFileAtomic(p, []byte("one"), `"absent"`)
	if err != nil || etag != ETag([]byte("one"), true) || backup != "" {
		t.Fatalf("etag %s backup %q err %v", etag, backup, err)
	}
	if got := readText(t, p); got != "one" {
		t.Fatalf("content %q", got)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"models.yaml"}) {
		t.Fatalf("files %q", names)
	}
}

// §5.7 шаг 1: ETag не совпал — *ChangedError до любых записей; совпал — прежние байты уходят в копию.
func TestWriteFileAtomicETagMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	one := ETag([]byte("one"), true)
	for _, ifMatch := range []string{`"absent"`, ETag([]byte("other"), true), ""} {
		_, _, err := WriteFileAtomic(p, []byte("two"), ifMatch)
		var ce *ChangedError
		if !errors.As(err, &ce) || ce.Path != p || ce.ETag != one {
			t.Fatalf("If-Match %s: err %v", ifMatch, err)
		}
	}
	if got := readText(t, p); got != "one" {
		t.Fatalf("content %q", got)
	}
	if names := dirNames(t, dir); !slices.Equal(names, []string{"models.yaml"}) {
		t.Fatalf("files %q", names)
	}
	q := filepath.Join(dir, "new.yaml")
	if _, _, err := WriteFileAtomic(q, []byte("x"), one); !errors.As(err, new(*ChangedError)) {
		t.Fatalf("missing file with an expected ETag: %v", err)
	}
	if _, err := os.Stat(q); !os.IsNotExist(err) {
		t.Fatalf("file must not be created: %v", err)
	}
	etag, backup, err := WriteFileAtomic(p, []byte("two"), one)
	if err != nil || etag != ETag([]byte("two"), true) {
		t.Fatalf("etag %s err %v", etag, err)
	}
	if readText(t, p) != "two" || readText(t, backup) != "one" || filepath.Dir(backup) != dir ||
		!regexp.MustCompile(`^models\.yaml\.\d{8}T\d{6}Z\.bak$`).MatchString(filepath.Base(backup)) {
		t.Fatalf("backup %q", backup)
	}
}

// §5.7 шаг 3: остаются 10 самых новых копий этого файла; чужие файлы и каталоги с похожими именами не трогаются.
func TestWriteFileAtomicBackupsKeepTen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	strays := []string{"models.yaml.bak", "other.yaml.20200101T000000Z.bak", "models.yaml.20200101T000000Z.bak.txt", "models.yaml.2020-01-01.bak"}
	for _, s := range strays {
		if err := os.WriteFile(filepath.Join(dir, s), []byte("stray"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "models.yaml.20190101T000000Z.bak", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	calls := 0
	writeNow = func() time.Time { at := base.Add(time.Duration(calls/2) * time.Second); calls++; return at }
	t.Cleanup(func() { writeNow = time.Now })
	etag := ETag(nil, false)
	for k := 1; k <= 13; k++ {
		var err error
		if etag, _, err = WriteFileAtomic(p, []byte(fmt.Sprintf("v%d", k)), etag); err != nil {
			t.Fatal(err)
		}
	}
	want := append([]string{"models.yaml", "models.yaml.20190101T000000Z.bak"}, strays...)
	for s := 1; s <= 5; s++ {
		stamp := fmt.Sprintf("20261109T10000%dZ", s)
		want = append(want, "models.yaml."+stamp+".bak", "models.yaml."+stamp+"-1.bak")
	}
	slices.Sort(want)
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := readText(t, filepath.Join(dir, "models.yaml.20261109T100005Z-1.bak")); got != "v12" {
		t.Fatalf("newest copy %q, want v12", got)
	}
	for _, s := range strays {
		if got := readText(t, filepath.Join(dir, s)); got != "stray" {
			t.Fatalf("%s changed: %q", s, got)
		}
	}
}

// §5.7 шаг 5: сбой — исходный файл не тронут, временного файла нет.
func TestWriteFileAtomicFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	one := ETag([]byte("one"), true)
	oldSync, oldMove := syncWrite, moveFileEx
	t.Cleanup(func() { syncWrite, moveFileEx = oldSync, oldMove })

	syncWrite = func(*os.File) error { return errors.New("sync failed") }
	_, _, err := WriteFileAtomic(p, []byte("two"), one)
	syncWrite = oldSync
	if err == nil || readText(t, p) != "one" || !slices.Equal(dirNames(t, dir), []string{"models.yaml"}) {
		t.Fatalf("sync failure: err %v, files %q", err, dirNames(t, dir))
	}

	moveFileEx = func(string, string) error { return errors.New("move failed") }
	_, _, err = WriteFileAtomic(p, []byte("two"), one)
	moveFileEx = oldMove
	if err == nil || readText(t, p) != "one" {
		t.Fatalf("move failure: err %v", err)
	}
	// Копия этого вызова удалена: исходный файл не изменился (F5-D3 ledger первого плана).
	if names := dirNames(t, dir); !slices.Equal(names, []string{"models.yaml"}) {
		t.Fatalf("files after a failed move %q", names)
	}
}

// Временный файл, оставшийся от сбоя, не мешает: O_EXCL и другое имя; чужой временный файл не удаляется.
func TestWriteFileAtomicLeftoverTempDoesNotBlock(t *testing.T) {
	oldSuffix := tmpSuffix
	t.Cleanup(func() { tmpSuffix = oldSuffix })
	if s := oldSuffix(); len(s) != 8 || strings.Trim(s, "0123456789abcdef") != "" {
		t.Fatalf("temp suffix %q: want 8 hex digits", s)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	leftover := p + ".tmp-deadbeef"
	if err := os.WriteFile(leftover, []byte("old temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	suffixes := []string{"deadbeef", "cafebabe"}
	tmpSuffix = func() string { s := suffixes[0]; suffixes = suffixes[1:]; return s }
	if _, _, err := WriteFileAtomic(p, []byte("x"), `"absent"`); err != nil {
		t.Fatal(err)
	}
	if readText(t, p) != "x" || readText(t, leftover) != "old temp" ||
		!slices.Equal(dirNames(t, dir), []string{"state.json", "state.json.tmp-deadbeef"}) {
		t.Fatalf("files %q", dirNames(t, dir))
	}
}

// Ошибка сброса копии не меняет исходник, не оставляет неполную копию и не удаляет старые копии.
func TestWriteFileAtomicBackupFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := p + ".20200101T000000Z.bak"
	if err := os.WriteFile(prior, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldSync := syncWrite
	t.Cleanup(func() { syncWrite = oldSync })
	calls := 0
	syncWrite = func(f *os.File) error {
		calls++
		if calls == 2 {
			return errors.New("backup sync failed")
		}
		return oldSync(f)
	}
	_, _, err := WriteFileAtomic(p, []byte("replacement"), ETag([]byte("original"), true))
	if err == nil || !strings.Contains(err.Error(), "backup sync failed") {
		t.Fatalf("err %v", err)
	}
	if readText(t, p) != "original" || readText(t, prior) != "prior" ||
		!slices.Equal(dirNames(t, dir), []string{"models.yaml", filepath.Base(prior)}) {
		t.Fatalf("files after failure: %q", dirNames(t, dir))
	}
}
