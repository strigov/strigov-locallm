package testutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HostOptions — каталог хоста для тестов (NewHostDir). Все файлы — во временном каталоге теста.
type HostOptions struct {
	DefaultModel string   // swap.default_model; "" — нет
	Models       string   // строки моделей YAML с отступом 2; {models} заменяется каталогом моделей
	Stubs        []string // файлы в <Dir>\models: заголовок GGUF (llama, context_length 131072; имя с "mmproj" — проектор clip)
	Legacy       bool     // модели в locallm.yaml (режим legacy); иначе — в <Dir>\models.yaml (если Models не пуст)
	ServerExe    string   // llama.server_exe; "" — исполняемый файл теста
	LlamaPort    int      // llama.port; 0 — 18081
	StateFile    string   // server.state_file; "" — <Dir>\state.json
}

// HostDir — пути тестового каталога хоста.
type HostDir struct {
	Dir        string // временный каталог теста; LOCALAPPDATA указывает на него
	Config     string // <Dir>\locallm.yaml
	ModelsFile string // <Dir>\models.yaml
	ModelsDir  string // <Dir>\models — единственный корень models_dirs
	Keys       string // <Dir>\keys.txt
	AdminKey   string // <Dir>\admin-key.txt
	State      string // server.state_file
}

// NewHostDir пишет locallm.yaml (и models.yaml в режиме file) во временный каталог и задаёт LOCALAPPDATA на него:
// тест не может задеть конфиг, ключи и состояние владельца. server.listen — 127.0.0.1:18080 (Load порт не занимает),
// vram.source — none, swap_cooldown — 0s, wait_timeout — 30s, stop_timeout — 5s, default_args — [].
func NewHostDir(t testing.TB, o HostOptions) HostDir {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	h := HostDir{Dir: dir, Config: filepath.Join(dir, "locallm.yaml"), ModelsFile: filepath.Join(dir, "models.yaml"),
		ModelsDir: filepath.Join(dir, "models"), Keys: filepath.Join(dir, "keys.txt"),
		AdminKey: filepath.Join(dir, "admin-key.txt"), State: o.StateFile}
	if h.State == "" {
		h.State = filepath.Join(dir, "state.json")
	}
	if err := os.MkdirAll(h.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range o.Stubs {
		if strings.Contains(strings.ToLower(s), "mmproj") {
			GGUF(t, filepath.Join(h.ModelsDir, s), "clip", "", 0)
		} else {
			GGUF(t, filepath.Join(h.ModelsDir, s), "llama", "", 131072)
		}
	}
	exe := o.ServerExe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			t.Fatal(err)
		}
	}
	port := o.LlamaPort
	if port == 0 {
		port = 18081
	}
	var b strings.Builder
	fmt.Fprintf(&b, "server:\n  listen: 127.0.0.1:18080\n  api_keys_file: '%s'\n  admin_key_file: '%s'\n  state_file: '%s'\n",
		h.Keys, h.AdminKey, h.State)
	fmt.Fprintf(&b, "llama:\n  server_exe: '%s'\n  port: %d\n  default_args: []\n  stop_timeout: 5s\n", exe, port)
	b.WriteString("swap:\n  swap_cooldown: 0s\n  wait_timeout: 30s\n")
	if o.DefaultModel != "" {
		fmt.Fprintf(&b, "  default_model: %s\n", o.DefaultModel)
	}
	fmt.Fprintf(&b, "vram:\n  source: none\nlogging:\n  dir: '%s'\nmodels_dirs: ['%s']\n", filepath.Join(dir, "logs"), h.ModelsDir)
	models := strings.ReplaceAll(o.Models, "{models}", h.ModelsDir)
	if o.Legacy {
		b.WriteString("models:\n" + models)
	} else if o.Models != "" {
		writeText(t, h.ModelsFile, "version: 1\nmodels:\n"+models)
	}
	writeText(t, h.Config, b.String())
	return h
}

// WriteModels перезаписывает models.yaml: version: 1 и строки моделей (отступ 2, {models} — каталог моделей);
// models "" — пустой каталог.
func (h HostDir) WriteModels(t testing.TB, models string) {
	t.Helper()
	if models == "" {
		writeText(t, h.ModelsFile, "version: 1\n")
		return
	}
	writeText(t, h.ModelsFile, "version: 1\nmodels:\n"+strings.ReplaceAll(models, "{models}", h.ModelsDir))
}

// GGUF пишет синтетический заголовок GGUF v3 без тензоров: general.architecture = arch, general.type = kind
// (если не пусто) и <arch>.context_length = ctx (если не 0).
func GGUF(t testing.TB, path, arch, kind string, ctx uint32) {
	t.Helper()
	writeGGUF(t, path, arch, kind, ctx, 0)
}

// GGUFEmbd — заголовок модели как у GGUF без general.type, с <arch>.context_length = ctx (если не 0) и
// <arch>.embedding_length = embd (если не 0).
func GGUFEmbd(t testing.TB, path, arch string, ctx, embd uint32) {
	t.Helper()
	writeGGUF(t, path, arch, "", ctx, embd)
}

// writeGGUF — общий писатель заголовка GGUF v3: пары general.architecture, general.type, <arch>.context_length и
// <arch>.embedding_length (последние три — если заданы; числа — uint32).
func writeGGUF(t testing.TB, path, arch, kind string, ctx, embd uint32) {
	t.Helper()
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) {
		le(uint64(len(s)))
		b.WriteString(s)
	}
	n := uint64(1)
	for _, set := range []bool{kind != "", ctx != 0, embd != 0} {
		if set {
			n++
		}
	}
	b.WriteString("GGUF")
	le(uint32(3)) // версия
	le(uint64(0)) // tensor_count
	le(n)         // kv_count
	str("general.architecture")
	le(uint32(8)) // строка
	str(arch)
	if kind != "" {
		str("general.type")
		le(uint32(8))
		str(kind)
	}
	if ctx != 0 {
		str(arch + ".context_length")
		le(uint32(4)) // uint32
		le(ctx)
	}
	if embd != 0 {
		str(arch + ".embedding_length")
		le(uint32(4)) // uint32
		le(embd)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeText(t testing.TB, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
