package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testEnv создаёт заглушки файлов, на которые ссылаются testdata/config/*.yaml, и задаёт LOCALLM_TEST_DIR.
func testEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"llama-server.exe", "model.gguf", "MODEL2.GGUF", "mmproj.gguf", "template.jinja", "model.bin",
		filepath.Join("dir with space", "model.gguf"), "shard-00001-of-00002.gguf", "shard-00002-of-00002.gguf",
		"lonely-00001-of-00003.gguf", "lonely-00002-of-00003.gguf"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALLM_TEST_DIR", dir)
	t.Setenv("LOCALAPPDATA", dir)
	return dir
}

func fixture(name string) string { return filepath.Join("..", "..", "testdata", "config", name) }

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	dir := testEnv(t)
	c, err := Load(fixture("valid_minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(fixture("valid_minimal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	local := os.Getenv("LOCALAPPDATA")
	want := Config{
		Path: fixture("valid_minimal.yaml"),
		Server: Server{Listen: "127.0.0.1:8080", APIKeysFile: local + `\locallm\keys.txt`,
			AdminKeyFile: local + `\locallm\admin-key.txt`, StateFile: local + `\locallm\state.json`},
		Llama: Llama{ServerExe: dir + `\llama-server.exe`, Port: 8081,
			DefaultArgs:       []string{"-ngl", "99", "-np", "1", "-fa", "on", "--cache-reuse", "256", "--no-webui"},
			DefaultArgsSource: []string{"-ngl", "99", "-np", "1", "-fa", "on", "--cache-reuse", "256", "--no-webui"},
			LoadTimeout:       300 * time.Second, StopTimeout: 10 * time.Second, LogLines: 1000},
		Swap: Swap{SwapCooldown: 30 * time.Second, WaitTimeout: 210 * time.Second, VRAMFreeTimeout: 10 * time.Second,
			VRAMMarginMiB: 512, MaxQueue: 8, FailedTTL: 120 * time.Second},
		Vram:       Vram{Source: "auto"},
		Logging:    Logging{Dir: local + `\locallm\logs`, Level: "info", MaxSizeMiB: 10, MaxFiles: 5},
		ModelsMode: ModeLegacy,
		ModelsFile: filepath.Join(filepath.Dir(abs), "models.yaml"),
		ModelsDoc:  &ModelsDoc{Models: []DocModel{{ID: "tiny", Path: `%LOCALLM_TEST_DIR%\model.gguf`}}},
		Models:     []Model{{ID: "tiny", Base: "tiny", Path: dir + `\model.gguf`, Args: []string{}}},
	}
	if !reflect.DeepEqual(*c, want) {
		t.Fatalf("got  %+v\nwant %+v", *c, want)
	}
}

func TestLoadFullKeepsOrderAndContextLength(t *testing.T) {
	testEnv(t)
	c, err := Load(fixture("valid_full.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ModelIDs(); !reflect.DeepEqual(got, []string{"zeta", "alpha", "mid"}) {
		t.Fatalf("order %v", got)
	}
	ctx := map[string]int{}
	for _, m := range c.Models {
		ctx[m.ID] = m.ContextLength
	}
	if !reflect.DeepEqual(ctx, map[string]int{"zeta": 8192, "alpha": 4096, "mid": 0}) {
		t.Fatalf("context_length %v", ctx)
	}
	if c.Swap.SwapCooldown != 0 || c.Swap.FailedTTL != 0 || c.Server.Listen != "127.0.0.2:18080" || c.Vram.Source != "none" {
		t.Fatalf("explicit values not applied: %+v", c)
	}
}

func TestSpacesAndPercentLiteral(t *testing.T) {
	dir := testEnv(t)
	c, err := Load(fixture("valid_space_in_path.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Models[0].Path != dir+`\dir with space\model.gguf` {
		t.Fatalf("path %q", c.Models[0].Path)
	}
	c, err = Load(fixture("valid_percent_literal.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Models[0].Args[1] != "p%q" {
		t.Fatalf("args %q", c.Models[0].Args)
	}
}

func TestArgvOrder(t *testing.T) {
	c := &Config{Llama: Llama{ServerExe: `C:\llama\llama-server.exe`, Port: 8081, DefaultArgs: []string{"-ngl", "99"}}}
	m := Model{ID: "g", Path: `C:\m\a b.gguf`, Args: []string{"-c", "16384"}}
	want := []string{`C:\llama\llama-server.exe`, "-ngl", "99", "-c", "16384", "-m", `C:\m\a b.gguf`, "--host", "127.0.0.1", "--port", "8081"}
	if got := c.Argv(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q", got)
	}
}

func TestModelLookup(t *testing.T) {
	c := &Config{Models: []Model{{ID: "a"}, {ID: "B"}}}
	if _, ok := c.Model("a"); !ok {
		t.Fatal("a not found")
	}
	if _, ok := c.Model("b"); ok {
		t.Fatal("lookup must be case-sensitive")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	if _, err := os.Stat(`C:\llama\llama-server.exe`); err != nil {
		t.Skip("target PC files not present")
	}
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir) // ключи, state.json и журналы из примера раскрываются во временный каталог
	for src, dst := range map[string]string{"locallm.example.yaml": "locallm.yaml", "models.example.yaml": "models.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", src))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := Load(filepath.Join(dir, "locallm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsMode != ModeFile {
		t.Fatalf("mode=%q want %q", c.ModelsMode, ModeFile)
	}
	if got, want := c.ModelIDs(), []string{"qwen-coder", "qwen-coder-long", "gemma", "tiny"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog=%v want %v", got, want)
	}
	if m, _ := c.Model("qwen-coder-long"); m.ContextLength != 65536 {
		t.Fatalf("qwen-coder-long context_length=%d want 65536", m.ContextLength)
	}
	if len(c.ModelsDirs) != 1 || !strings.EqualFold(c.ModelsDirs[0], `C:\models`) {
		t.Fatalf("models_dirs=%q want [C:\\models]", c.ModelsDirs)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings: %q", c.Warnings)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	if got, want := DefaultConfigPath(), filepath.Join(dir, "locallm", "locallm.yaml"); got != want {
		t.Fatalf("config path=%q want=%q", got, want)
	}
}

func loadText(t *testing.T, text string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestExplicitEmptyDefaultArgs(t *testing.T) {
	testEnv(t)
	text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  default_args: []\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n"
	c, err := loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Llama.DefaultArgs) != 0 || c.Models[0].ContextLength != 0 {
		t.Fatalf("explicit empty arguments must replace defaults: %+v", c)
	}
}

func TestContextLengthUsesLastFlag(t *testing.T) {
	testEnv(t)
	for _, tc := range []struct {
		name, args string
		want       int
	}{
		{"inherited", "[]", 4096},
		{"model overrides defaults", "['--ctx-size', '8192']", 8192},
		{"last flag wins", "['--ctx-size', '8192', '-c', '16384']", 16384},
		{"underscore spelling", "['--ctx_size', '8192']", 8192},
		{"underscore spelling then short", "['--ctx_size', '8192', '-c', '16384']", 16384},
		{"zero means unknown", "['-c', '0']", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  default_args: ['-c', '4096']\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    args: " + tc.args + "\n"
			c, err := loadText(t, text)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Models[0].ContextLength; got != tc.want {
				t.Fatalf("context length=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestLoadAggregatesYAMLandValidationErrors(t *testing.T) {
	testEnv(t)
	text := "llama:\n  server_exe: '%LOCALLM_TEST_DIR%\\llama-server.exe'\n  typo: true\n  load_timeout: 300\n  port: 80\nlogging:\n  level: verbose\nmodels:\n  tiny:\n    path: '%LOCALLM_TEST_DIR%\\model.gguf'\n    argz: []\n"
	c, err := loadText(t, text)
	if c != nil || err == nil {
		t.Fatalf("invalid config must return no usable configuration: config=%v err=%v", c, err)
	}
	for _, want := range []string{"typo", "has no unit", "llama.port", "logging.level", "argz"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("combined error does not mention %q: %v", want, err)
		}
	}
}

// Неявный models_file считается от файла конфига, даже если его путь относительный и содержит '%'.
func TestModelsFileDefaultUsesConfigDirectory(t *testing.T) {
	testEnv(t)
	dir := filepath.Join(t.TempDir(), "100%")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "locallm.yaml")
	if err := os.WriteFile(p, []byte(serverText("", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(dir))
	c, err := Load(filepath.Join("100%", "locallm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsFile != filepath.Join(dir, "models.yaml") || c.Path != filepath.Join("100%", "locallm.yaml") {
		t.Fatalf("models_file %q, config path %q", c.ModelsFile, c.Path)
	}
}
