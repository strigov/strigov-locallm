package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// countCanonical считает вызовы canonicalPath до конца теста.
func countCanonical(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := canonicalPath
	canonicalPath = func(p string) (string, error) {
		*n++
		return old(p)
	}
	t.Cleanup(func() { canonicalPath = old })
	return n
}

// F4-D1 (ledger первого плана): пути шардов строятся по одному; проверка останавливается на первом шарде,
// который не разрешается, а не строит 99999 путей заранее.
func TestPanelShardPathsAreLazy(t *testing.T) {
	f := newPanelFiles(t)
	huge := filepath.Join(f.root, "huge-00001-of-99999.gguf")
	if err := os.WriteFile(huge, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := countCanonical(t)
	want := []FieldError{{"models[0].path", msgPanelResolve}}
	if errs := CheckPanel(f.c, nil, docWith(huge, nil, nil)); !reflect.DeepEqual(errs, want) {
		t.Fatalf("got %+v, want %+v", errs, want)
	}
	if *calls != 2 {
		t.Fatalf("Canonical called %d times, want 2 (first shard, then the missing second one)", *calls)
	}
	if allocs := testing.AllocsPerRun(5, func() { CheckPanel(f.c, nil, docWith(huge, nil, nil)) }); allocs > 500 {
		t.Fatalf("%v allocations per check: shard paths are built up front", allocs)
	}
	n := 0
	for i, p := range shards(`C:\m\x-00001-of-99999.gguf`) {
		n++
		if want := fmt.Sprintf(`C:\m\x-%05d-of-99999.gguf`, n); i != n || p != want {
			t.Fatalf("shard %d: %d %q, want %q", n, i, p, want)
		}
		if n == 3 {
			break
		}
	}
	if n != 3 {
		t.Fatalf("iterated %d shards, want 3", n)
	}
}

// F4-D2 (ledger первого плана): путь панели на другом томе отвергается по тексту, без Canonical, то есть без
// обращения к чужому серверу или диску.
func TestPanelOtherVolumeWithoutDiskAccess(t *testing.T) {
	f := newPanelFiles(t)
	other := `Q:\models\x.gguf`
	if strings.EqualFold(filepath.VolumeName(f.root), "Q:") {
		other = `R:\models\x.gguf`
	}
	calls := countCanonical(t)
	for _, p := range []string{`\\attacker.invalid\share\x.gguf`, `//attacker.invalid/share/x.gguf`, other} {
		want := []FieldError{{"models[0].path", msgPanelOutside}}
		if errs := CheckPanel(f.c, nil, docWith(p, nil, nil)); !reflect.DeepEqual(errs, want) {
			t.Errorf("path %s: %+v", p, errs)
		}
		want = []FieldError{{"models[0].args[1]", "--mmproj: " + msgPanelOutside}}
		if errs := CheckPanel(f.c, nil, docWith(f.model, []string{"--mmproj", p}, nil)); !reflect.DeepEqual(errs, want) {
			t.Errorf("--mmproj %s: %+v", p, errs)
		}
	}
	if *calls != 3 { // только f.model, по разу на круг
		t.Fatalf("Canonical called %d times, want 3", *calls)
	}
	for _, tc := range []struct {
		roots []string
		p     string
		want  bool
	}{
		{[]string{`\\srv\share\m`}, `\\SRV\Share\m\a.gguf`, true},
		{[]string{`\\srv\share\m`}, `//srv/share/m/a.gguf`, true},
		{[]string{`\\srv\share\m`}, `\\srv\other\a.gguf`, false},
		{[]string{`C:\m`, `D:\m`}, `d:\x\a.gguf`, true},
		{nil, `C:\a.gguf`, false},
		{[]string{`C:\m`}, `m\a.gguf`, false},
	} {
		if got := onRootVolume(tc.roots, tc.p); got != tc.want {
			t.Errorf("onRootVolume(%q, %q) = %v", tc.roots, tc.p, got)
		}
	}
}

// FR1-D1 (ledger первого плана): в стиле API тексты ошибок пути не содержат путь после раскрытия %VAR%.
func TestAPIPathErrorsHaveNoValues(t *testing.T) {
	testEnv(t)
	const secret = "hf_SuperSecretValue123"
	t.Setenv("LOCALLM_SECRET", secret)
	dir := t.TempDir()
	for _, name := range []string{secret + ".bin", secret + "-00001-of-00003.gguf", secret + "-00002-of-00003.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, secret+"-dir.gguf"), 0o755); err != nil {
		t.Fatal(err)
	}
	at := func(name string) string { return filepath.Join(dir, "%LOCALLM_SECRET%"+name) }
	doc := &ModelsDoc{Models: []DocModel{
		{ID: "m0", Path: `%LOCALLM_SECRET%\x.gguf`},
		{ID: "m1", Path: at("-missing.gguf")},
		{ID: "m2", Path: at("-dir.gguf")},
		{ID: "m3", Path: at(".bin")},
		{ID: "m4", Path: at("-00002-of-00003.gguf")},
		{ID: "m5", Path: at("-00001-of-00003.gguf")},
	}}
	_, errs := (&Config{}).WithModels(doc)
	want := []FieldError{
		{"models[0].path", `must be an absolute path (C:\... or \\server\share\...)`},
		{"models[1].path", "file does not exist"},
		{"models[2].path", "is a directory, not a file"},
		{"models[3].path", "must have extension .gguf"},
		{"models[4].path", "is not the first shard (point path at the -00001-of-N.gguf file)"},
		{"models[5].path", "shard 3 is missing"},
	}
	if !reflect.DeepEqual(errs, want) {
		t.Fatalf("got %+v\nwant %+v", errs, want)
	}
	data := "version: 1\nmodels:\n  rel:\n    path: '%LOCALLM_SECRET%\\x.gguf'\n  lonely:\n    path: '" + at("-00001-of-00003.gguf") + "'\n"
	_, ferrs := (&Config{}).WithModelsFile([]byte(data), true)
	fwant := []FieldError{
		{"models.rel.path", `must be an absolute path (C:\... or \\server\share\...)`},
		{"models.lonely.path", "shard 3 is missing"},
	}
	if !reflect.DeepEqual(ferrs, fwant) {
		t.Fatalf("WithModelsFile: got %+v\nwant %+v", ferrs, fwant)
	}
	for _, e := range append(errs, ferrs...) {
		if strings.Contains(e.Error(), secret) {
			t.Fatalf("value leaked: %v", e)
		}
	}
}

// §5.9 спецификации v2: «Применить файл» проверяет файл как при старте, включая CheckDefault; режим file.
func TestWithModelsFile(t *testing.T) {
	testEnv(t)
	c, err := loadFileMode(t, profilesYAML, "swap:\n  default_model: qwen-long\n")
	if err != nil {
		t.Fatal(err)
	}
	applied := c.ModelsETag
	gemmaOnly := []byte("version: 1\nmodels:\n  gemma:\n    path: '%LOCALLM_TEST_DIR%\\MODEL2.GGUF'\n")
	want := []FieldError{{"swap.default_model", `"qwen-long" is not a key of models or a <model>-<profile> name`}}
	if n, errs := c.WithModelsFile(gemmaOnly, true); n != nil || !reflect.DeepEqual(errs, want) {
		t.Fatalf("default model: %v %+v", n, errs)
	}
	data := []byte(profilesYAML + "  extra:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n")
	n, errs := c.WithModelsFile(data, true)
	if errs != nil {
		t.Fatalf("errs %+v", errs)
	}
	if n.ModelsMode != ModeFile || n.ModelsETag != ETag(data, true) || len(n.Models) != len(c.Models)+1 ||
		n.ModelsDoc.Models[3].ID != "extra" || c.ModelsETag != applied {
		t.Fatalf("config %+v", n)
	}
	if _, errs := c.WithModelsFile([]byte("version: 2\n"), true); len(errs) != 1 || errs[0].Field != "models_file" {
		t.Fatalf("bad file: %+v", errs)
	}
	c2 := *c
	c2.Swap.DefaultModel = ""
	if n, errs := c2.WithModelsFile(nil, false); errs != nil || len(n.Models) != 0 || n.ModelsETag != `"absent"` {
		t.Fatalf("absent file: %v %+v", n, errs)
	}
}

// F3-D1 (ledger первого плана): models_file читается не больше чем на 1 МиБ + 1 байт, ETag — от всего файла.
func TestReadModelsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	data, etag, exists, err := ReadModelsFile(p)
	if err != nil || exists || data != nil || etag != `"absent"` {
		t.Fatalf("absent: %q %q %v %v", data, etag, exists, err)
	}
	big := bytes.Repeat([]byte("# comment line\n"), 3<<20/15+1)
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatal(err)
	}
	data, etag, exists, err = ReadModelsFile(p)
	if err != nil || !exists || len(data) != MaxModelsFileBytes+1 || etag != ETag(big, true) {
		t.Fatalf("big: len %d, etag %q, %v %v", len(data), etag, exists, err)
	}
	if _, errs := ParseModels(data); len(errs) != 1 || !strings.Contains(errs[0].Error(), "larger than 1048576 bytes") {
		t.Fatalf("ParseModels: %v", errs)
	}
	small := []byte("version: 1\n")
	if err := os.WriteFile(p, small, 0o600); err != nil {
		t.Fatal(err)
	}
	if data, etag, exists, err = ReadModelsFile(p); err != nil || !exists || string(data) != string(small) || etag != ETag(small, true) {
		t.Fatalf("small: %q %q %v %v", data, etag, exists, err)
	}
	testEnv(t)
	if _, err := loadFileMode(t, string(big), ""); err == nil || !strings.Contains(err.Error(), "larger than 1048576 bytes") {
		t.Fatalf("Load: %v", err)
	}
}

// §5.5 п. 10 спецификации v2 без диска: шаг 3 PUT.
func TestCheckLimits(t *testing.T) {
	if errs := CheckLimits(nil); errs != nil {
		t.Fatalf("nil: %+v", errs)
	}
	if errs := CheckLimits(docWith(`C:\m\a.gguf`, []string{"-c", "4096"}, []string{"--x"})); errs != nil {
		t.Fatalf("ok: %+v", errs)
	}
	doc := &ModelsDoc{}
	for i := range 257 {
		doc.Models = append(doc.Models, DocModel{ID: fmt.Sprint("m", i)})
	}
	doc.Models[0].Args = make([]string, 257)
	doc.Models[1].Args = []string{strings.Repeat("a", 4097)}
	doc.Models[2].Profiles = make([]DocProfile, 33)
	doc.Models[2].Profiles[0].Args = []string{strings.Repeat("s", 4097)}
	want := []FieldError{
		{"models", "at most 256 models"},
		{"models[0].args", "at most 256 arguments"},
		{"models[1].args[0]", "argument is longer than 4096 bytes"},
		{"models[2].profiles", "at most 32 profiles"},
		{"models[2].profiles[0].args[0]", "argument is longer than 4096 bytes"},
	}
	if errs := CheckLimits(doc); !reflect.DeepEqual(errs, want) {
		t.Fatalf("got %+v", errs)
	}
	names := &ModelsDoc{}
	for i := range 200 {
		names.Models = append(names.Models, DocModel{ID: fmt.Sprint("m", i), Profiles: []DocProfile{{Name: "a"}, {Name: "b"}}})
	}
	if errs := CheckLimits(names); !reflect.DeepEqual(errs, []FieldError{{"models", "at most 512 client names (models and profiles together)"}}) {
		t.Fatalf("names: %+v", errs)
	}
}

// F2-D2 и F5-D2 (ledger первого плана): state.json, models.yaml и файлы ключей — разные файлы.
func TestDistinctFiles(t *testing.T) {
	testEnv(t)
	const head = "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
	for _, tc := range []struct{ text, want string }{
		{"server:\n  state_file: '%LOCALLM_TEST_CONFDIR%\\LOCALLM.YAML'\n", "server.state_file: must differ from the config file"},
		{"server:\n  state_file: '%LOCALLM_TEST_CONFDIR%\\models.yaml'\n", "server.state_file: must differ from models_file"},
		{"server:\n  state_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\n  api_keys_file: '%LOCALLM_TEST_CONFDIR%\\K.TXT'\n",
			"server.state_file: must differ from server.api_keys_file"},
		{"server:\n  state_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\n  admin_key_file: '%LOCALLM_TEST_CONFDIR%\\.\\k.txt'\n",
			"server.state_file: must differ from server.admin_key_file"},
		{"server:\n  api_keys_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\nmodels_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\n",
			"models_file: must differ from server.api_keys_file"},
		{"server:\n  admin_key_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\nmodels_file: '%LOCALLM_TEST_CONFDIR%\\k.txt'\n",
			"models_file: must differ from server.admin_key_file"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("LOCALLM_TEST_CONFDIR", dir)
			p := filepath.Join(dir, "locallm.yaml")
			if err := os.WriteFile(p, []byte(head+tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// F2-D1 (ledger первого плана): admin_hosts и NormalizeHost дают каноническую запись netip.
func TestNormalizeHostCanonical(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"[FD7A:115C:A1E0:0:0:0:0:1]", "fd7a:115c:a1e0::1", true},
		{"fd7a:115c:a1e0:0::1", "fd7a:115c:a1e0::1", true},
		{"::FFFF:100.64.0.1", "100.64.0.1", true},
		{"PC.Tail1234.TS.NET", "pc.tail1234.ts.net", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"[::ffff:1.2.3.4]", "", false},
		{"[1.2.3.4]", "", false},
		{"fe80::1%eth0", "", false},
		{"[fe80::1%eth0]", "", false},
		{"localhost.", "", false},
		{"[::1]:8080", "", false},
	} {
		if got, ok := NormalizeHost(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeHost(%q) = %q, %v", tc.in, got, ok)
		}
	}
	testEnv(t)
	c, err := loadText(t, serverText("  admin_hosts: ['FD7A:115C:A1E0:0:0:0:0:1', '::ffff:100.64.0.1']\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fd7a:115c:a1e0::1", "100.64.0.1"}; !reflect.DeepEqual(c.Server.AdminHosts, want) {
		t.Fatalf("admin_hosts %q", c.Server.AdminHosts)
	}
}

// F5-D4 (ledger первого плана): чистка копий не удаляет копию этого вызова, даже если у неё самая старая метка времени.
func TestWriteFileAtomicKeepsOwnBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("models.yaml.29991231T000000Z-%d.bak", i)), []byte("future"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := writeNow
	writeNow = func() time.Time { return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC) }
	t.Cleanup(func() { writeNow = old })
	_, backup, err := WriteFileAtomic(p, []byte("two"), ETag([]byte("one"), true))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(backup) != "models.yaml.20200102T030405Z.bak" || readText(t, backup) != "one" {
		t.Fatalf("backup %q", backup)
	}
	names := dirNames(t, dir)
	if len(names) != 11 || slices.Contains(names, "models.yaml.29991231T000000Z-0.bak") || !slices.Contains(names, filepath.Base(backup)) {
		t.Fatalf("files %q", names)
	}
}

// F5-D1 (ledger первого плана): перенос не пишет models.yaml поверх файлов ключей и state.json.
func TestMigrateRefusesKeyAndStateFiles(t *testing.T) {
	for _, name := range []string{"server.api_keys_file", "server.admin_key_file", "server.state_file"} {
		t.Run(name, func(t *testing.T) {
			dir, c := loadLegacy(t, legacyYAML)
			target := map[string]string{"server.api_keys_file": c.Server.APIKeysFile,
				"server.admin_key_file": c.Server.AdminKeyFile, "server.state_file": c.Server.StateFile}[name]
			c.ModelsFile = target
			before := dirNames(t, dir)
			_, _, err := Migrate(c)
			if !errors.Is(err, ErrMigrationCheck) || !strings.Contains(err.Error(), "models_file is the same file as "+name) {
				t.Fatalf("err %v", err)
			}
			if !slices.Equal(dirNames(t, dir), before) || readText(t, filepath.Join(dir, "locallm.yaml")) != legacyYAML {
				t.Fatal("files changed")
			}
			if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s was written: %v", name, err)
			}
		})
	}
}
