package config

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysValidFile(t *testing.T) {
	keys, err := LoadOrCreateKeys(filepath.Join("..", "..", "testdata", "config", "keys_valid.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || len(keys[0]) < 32 || strings.Trim(string(keys[0]), "a") != "" ||
		len(keys[1]) < 32 || strings.Trim(string(keys[1]), "b") != "" {
		t.Fatalf("keys %q", keys)
	}
}

func TestKeysCRLFAndBOM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(p, []byte("\xef\xbb\xbf"+strings.Repeat("k", 40)+"\r\n# c\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadOrCreateKeys(p, nil)
	if err != nil || len(keys) != 1 || len(keys[0]) != 40 {
		t.Fatalf("keys=%q err=%v", keys, err)
	}
}

func TestKeysInvalidLineReportsNumberWithoutValue(t *testing.T) {
	_, err := LoadOrCreateKeys(filepath.Join("..", "..", "testdata", "config", "keys_short.txt"), nil)
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "tooShortKey123") {
		t.Fatalf("err=%v", err)
	}
	p := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 20)+" "+strings.Repeat("y", 20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeys(p, nil); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("key with space must be rejected, err=%v", err)
	}
}

func TestKeysEmptyFileIsError(t *testing.T) {
	_, err := LoadOrCreateKeys(filepath.Join("..", "..", "testdata", "config", "keys_empty.txt"), nil)
	if err == nil || !strings.Contains(err.Error(), "contains no keys") {
		t.Fatalf("err=%v", err)
	}
}

func TestKeysCreatedWhenMissing(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := filepath.Join(t.TempDir(), "nested", "dir", "keys.txt")
	keys, err := LoadOrCreateKeys(p, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || len(keys[0]) != 43 {
		t.Fatalf("generated %q", keys)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(string(keys[0])); err != nil || len(raw) != 32 {
		t.Fatalf("generated key must encode 32 bytes: length=%d err=%v", len(raw), err)
	}
	again, err := LoadOrCreateKeys(p, nil)
	if err != nil || len(again) != 1 || !bytes.Equal(again[0], keys[0]) {
		t.Fatalf("re-read %q %v", again, err)
	}
	if !strings.Contains(logBuf.String(), "keys.txt") || strings.Contains(logBuf.String(), string(keys[0])) {
		t.Fatalf("log must name the file and never the key: %q", logBuf.String())
	}
}

func TestKeysASCIIAndLength(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		valid     bool
	}{
		{"minimum", strings.Repeat("a", 32), true},
		{"short", strings.Repeat("a", 31), false},
		{"ASCII boundaries", strings.Repeat("!~", 16), true},
		{"non-ASCII", strings.Repeat("я", 32), false},
		{"control", strings.Repeat("a", 32) + "\x00", false},
		{"DEL", strings.Repeat("a", 32) + "\x7f", false},
		{"tab inside", strings.Repeat("a", 16) + "\t" + strings.Repeat("b", 16), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "keys.txt")
			data := []byte("  # comment\r\n\r\n  " + tc.key + "  \r\n")
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			keys, err := LoadOrCreateKeys(p, nil)
			if tc.valid {
				if err != nil || len(keys) != 1 || string(keys[0]) != tc.key {
					t.Fatalf("valid key rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), tc.key) {
				t.Fatalf("invalid key must report its line without its value: %v", err)
			}
			after, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatalf("existing key file changed: %v", err)
			}
		})
	}
}

func TestKeysDirectoryIsError(t *testing.T) {
	if _, err := LoadOrCreateKeys(t.TempDir(), nil); err == nil {
		t.Fatal("directory must not be treated as a missing key file")
	}
}

func TestKeysCreateFailureRemovesNewFile(t *testing.T) {
	old := syncKeyFile
	defer func() { syncKeyFile = old }()
	syncKeyFile = func(*os.File) error { return os.ErrInvalid }
	p := filepath.Join(t.TempDir(), "keys.txt")
	keys, err := LoadOrCreateKeys(p, nil)
	if err == nil || keys != nil {
		t.Fatalf("flush failure must be an error: keys=%q err=%v", keys, err)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Fatalf("partially written key file must be removed: %v", statErr)
	}
	syncKeyFile = old
	if keys, err := LoadOrCreateKeys(p, nil); err != nil || len(keys) != 1 {
		t.Fatalf("retry after failure: keys=%d err=%v", len(keys), err)
	}
}

func TestKeysCreateRaceKeepsExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(p, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createKeys(p, nil); err == nil {
		t.Fatal("O_EXCL must fail on an existing file")
	}
	if data, _ := os.ReadFile(p); string(data) != "existing" {
		t.Fatalf("pre-existing file changed: %q", data)
	}
}

// §5.10 спецификации v2: файл ключей администратора создаётся как файл ключей клиентов, со своим комментарием.
func TestAdminKeysCreated(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := filepath.Join(t.TempDir(), "nested", "admin-key.txt")
	client := [][]byte{[]byte(strings.Repeat("c", 43))}
	keys, err := LoadOrCreateAdminKeys(p, client, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || len(keys[0]) != 43 {
		t.Fatalf("generated %d keys", len(keys))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	header := "# locallm administrator keys: one key per line; they open the web panel and /api/admin; restart the host after editing\n"
	if string(data) != header+string(keys[0])+"\n" {
		t.Fatalf("admin keys file %q", data)
	}
	if !strings.Contains(logBuf.String(), "admin keys file created") || !strings.Contains(logBuf.String(), "admin-key.txt") ||
		strings.Contains(logBuf.String(), string(keys[0])) {
		t.Fatalf("log must name the file and never the key: %q", logBuf.String())
	}
	again, err := LoadOrCreateAdminKeys(p, client, nil)
	if err != nil || len(again) != 1 || !bytes.Equal(again[0], keys[0]) {
		t.Fatalf("re-read %d keys, err %v", len(again), err)
	}
}

// §5.10: ключ, который есть и в файле ключей клиентов, — ошибка с номером ключа, без значения.
func TestAdminKeysMustDifferFromClientKeys(t *testing.T) {
	shared := strings.Repeat("s", 40)
	p := filepath.Join(t.TempDir(), "admin-key.txt")
	if err := os.WriteFile(p, []byte("# admin\n"+strings.Repeat("a", 40)+"\n"+shared+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateAdminKeys(p, [][]byte{[]byte(strings.Repeat("c", 40)), []byte(shared)}, nil)
	want := "admin keys file " + p + ": key 2 is also a client API key; administrator and client keys must differ"
	if err == nil || err.Error() != want || strings.Contains(err.Error(), shared) {
		t.Fatalf("err %v", err)
	}
	if keys, err := LoadOrCreateAdminKeys(p, [][]byte{[]byte(strings.Repeat("c", 40))}, nil); err != nil || len(keys) != 2 {
		t.Fatalf("distinct keys: %d keys, err %v", len(keys), err)
	}
}

func TestAdminKeysInvalid(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct{ data, want string }{
		"short": {"short-key\n", "line 1: invalid key"},
		"empty": {"# only a comment\n", "contains no keys"},
	} {
		p := filepath.Join(dir, name+".txt")
		if err := os.WriteFile(p, []byte(tc.data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateAdminKeys(p, nil, nil); err == nil || !strings.HasPrefix(err.Error(), "admin keys file "+p) ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if _, err := LoadOrCreateAdminKeys(dir, nil, nil); err == nil {
		t.Fatal("a directory must be an error")
	}
}

func TestAdminKeysCreateFailureCanRetry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "admin-key.txt")
	oldSync := syncKeyFile
	t.Cleanup(func() { syncKeyFile = oldSync })
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	syncKeyFile = func(*os.File) error { return os.ErrInvalid }
	if keys, err := LoadOrCreateAdminKeys(p, nil, log); err == nil || keys != nil {
		t.Fatalf("keys=%d err=%v", len(keys), err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) || logBuf.Len() != 0 {
		t.Fatalf("partial file must be removed without a creation log: %v", err)
	}
	syncKeyFile = oldSync
	keys, err := LoadOrCreateAdminKeys(p, nil, log)
	if err != nil || len(keys) != 1 {
		t.Fatalf("retry: keys=%d err=%v", len(keys), err)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(string(keys[0])); err != nil || len(raw) != 32 {
		t.Fatalf("generated key must encode 32 bytes: length=%d err=%v", len(raw), err)
	}
}
