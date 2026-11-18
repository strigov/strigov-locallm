package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// legacyYAML — locallm.yaml MVP с комментариями; models стоит между глобальными ключами.
const legacyYAML = `# Конфиг locallm-host
server:
  listen: 127.0.0.1:8080 # порт для клиентов
llama:
  server_exe: '%LOCALLM_TEST_DIR%\llama-server.exe'
  default_args: ['-c', '4096']
# Модели
models:
  # основная модель
  qwen:
    path: '%LOCALLM_TEST_DIR%\model.gguf'
    args: ['-c', '32768']
    profiles:
      long: ['-c', '65536']
  tiny:
    path: '%LOCALLM_TEST_DIR%\MODEL2.GGUF'
swap:
  default_model: qwen-long
`

// anchorYAML — якорь в глобальном ключе, ссылка на него — в models: после разделения ссылка повиснет.
const anchorYAML = `llama:
  server_exe: '%LOCALLM_TEST_DIR%\llama-server.exe'
  default_args: &common ['-c', '4096']
models:
  tiny:
    path: '%LOCALLM_TEST_DIR%\model.gguf'
    args: *common
`

// loadLegacy пишет locallm.yaml во временный каталог и загружает его (режим legacy).
func loadLegacy(t *testing.T, text string) (string, *Config) {
	t.Helper()
	testEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "locallm.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsMode != ModeLegacy {
		t.Fatalf("mode %q", c.ModelsMode)
	}
	c.Version = "0.2.0-test"
	return dir, c
}

// §5.8 спецификации v2: models уходит в models.yaml вместе с комментариями, каталог тот же, оригинал — в .bak.
func TestMigrateMovesModelsWithComments(t *testing.T) {
	dir, c := loadLegacy(t, legacyYAML)
	n, res, err := Migrate(c)
	if err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(dir, "models.yaml")
	if res.ModelsFile != modelsPath || n.ModelsMode != ModeFile || n.Version != c.Version || !reflect.DeepEqual(n.Models, c.Models) {
		t.Fatalf("result %+v, mode %q, version %q, models %+v", res, n.ModelsMode, n.Version, n.Models)
	}
	models := readText(t, modelsPath)
	for _, want := range []string{modelsHeader + "version: 1\n# Модели\nmodels:\n  # основная модель\n  qwen:\n", "      long: ['-c', '65536']\n"} {
		if !strings.Contains(models, want) {
			t.Errorf("models.yaml lacks %q:\n%s", want, models)
		}
	}
	main := readText(t, c.Path)
	if strings.Contains(main, "models:") || !strings.HasPrefix(main, "# Конфиг locallm-host\n") ||
		!strings.Contains(main, "listen: 127.0.0.1:8080 # порт для клиентов") || !strings.Contains(main, "default_model: qwen-long") {
		t.Fatalf("new locallm.yaml:\n%s", main)
	}
	if filepath.Dir(res.Backup) != dir || readText(t, res.Backup) != legacyYAML {
		t.Fatalf("backup %q", res.Backup)
	}
	if res.ModelsETag != ETag([]byte(models), true) || n.ModelsETag != res.ModelsETag {
		t.Fatalf("etag %s / %s", res.ModelsETag, n.ModelsETag)
	}
	again, err := Load(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ModelsMode != ModeFile || !reflect.DeepEqual(again.Models, c.Models) || again.ModelsETag != res.ModelsETag || len(again.Warnings) != 0 {
		t.Fatalf("reloaded: mode %q warnings %q", again.ModelsMode, again.Warnings)
	}
	if _, _, err := Migrate(n); !errors.Is(err, ErrAlreadyMigrated) {
		t.Fatalf("second migration: %v", err)
	}
}

func TestMigrateRequiresLegacy(t *testing.T) {
	testEnv(t)
	c, err := loadFileMode(t, profilesYAML, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Migrate(c); !errors.Is(err, ErrAlreadyMigrated) || err.Error() != "models are already in models_file" {
		t.Fatalf("err %v", err)
	}
}

// §5.8 шаг 4: проверка до записи; не прошла — ErrMigrationCheck и ни одного нового файла.
func TestMigrateCheckFailsWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, text, edited, want string // edited — locallm.yaml на диске к моменту переноса ("" — как при загрузке)
	}{
		{"anchor used across files", anchorYAML, "", "the new files do not load"},
		{"globals edited after start", legacyYAML, legacyYAML + "logging:\n  level: debug\n", "global settings differ"},
		{"models edited after start", legacyYAML, strings.Replace(legacyYAML, "'32768'", "'16384'", 1), "model catalog differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, c := loadLegacy(t, tc.text)
			if tc.edited != "" {
				if err := os.WriteFile(c.Path, []byte(tc.edited), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := Migrate(c)
			if !errors.Is(err, ErrMigrationCheck) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if names := dirNames(t, dir); !slices.Equal(names, []string{"locallm.yaml"}) {
				t.Fatalf("files %q", names)
			}
		})
	}
}

// §5.8: models_file — сам locallm.yaml в любом написании пути: отказ до записи, байты locallm.yaml те же, новых файлов нет.
// Без проверки первая запись заменила бы locallm.yaml документом моделей, а вторая упала бы на изменённом ETag.
func TestMigrateRefusesModelsFileSameAsConfig(t *testing.T) {
	testEnv(t)
	for _, tc := range []struct{ name, spelling string }{
		{"same path", `\locallm.yaml`},
		{"other case", `\LOCALLM.YAML`},
		{"dot", `\.\locallm.yaml`},
		{"dot-dot", `\sub\..\locallm.yaml`},
		{"forward slash", `/locallm.yaml`},
		{"double separator", `\\locallm.yaml`},
		{"hard link", `\link.yaml`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("LOCALLM_TEST_CONFDIR", dir)
			p := filepath.Join(dir, "locallm.yaml")
			text := "models_file: '%LOCALLM_TEST_CONFDIR%" + tc.spelling + "'\n" + legacyYAML
			if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.name == "hard link" { // другое имя, тот же файл: ловит os.SameFile
				if err := os.Link(p, filepath.Join(dir, "link.yaml")); err != nil {
					t.Fatal(err)
				}
			}
			names := dirNames(t, dir)
			c, err := Load(p)
			if err != nil {
				t.Fatal(err)
			}
			if c.ModelsMode != ModeLegacy {
				t.Fatalf("mode %q", c.ModelsMode)
			}
			_, _, err = Migrate(c)
			if !errors.Is(err, ErrMigrationCheck) || !strings.Contains(err.Error(), "models_file is the same file as locallm.yaml") {
				t.Fatalf("err %v", err)
			}
			if got := readText(t, p); got != text {
				t.Fatalf("locallm.yaml changed:\n%s", got)
			}
			if got := dirNames(t, dir); !slices.Equal(got, names) {
				t.Fatalf("files %q, want %q", got, names)
			}
		})
	}
}

// §5.8: сбой второй записи — locallm.yaml с models, режим legacy при следующем старте; повтор переписывает models.yaml.
func TestMigrateSecondWriteFailureKeepsLegacy(t *testing.T) {
	dir, c := loadLegacy(t, legacyYAML)
	t.Cleanup(func() { migrateWrite = WriteFileAtomic })
	calls := 0
	migrateWrite = func(path string, data []byte, ifMatch string) (string, string, error) {
		calls++
		if calls == 2 {
			return "", "", errors.New("disk full")
		}
		return WriteFileAtomic(path, data, ifMatch)
	}
	if _, _, err := Migrate(c); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
	if got := readText(t, c.Path); got != legacyYAML {
		t.Fatalf("locallm.yaml changed:\n%s", got)
	}
	modelsPath := filepath.Join(dir, "models.yaml")
	c2, err := Load(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	wantWarn := "models_file " + modelsPath + " is not read: locallm.yaml still has models (legacy mode)"
	if c2.ModelsMode != ModeLegacy || !reflect.DeepEqual(c2.Models, c.Models) || !slices.Equal(c2.Warnings, []string{wantWarn}) {
		t.Fatalf("after the failure: mode %q warnings %q", c2.ModelsMode, c2.Warnings)
	}
	migrateWrite = WriteFileAtomic
	if _, _, err := Migrate(c2); err != nil {
		t.Fatal(err)
	}
	if copies, _ := filepath.Glob(modelsPath + ".*.bak"); len(copies) != 1 {
		t.Fatalf("copies of models.yaml %q", copies)
	}
	if again, err := Load(c.Path); err != nil || again.ModelsMode != ModeFile {
		t.Fatalf("after the retry: %v", err)
	}
}

// Ручная правка между стартом и переносом может испортить YAML: значение аргумента не попадает в ошибку.
func TestMigrateParseErrorDoesNotLeakArgument(t *testing.T) {
	dir, c := loadLegacy(t, legacyYAML)
	const secret = "SENTINEL-migration-argument"
	edited := strings.Replace(legacyYAML, "args: ['-c', '32768']", "args: [*"+secret+"]", 1)
	if err := os.WriteFile(c.Path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Migrate(c)
	if !errors.Is(err, ErrMigrationCheck) || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected safe migration check error: %v", err)
	}
	if readText(t, c.Path) != edited || !slices.Equal(dirNames(t, dir), []string{"locallm.yaml"}) {
		t.Fatal("failed migration must leave files unchanged")
	}
}
