// Package hwtest — общий разбор переменных окружения hw-тестов (§14.1).
package hwtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

type Model struct {
	Path string
	Args []string
}

// DefaultArgs — llama.default_args из §5.2.
var DefaultArgs = []string{"-ngl", "99", "-np", "1", "-fa", "on", "--cache-reuse", "256", "--no-webui"}

func need(t testing.TB, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}

func LlamaExe(t testing.TB) string   { return need(t, "LOCALLM_LLAMA_EXE") }
func SmallModel(t testing.TB) string { return need(t, "LOCALLM_SMALL_MODEL") }

// BigModels разбирает LOCALLM_BIG_MODELS: записи через ';', поля через '|': путь, затем аргументы.
func BigModels(t testing.TB) []Model {
	v := need(t, "LOCALLM_BIG_MODELS")
	var out []Model
	for _, rec := range strings.Split(v, ";") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "|")
		out = append(out, Model{Path: f[0], Args: f[1:]})
	}
	if len(out) == 0 {
		t.Skip("LOCALLM_BIG_MODELS has no entries")
	}
	return out
}

// NoOrphans останавливает тест, если llama-server.exe уже запущен (чужой процесс исказит замеры).
func NoOrphans(t testing.TB) {
	t.Helper()
	out, _ := exec.Command("tasklist", "/fi", "imagename eq llama-server.exe", "/fo", "csv", "/nh").Output()
	if strings.Contains(strings.ToLower(string(out)), "llama-server.exe") {
		t.Fatalf("llama-server.exe is already running; stop it before hw tests:\n%s", out)
	}
}

// Config — конфиг для hw-тестов с значениями §5.2.
func Config(t testing.TB, exe string, port int, models ...config.Model) *config.Config {
	t.Helper()
	return &config.Config{
		Server: config.Server{Listen: "127.0.0.1:8080"},
		Llama: config.Llama{ServerExe: exe, Port: port, DefaultArgs: append([]string{}, DefaultArgs...),
			LoadTimeout: 300 * time.Second, StopTimeout: 10 * time.Second, LogLines: 1000},
		Swap: config.Swap{SwapCooldown: 30 * time.Second, WaitTimeout: 210 * time.Second, VRAMFreeTimeout: 10 * time.Second,
			VRAMMarginMiB: 512, MaxQueue: 8, FailedTTL: 120 * time.Second},
		Vram:   config.Vram{Source: "auto"},
		Models: models,
	}
}
