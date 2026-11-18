package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// keyFileKind — вид файла ключей: имя в ошибках, строка-комментарий нового файла, строка журнала при его создании.
type keyFileKind struct {
	name    string
	header  string
	created string
}

var (
	clientKeysKind = keyFileKind{"keys file", "# locallm API keys: one key per line; restart the host after editing", "api keys file created"}
	adminKeysKind  = keyFileKind{"admin keys file",
		"# locallm administrator keys: one key per line; they open the web panel and /api/admin; restart the host after editing",
		"admin keys file created"}
)

// LoadOrCreateKeys читает файл ключей клиентов (§5.4) или создаёт его с одним случайным ключом.
// Значения ключей не попадают ни в ошибки, ни в журнал.
func LoadOrCreateKeys(path string, log *slog.Logger) ([][]byte, error) {
	return loadOrCreateKeys(clientKeysKind, path, log)
}

// LoadOrCreateAdminKeys — файл ключей администратора (§5.10 спецификации v2): формат, создание, права и журнал — как
// у файла ключей клиентов. Ключ, который есть и среди clientKeys, — ошибка (у хоста — код 4): разделение прав держится
// на том, что это разные строки. В ошибке — номер ключа, а не значение.
func LoadOrCreateAdminKeys(path string, clientKeys [][]byte, log *slog.Logger) ([][]byte, error) {
	keys, err := loadOrCreateKeys(adminKeysKind, path, log)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		for _, ck := range clientKeys {
			if subtle.ConstantTimeCompare(k, ck) == 1 {
				return nil, fmt.Errorf("%s %s: key %d is also a client API key; administrator and client keys must differ", adminKeysKind.name, path, i+1)
			}
		}
	}
	return keys, nil
}

func loadOrCreateKeys(kind keyFileKind, path string, log *slog.Logger) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKeyFile(kind, path, log)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", kind.name, path, err)
	}
	return parseKeyFile(kind, path, data)
}

func parseKeyFile(kind keyFileKind, path string, data []byte) ([][]byte, error) {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	var keys [][]byte
	for i, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if !validKey(s) {
			return nil, fmt.Errorf("%s %s: line %d: invalid key (need at least 32 printable ASCII characters without spaces)", kind.name, path, i+1)
		}
		keys = append(keys, []byte(s))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s %s contains no keys", kind.name, path)
	}
	return keys, nil
}

func validKey(s string) bool {
	if len(s) < 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// syncKeyFile позволяет подменить сброс файла для проверки ошибки записи.
var syncKeyFile = func(f *os.File) error { return f.Sync() }

// createKeys — файл ключей клиентов с одним новым ключом (тесты MVP вызывают его напрямую).
func createKeys(path string, log *slog.Logger) ([][]byte, error) {
	return createKeyFile(clientKeysKind, path, log)
}

func createKeyFile(kind keyFileKind, path string, log *slog.Logger) ([][]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%s %s: %w", kind.name, path, err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("%s %s: generate key: %w", kind.name, path, err)
	}
	key := base64.RawURLEncoding.EncodeToString(raw[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", kind.name, path, err)
	}
	// Файл создан этим вызовом (O_EXCL), поэтому при ошибке его можно удалить.
	fail := func(err error) ([][]byte, error) {
		_ = os.Remove(path)
		return nil, fmt.Errorf("%s %s: %w", kind.name, path, err)
	}
	if _, err := f.WriteString(kind.header + "\n" + key + "\n"); err != nil {
		f.Close()
		return fail(err)
	}
	if err := syncKeyFile(f); err != nil {
		f.Close()
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if log != nil {
		log.Info(kind.created, "path", path)
	}
	return [][]byte{[]byte(key)}, nil
}
