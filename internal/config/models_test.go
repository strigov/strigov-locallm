package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// profilesYAML — models.yaml с профилями (файлы — заглушки testEnv).
const profilesYAML = `version: 1
models:
  qwen:
    path: '%LOCALLM_TEST_DIR%\model.gguf'
    args: ['-c', '32768']
    profiles:
      long: ['-c', '65536', '-ctk', 'q8_0']
      fast: ['--draft-max', '8']
  gemma:
    path: '%LOCALLM_TEST_DIR%\MODEL2.GGUF'
  tiny:
    path: '%LOCALLM_TEST_DIR%\model.gguf'
    profiles:
      big: ['-c', '8192']
`

// loadFileMode пишет во временный каталог locallm.yaml без ключа models (режим file) с default_args ['-c', '4096']
// и строками верхнего уровня extra; если modelsYAML не пуст — models.yaml рядом. Возвращает результат Load.
func loadFileMode(t *testing.T, modelsYAML, extra string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	if modelsYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  default_args: ['-c', '4096']\n" + extra
	p := filepath.Join(dir, "locallm.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// §5.4 спецификации v2: порядок каталога, имена, context_length по последнему -c, argv с args профиля после args модели.
func TestFileModeProfilesExpandInOrder(t *testing.T) {
	dir := testEnv(t)
	c, err := loadFileMode(t, profilesYAML, "swap:\n  default_model: qwen-long\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsMode != ModeFile {
		t.Fatalf("mode %q", c.ModelsMode)
	}
	type row struct {
		id, base, profile string
		ctx               int
	}
	var got []row
	for _, m := range c.Models {
		got = append(got, row{m.ID, m.Base, m.Profile, m.ContextLength})
	}
	want := []row{{"qwen", "qwen", "", 32768}, {"qwen-long", "qwen", "long", 65536}, {"qwen-fast", "qwen", "fast", 32768},
		{"gemma", "gemma", "", 4096}, {"tiny", "tiny", "", 4096}, {"tiny-big", "tiny", "big", 8192}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog %+v", got)
	}
	m, ok := c.Model("qwen-long")
	if !ok {
		t.Fatal("qwen-long not found")
	}
	wantArgv := []string{dir + `\llama-server.exe`, "-c", "4096", "-c", "32768", "-c", "65536", "-ctk", "q8_0",
		"-m", dir + `\model.gguf`, "--host", "127.0.0.1", "--port", "8081"}
	if argv := c.Argv(m); !reflect.DeepEqual(argv, wantArgv) {
		t.Fatalf("argv %q", argv)
	}
	if c.Models[3].Path != dir+`\MODEL2.GGUF` {
		t.Fatalf("gemma path %q", c.Models[3].Path)
	}
	doc, errs := ParseModels([]byte(profilesYAML))
	if errs != nil || !reflect.DeepEqual(c.ModelsDoc, doc) || c.ModelsDoc.Models[0].Path != `%LOCALLM_TEST_DIR%\model.gguf` {
		t.Fatalf("ModelsDoc %+v, errs %v", c.ModelsDoc, errs)
	}
}

// §5.3 спецификации v2: схема models.yaml.
func TestParseModelsSchema(t *testing.T) {
	doc, errs := ParseModels([]byte(profilesYAML))
	want := &ModelsDoc{Models: []DocModel{
		{ID: "qwen", Path: `%LOCALLM_TEST_DIR%\model.gguf`, Args: []string{"-c", "32768"}, Profiles: []DocProfile{
			{Name: "long", Args: []string{"-c", "65536", "-ctk", "q8_0"}}, {Name: "fast", Args: []string{"--draft-max", "8"}}}},
		{ID: "gemma", Path: `%LOCALLM_TEST_DIR%\MODEL2.GGUF`},
		{ID: "tiny", Path: `%LOCALLM_TEST_DIR%\model.gguf`, Profiles: []DocProfile{{Name: "big", Args: []string{"-c", "8192"}}}},
	}}
	if errs != nil || !reflect.DeepEqual(doc, want) {
		t.Fatalf("doc %+v errs %v", doc, errs)
	}
	for _, text := range []string{"version: 1\n", "version: 1\nmodels: {}\n", "version: 1\nmodels:\n"} {
		if doc, errs := ParseModels([]byte(text)); errs != nil || !reflect.DeepEqual(doc, &ModelsDoc{}) {
			t.Errorf("%q: doc %+v errs %v", text, doc, errs)
		}
	}
	for _, tc := range []struct{ name, text, want string }{
		{"empty file", "", "version: required (write version: 1)"},
		{"comments only", "# nothing\n", "version: required (write version: 1)"},
		{"no version", "models: {}\n", "version: required (write version: 1)"},
		{"version 2", "version: 2\n", "version (line 1): must be 1"},
		{"version string", "version: '1'\n", "version (line 1): must be 1"},
		{"version float", "version: 1.0\n", "version (line 1): must be 1"},
		{"not a mapping", "- 1\n", "line 1: must be a mapping with version and models"},
		{"unknown top key", "version: 1\nmodel: {}\n", `line 2: unknown key "model"`},
		{"duplicate top key", "version: 1\nversion: 1\n", `line 2: duplicate key "version"`},
		{"models not a mapping", "version: 1\nmodels: [a]\n", "models (line 2): must be a mapping of model id to settings"},
		{"duplicate model", "version: 1\nmodels:\n  a:\n    path: x\n  a:\n    path: y\n", "models.a (line 5): duplicate model id"},
		{"duplicate model key", "version: 1\nmodels:\n  a:\n    path: x\n    path: y\n", `models.a (line 5): duplicate key "path"`},
		{"unknown model key", "version: 1\nmodels:\n  a:\n    path: x\n    argz: []\n", `models.a (line 5): unknown key "argz"`},
		{"model not a mapping", "version: 1\nmodels:\n  a: x\n", "models.a (line 3): must be a mapping with path, args and profiles"},
		{"args not a list", "version: 1\nmodels:\n  a:\n    path: x\n    args: {k: v}\n", "models.a.args (line 5): must be a list of strings"},
		{"path not a string", "version: 1\nmodels:\n  a:\n    path: [x]\n", "models.a.path (line 4): must be a string"},
		{"profiles not a mapping", "version: 1\nmodels:\n  a:\n    path: x\n    profiles: [p]\n",
			"models.a.profiles (line 5): must be a mapping of profile name to an argument list"},
		{"duplicate profile", "version: 1\nmodels:\n  a:\n    path: x\n    profiles:\n      p: []\n      p: []\n",
			"models.a.profiles.p (line 7): duplicate profile name"},
		{"profile args not a list", "version: 1\nmodels:\n  a:\n    path: x\n    profiles:\n      p: {k: v}\n", "models.a.profiles.p (line 6): must be a list of strings"},
		{"syntax error", "version: 1\nmodels: [\n", "yaml: "},
		{"too large", "version: 1\n#" + strings.Repeat("x", MaxModelsFileBytes) + "\n", "file is larger than 1048576 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, errs := ParseModels([]byte(tc.text))
			if doc != nil || errs == nil {
				t.Fatalf("want errors, got doc %+v", doc)
			}
			if text := fmt.Sprint(errs); !strings.Contains(text, tc.want) {
				t.Fatalf("want %q in %s", tc.want, text)
			}
		})
	}
}

// §5.1 спецификации v2: режим file без models.yaml — пустой каталог, хост стартует.
func TestFileModeWithoutModelsFile(t *testing.T) {
	testEnv(t)
	c, err := loadFileMode(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsMode != ModeFile || len(c.Models) != 0 || c.ModelsDoc == nil || len(c.ModelsDoc.Models) != 0 || len(c.Warnings) != 0 {
		t.Fatalf("mode %q models %v doc %+v warnings %q", c.ModelsMode, c.Models, c.ModelsDoc, c.Warnings)
	}
	for _, text := range []string{"version: 1\n", "version: 1\nmodels: {}\n"} {
		if c, err := loadFileMode(t, text, ""); err != nil || len(c.Models) != 0 {
			t.Fatalf("%q: %v", text, err)
		}
	}
	_, err = loadFileMode(t, "", "swap:\n  default_model: qwen\n")
	if want := `swap.default_model: "qwen" is not a key of models or a <model>-<profile> name`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

// §5.3: models: в locallm.yaml (legacy) разбирает тот же декодер, с профилями и проверкой повторов.
func TestLegacyModeUsesTheSameDecoder(t *testing.T) {
	testEnv(t)
	head := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  default_args: ['-c', '4096']\n"
	c, err := loadText(t, head+"models:\n  qwen:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    args: ['-c', '32768']\n    profiles:\n      long: ['-c', '65536']\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsMode != ModeLegacy || !reflect.DeepEqual(c.ModelIDs(), []string{"qwen", "qwen-long"}) ||
		c.Models[0].ContextLength != 32768 || c.Models[1].ContextLength != 65536 {
		t.Fatalf("mode %q catalog %+v", c.ModelsMode, c.Models)
	}
	for _, tc := range []struct{ models, want string }{
		{"models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n", `models.tiny (line 7): duplicate key "path"`},
		{"models:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    profiles: [x]\n", "models.tiny.profiles (line 7): must be a mapping of profile name to an argument list"},
	} {
		if _, err := loadText(t, head+tc.models); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
}

// §5.1: в режиме legacy models_file не читается, но о нём есть предупреждение.
func TestLegacyWarnsWhenModelsFileExists(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte("version: 1\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "locallm.yaml")
	text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "models_file " + filepath.Join(dir, "models.yaml") + " is not read: locallm.yaml still has models (legacy mode)"
	if c.ModelsMode != ModeLegacy || !reflect.DeepEqual(c.ModelIDs(), []string{"tiny"}) || !reflect.DeepEqual(c.Warnings, []string{want}) {
		t.Fatalf("mode %q models %v warnings %q", c.ModelsMode, c.ModelIDs(), c.Warnings)
	}
}

// §5.1: в режиме legacy действует правило MVP «не меньше одной модели».
func TestModelsCountRules(t *testing.T) {
	testEnv(t)
	head := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n"
	for _, models := range []string{"models: {}\n", "models:\n", "models: ~\n"} {
		if _, err := loadText(t, head+models); err == nil || !strings.Contains(err.Error(), "models: at least one model is required") {
			t.Errorf("%q: %v", models, err)
		}
	}
	if _, err := loadText(t, head+"models: [a]\n"); err == nil || !strings.Contains(err.Error(), "models (line 3): must be a mapping of model id to settings") {
		t.Errorf("list instead of mapping: %v", err)
	}
}

// §5.2: явный models_file читается в режиме file; каталог вместо файла — ошибка с путём.
func TestExplicitModelsFile(t *testing.T) {
	dir := testEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "my-models.yaml"), []byte(profilesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadFileMode(t, "", "models_file: '%LOCALLM_TEST_DIR%\\my-models.yaml'\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsFile != dir+`\my-models.yaml` || len(c.Models) != 6 {
		t.Fatalf("models_file %q, %d models", c.ModelsFile, len(c.Models))
	}
	if _, err := loadFileMode(t, "", "models_file: '%LOCALLM_TEST_DIR%'\n"); err == nil || !strings.Contains(err.Error(), "models_file "+dir) {
		t.Fatalf("directory as models_file: %v", err)
	}
}

// Ошибки файла моделей называют сам файл: владелец правит его вручную.
func TestFileModeErrorsNameTheFile(t *testing.T) {
	testEnv(t)
	for _, tc := range []struct{ models, want string }{
		{"version: 2\n", "version (line 1): must be 1"},
		{"version: 1\nmodels:\n  tiny:\n    path: 'model.gguf'\n", `models.tiny.path: "model.gguf" must be an absolute path`},
	} {
		if _, err := loadFileMode(t, tc.models, ""); err == nil || !strings.Contains(err.Error(), `\models.yaml: `+tc.want) {
			t.Errorf("want %q after the file path, got %v", tc.want, err)
		}
	}
}

// §5.4: имена профилей, предел 64 символа у имени для клиентов, совпадения называют оба источника.
func TestProfileNamesAndCollisions(t *testing.T) {
	testEnv(t)
	model := func(id, extra string) string {
		return "  " + id + ":\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n" + extra
	}
	long60 := strings.Repeat("q", 60)
	for _, tc := range []struct{ name, models, want string }{
		{"invalid profile name", model("qwen", "    profiles:\n      '-x': []\n"),
			`models.qwen.profiles.-x: invalid profile name (must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$)`},
		{"profile name of 33 characters", model("qwen", "    profiles:\n      "+strings.Repeat("p", 33)+": []\n"), "invalid profile name"},
		{"client name longer than 64", model(long60, "    profiles:\n      abcde: []\n"),
			"models." + long60 + `.profiles.abcde: client name "` + long60 + `-abcde" is longer than 64 characters`},
		{"profile, then model", model("qwen", "    profiles:\n      long: []\n") + model("qwen-long", ""),
			`models.qwen-long: name "qwen-long" is already used by models.qwen.profiles.long`},
		{"model, then profile", model("qwen-long", "") + model("qwen", "    profiles:\n      long: []\n"),
			`models.qwen.profiles.long: name "qwen-long" is already used by models.qwen-long`},
		{"profiles of two models", model("a", "    profiles:\n      b-c: []\n") + model("a-b", "    profiles:\n      c: []\n"),
			`models.a-b.profiles.c: name "a-b-c" is already used by models.a.profiles.b-c`},
		{"profile context length", model("qwen", "    profiles:\n      bad: ['-c', 'x']\n"),
			"models.qwen.profiles.bad: flag -c: value is not a non-negative integer"},
		{"reserved flag in a profile", model("qwen", "    profiles:\n      p: ['-c', '1', '--port', '1']\n"),
			`models.qwen.profiles.p[2]: flag "--port" is reserved for the host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadFileMode(t, "version: 1\nmodels:\n"+tc.models, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// Имена различаются регистром — это разные модели (как в MVP).
	if c, err := loadFileMode(t, "version: 1\nmodels:\n"+model("Qwen", "")+model("qwen", ""), ""); err != nil || len(c.Models) != 2 {
		t.Fatalf("case-sensitive names: %v", err)
	}
}

// §5.5 п. 10 спецификации v2: пределы каталога; на границе документ проходит.
func TestCatalogLimits(t *testing.T) {
	testEnv(t)
	c := &Config{Llama: Llama{DefaultArgs: []string{}}}
	const path = `%LOCALLM_TEST_DIR%\model.gguf`
	models := func(n, profiles int) []DocModel {
		out := make([]DocModel, n)
		for i := range out {
			out[i] = DocModel{ID: fmt.Sprintf("m%d", i), Path: path}
			for j := 0; j < profiles; j++ {
				out[i].Profiles = append(out[i].Profiles, DocProfile{Name: fmt.Sprintf("p%d", j)})
			}
		}
		return out
	}
	list := func(n int) []string { return slices.Repeat([]string{"x"}, n) }
	for _, tc := range []struct {
		name string
		doc  *ModelsDoc
		want *FieldError // nil — документ проходит
	}{
		{"256 models", &ModelsDoc{Models: models(256, 0)}, nil},
		{"257 models", &ModelsDoc{Models: models(257, 0)}, &FieldError{"models", "at most 256 models"}},
		{"32 profiles", &ModelsDoc{Models: models(1, 32)}, nil},
		{"33 profiles", &ModelsDoc{Models: models(1, 33)}, &FieldError{"models[0].profiles", "at most 32 profiles"}},
		{"512 client names", &ModelsDoc{Models: models(16, 31)}, nil},
		{"513 client names", &ModelsDoc{Models: append(models(16, 31), DocModel{ID: "extra", Path: path})},
			&FieldError{"models", "at most 512 client names (models and profiles together)"}},
		{"256 arguments", &ModelsDoc{Models: []DocModel{{ID: "a", Path: path, Args: list(256)}}}, nil},
		{"257 arguments", &ModelsDoc{Models: []DocModel{{ID: "a", Path: path, Args: list(257)}}}, &FieldError{"models[0].args", "at most 256 arguments"}},
		{"257 profile arguments", &ModelsDoc{Models: []DocModel{{ID: "a", Path: path, Profiles: []DocProfile{{Name: "p", Args: list(257)}}}}},
			&FieldError{"models[0].profiles[0].args", "at most 256 arguments"}},
		{"4096-byte argument", &ModelsDoc{Models: []DocModel{{ID: "a", Path: path, Args: []string{"--alias", strings.Repeat("a", 4096)}}}}, nil},
		{"4097-byte argument", &ModelsDoc{Models: []DocModel{{ID: "a", Path: path, Args: []string{"--alias", strings.Repeat("a", 4097)}}}},
			&FieldError{"models[0].args[1]", "argument is longer than 4096 bytes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, errs := c.WithModels(tc.doc)
			if tc.want == nil {
				if errs != nil || n == nil {
					t.Fatalf("errs %v", errs)
				}
				return
			}
			if n != nil || !slices.Contains(errs, *tc.want) {
				t.Fatalf("want %+v in %+v", *tc.want, errs)
			}
		})
	}
}

// §4.4: WithModels называет поля по документу API, копирует глобальные поля и документ, старый конфиг не трогает.
func TestWithModelsReportsAPIFieldsAndCopies(t *testing.T) {
	dir := testEnv(t)
	c, err := loadFileMode(t, profilesYAML, "")
	if err != nil {
		t.Fatal(err)
	}
	bad := &ModelsDoc{Models: []DocModel{
		{ID: "ok", Path: `%LOCALLM_TEST_DIR%\model.gguf`},
		{ID: "bad id", Path: "model.gguf", Args: []string{"--port", "1"},
			Profiles: []DocProfile{{Name: "-p", Args: []string{"-c", "1", "--mmproj", "rel.gguf"}}}},
		{ID: "ok", Path: `%LOCALLM_TEST_DIR%\model.gguf`},
	}}
	n, errs := c.WithModels(bad)
	if n != nil {
		t.Fatal("invalid document must not produce a config")
	}
	for _, want := range []FieldError{
		{"models[1].id", "invalid model id (must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$)"},
		{"models[1].path", `must be an absolute path (C:\... or \\server\share\...)`},
		{"models[1].args[0]", `flag "--port" is reserved for the host`},
		{"models[1].profiles[0].name", "invalid profile name (must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$)"},
		{"models[1].profiles[0].args[3]", "--mmproj: value must be an absolute path"},
		{"models[2].id", `name "ok" is already used by models[0].id`},
	} {
		if !slices.Contains(errs, want) {
			t.Errorf("missing %+v in %+v", want, errs)
		}
	}
	good := &ModelsDoc{Models: []DocModel{{ID: "solo", Path: `%LOCALLM_TEST_DIR%\model.gguf`, Args: []string{"-c", "1024"},
		Profiles: []DocProfile{{Name: "big", Args: []string{"-c", "2048"}}}}}}
	n, errs = c.WithModels(good)
	if errs != nil || n == nil || n == c {
		t.Fatalf("errs %v", errs)
	}
	if !reflect.DeepEqual(n.ModelIDs(), []string{"solo", "solo-big"}) || n.Models[1].ContextLength != 2048 || n.Models[0].Path != dir+`\model.gguf` {
		t.Fatalf("catalog %+v", n.Models)
	}
	if len(c.Models) != 6 || !reflect.DeepEqual(n.Llama, c.Llama) || !reflect.DeepEqual(n.Server, c.Server) ||
		n.ModelsMode != c.ModelsMode || n.ModelsFile != c.ModelsFile {
		t.Fatal("globals must be copied and the old config left as is")
	}
	good.Models[0].Args[0] = "changed"
	good.Models[0].Profiles[0].Name = "changed"
	if n.ModelsDoc.Models[0].Args[0] != "-c" || n.ModelsDoc.Models[0].Profiles[0].Name != "big" ||
		n.ModelsDoc.Models[0].Path != `%LOCALLM_TEST_DIR%\model.gguf` {
		t.Fatalf("ModelsDoc must be a deep copy with raw strings: %+v", n.ModelsDoc)
	}
}

// §5.5 п. 13: default_model — имя развёрнутого каталога, с учётом регистра.
func TestCheckDefault(t *testing.T) {
	c := &Config{Models: []Model{{ID: "qwen"}, {ID: "qwen-long"}}}
	for _, ok := range []string{"", "qwen", "qwen-long"} {
		c.Swap.DefaultModel = ok
		if fe := CheckDefault(c); fe != nil {
			t.Fatalf("%q: %v", ok, fe)
		}
	}
	for _, bad := range []string{"ghost", "QWEN", "qwen-"} {
		c.Swap.DefaultModel = bad
		want := FieldError{"swap.default_model", fmt.Sprintf("%q is not a key of models or a <model>-<profile> name", bad)}
		if fe := CheckDefault(c); fe == nil || *fe != want {
			t.Fatalf("%q: %v", bad, fe)
		}
	}
}

func TestSameLaunch(t *testing.T) {
	a := Model{ID: "q", Base: "q", Path: `C:\m\q.gguf`, Args: []string{"-c", "1"}}
	if !a.SameLaunch(Model{ID: "q-long", Base: "q", Profile: "long", Path: `C:\m\q.gguf`, Args: []string{"-c", "1"}, ContextLength: 1}) {
		t.Fatal("same path and args must be the same launch")
	}
	if !(Model{Path: "x"}).SameLaunch(Model{Path: "x", Args: []string{}}) {
		t.Fatal("nil and empty args are the same launch")
	}
	for _, o := range []Model{
		{Path: `C:\m\other.gguf`, Args: []string{"-c", "1"}},
		{Path: `C:\m\q.gguf`, Args: []string{"-c", "2"}},
		{Path: `C:\m\q.gguf`, Args: []string{"1", "-c"}},
		{Path: `C:\m\q.gguf`},
	} {
		if a.SameLaunch(o) {
			t.Errorf("%+v must differ", o)
		}
	}
}

func TestLive(t *testing.T) {
	c1, c2 := &Config{Version: "1"}, &Config{Version: "2"}
	l := NewLive(c1)
	if l.Load() != c1 {
		t.Fatal("Load must return the initial config")
	}
	l.Store(c2)
	if l.Load() != c2 {
		t.Fatal("Load must return the stored config")
	}
}

// §5.5 п. 14: ошибки в списках профилей не содержат значений.
func TestProfileErrorsDoNotLeakSecrets(t *testing.T) {
	testEnv(t)
	const secret = "SENTINEL-profile-Vb8Nc"
	t.Setenv("LOCALLM_TEST_TOKEN", secret)
	long := strings.Repeat("S", 4097)
	for _, args := range []string{
		"['--api_key=%LOCALLM_TEST_TOKEN%']",
		"['--lora', '%LOCALLM_TEST_TOKEN%']",
		"['--control-vector', '%LOCALLM_TEST_DIR%\\mmproj.gguf,%LOCALLM_TEST_TOKEN%']",
		"['-c', '%LOCALLM_TEST_TOKEN%']",
		"['--alias', '" + long + "']",
	} {
		_, err := loadFileMode(t, "version: 1\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    profiles:\n      p: "+args+"\n", "")
		if err == nil {
			t.Fatalf("%.40s: want error", args)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), long[:64]) {
			t.Fatalf("value leaked: %v", err)
		}
	}
}

// §5.5 п. 14: ошибки схемы (не тот тип или тег у path, args модели, списка профиля) не содержат значения. yaml.v3
// v3.0.1 вставляет скаляр в текст ошибки: до 10 символов целиком, длиннее — первые 7.
func TestSchemaErrorsDoNotLeakValues(t *testing.T) {
	testEnv(t)
	const secret = "TOKEN123"
	model := "version: 1\nmodels:\n  a:\n"
	for _, tc := range []struct{ name, text, want string }{
		{"model args unknown anchor", model + "    path: x\n    args: [*" + secret + "]\n", "yaml: invalid value"},
		{"profile args unknown anchor", model + "    path: x\n    profiles: {p: [*" + secret + "]}\n", "yaml: invalid value"},
		{"model args scalar", model + "    path: x\n    args: '" + secret + "'\n", "models.a.args (line 5): must be a list of strings"},
		{"model args element with a tag", model + "    path: x\n    args: ['--alias', !!int " + secret + "]\n", "models.a.args (line 5): must be a list of strings"},
		{"profile args scalar", model + "    path: x\n    profiles: {p: '" + secret + "'}\n", "models.a.profiles.p (line 5): must be a list of strings"},
		{"profile args element with a tag", model + "    path: x\n    profiles:\n      p: ['--alias', !!bool " + secret + "]\n",
			"models.a.profiles.p (line 6): must be a list of strings"},
		{"path with a tag", model + "    path: !!int " + secret + "\n", "models.a.path (line 4): must be a string"},
		{"path list", model + "    path: ['" + secret + "']\n", "models.a.path (line 4): must be a string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, errs := ParseModels([]byte(tc.text))
			text := fmt.Sprint(errs)
			if doc != nil || !strings.Contains(text, tc.want) {
				t.Fatalf("want %q in %s", tc.want, text)
			}
			if strings.Contains(text, secret[:7]) {
				t.Fatalf("value leaked: %s", text)
			}
		})
	}
	// Режим legacy: тот же декодер для models: в locallm.yaml.
	_, err := loadText(t, "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    profiles: {p: '"+secret+"'}\n")
	if want := "models.tiny.profiles.p (line 6): must be a list of strings"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
	if strings.Contains(err.Error(), secret[:7]) {
		t.Fatalf("value leaked: %v", err)
	}
}

// §5.5 п. 10: пределы списка действуют и для llama.default_args; на границе конфиг загружается, значение в ошибку
// не попадает.
func TestDefaultArgsLimits(t *testing.T) {
	testEnv(t)
	long := strings.Repeat("S", 4097)
	for _, tc := range []struct {
		name string
		args []string
		want string // "" — конфиг загружается
	}{
		{"256 arguments", slices.Repeat([]string{"x"}, 256), ""},
		{"257 arguments", slices.Repeat([]string{"x"}, 257), "llama.default_args: at most 256 arguments"},
		{"4096-byte argument", []string{"--alias", long[:4096]}, ""},
		{"4097-byte argument", []string{"--alias", long}, "llama.default_args[1]: argument is longer than 4096 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Без ключа models и без models.yaml — режим file с пустым каталогом: проверяются только глобальные ключи.
			text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  default_args: ['" + strings.Join(tc.args, "', '") + "']\n"
			_, err := loadText(t, text)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), long[:64]) {
				t.Fatalf("value leaked: %.200v", err)
			}
		})
	}
}

// §5.7: ETag прочитанного models_file; нет файла — "absent"; режим legacy — пусто; WithModels копирует его.
func TestModelsETag(t *testing.T) {
	testEnv(t)
	c, err := loadFileMode(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsETag != `"absent"` {
		t.Fatalf("no file: %s", c.ModelsETag)
	}
	if c, err = loadFileMode(t, profilesYAML, ""); err != nil {
		t.Fatal(err)
	}
	if c.ModelsETag != ETag([]byte(profilesYAML), true) {
		t.Fatalf("file: %s", c.ModelsETag)
	}
	if n, errs := c.WithModels(&ModelsDoc{}); errs != nil || n.ModelsETag != c.ModelsETag {
		t.Fatalf("WithModels: errs %v", errs)
	}
	if c, err = Load(fixture("valid_minimal.yaml")); err != nil {
		t.Fatal(err)
	}
	if c.ModelsETag != "" {
		t.Fatalf("legacy: %s", c.ModelsETag)
	}
}
