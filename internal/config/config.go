// Package config читает и проверяет locallm.yaml (§5).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Path       string
	Version    string
	Server     Server
	Llama      Llama
	Swap       Swap
	Vram       Vram
	Logging    Logging
	ModelsMode string     // ModeFile или ModeLegacy (§5.1 спецификации v2)
	ModelsFile string     // абсолютный путь models.yaml (§5.2)
	ModelsDirs []string   // канонические корни models_dirs (§5.2, PV1); недоступные пропущены
	ModelsDoc  *ModelsDoc // документ моделей: строки до раскрытия %VAR% (§4.4)
	ModelsETag string     // режим file: ETag прочитанного models_file ("absent" — файла не было); legacy — "". WithModels копирует его, новый ставит вызывающий после записи
	Models     []Model    // развёрнутый каталог (§5.4) в порядке файла
	Warnings   []string   // предупреждения загрузки (§5.2 v2); main пишет их в журнал строкой "config warning"
}

type Server struct {
	Listen       string
	APIKeysFile  string
	AdminKeyFile string   // §5.2 спецификации v2
	AdminHosts   []string // нижний регистр, IPv6 без скобок (§5.2 v2)
	StateFile    string   // §5.2 v2
}

type Llama struct {
	ServerExe         string
	Port              int
	DefaultArgs       []string
	DefaultArgsSource []string // как в locallm.yaml, без раскрытия %VAR% — для панели (PV4, §12.2 v2)
	LoadTimeout       time.Duration
	StopTimeout       time.Duration
	LogLines          int
}

type Swap struct {
	DefaultModel    string
	SwapCooldown    time.Duration
	WaitTimeout     time.Duration
	VRAMFreeTimeout time.Duration
	VRAMMarginMiB   int64
	MaxQueue        int
	FailedTTL       time.Duration
}

type Vram struct {
	Source string
}

type Logging struct {
	Dir        string
	Level      string
	MaxSizeMiB int
	MaxFiles   int
}

type Model struct {
	ID            string   // имя для клиентов: "<base>" или "<base>-<profile>" (§5.4 спецификации v2)
	Base          string   // ключ модели в файле
	Profile       string   // "" — базовый вариант
	Path          string   // после раскрытия %VAR%
	Args          []string // args модели, затем args профиля; после раскрытия %VAR%
	ContextLength int
}

// SameLaunch — совпадают ли Path и Args (§4.4 спецификации v2); основа restart_required (§6.8).
func (m Model) SameLaunch(o Model) bool {
	return m.Path == o.Path && slices.Equal(m.Args, o.Args)
}

// Значения по умолчанию — таблица §5.2. Меняются только вместе со спецификацией.
var defaultArgs = []string{"-ngl", "99", "-np", "1", "-fa", "on", "--cache-reuse", "256", "--no-webui"}

const (
	defaultListen       = "127.0.0.1:8080"
	defaultKeysFile     = `%LOCALAPPDATA%\locallm\keys.txt`
	defaultAdminKeyFile = `%LOCALAPPDATA%\locallm\admin-key.txt`
	defaultStateFile    = `%LOCALAPPDATA%\locallm\state.json`
	defaultLogDir       = `%LOCALAPPDATA%\locallm\logs`
)

// DefaultConfigPath — %LOCALAPPDATA%\locallm\locallm.yaml (§4.7).
func DefaultConfigPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "locallm", "locallm.yaml")
}

// Load читает, раскрывает и проверяет конфиг и модели (§5.1 спецификации v2). Все нарушения возвращаются одной ошибкой.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return load(path, data, readModelsLimited) // models_file: не больше 1 МиБ + 1 байт в памяти (F3-D1)
}

// load — Load по уже прочитанным байтам locallm.yaml; readModels читает models_file (Migrate подставляет новый
// файл из памяти).
func load(path string, data []byte, readModels func(string) ([]byte, error)) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c, models, errs := parse(abs, data)
	if c != nil {
		errs = append(errs, loadModels(c, models, readModels)...)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("config %s:\n%w", path, errors.Join(errs...))
	}
	c.Path = path
	return c, nil
}

// parse разбирает глобальные настройки locallm.yaml; abs — его абсолютный путь (от него считается models_file
// по умолчанию). Узел models возвращается как есть (Kind 0 — ключа нет): его разбирает loadModels.
func parse(abs string, data []byte) (*Config, *yaml.Node, []error) {
	var raw rawConfig
	var errs []error
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if !errors.As(err, &te) {
			return nil, nil, []error{fmt.Errorf("yaml: %s", safeYAMLError(err.Error()))}
		}
		for _, m := range te.Errors {
			errs = append(errs, errors.New(safeYAMLError(m)))
		}
	}
	c := &Config{}
	c.Server.Listen = strOr(raw.Server.Listen, defaultListen)
	c.Server.APIKeysFile = expandField(&errs, "server.api_keys_file", strOr(raw.Server.APIKeysFile, defaultKeysFile))
	c.Server.AdminKeyFile = expandField(&errs, "server.admin_key_file", strOr(raw.Server.AdminKeyFile, defaultAdminKeyFile))
	c.Server.AdminHosts = raw.Server.AdminHosts
	c.Server.StateFile = expandField(&errs, "server.state_file", strOr(raw.Server.StateFile, defaultStateFile))
	c.Llama.ServerExe = expandField(&errs, "llama.server_exe", strOr(raw.Llama.ServerExe, ""))
	c.Llama.Port = intOr(raw.Llama.Port, 8081)
	if raw.Llama.DefaultArgs != nil {
		checkListLimits(&errs, "llama.default_args", *raw.Llama.DefaultArgs)
		c.Llama.DefaultArgs = expandArgs(&errs, "llama.default_args", *raw.Llama.DefaultArgs)
		c.Llama.DefaultArgsSource = append([]string{}, *raw.Llama.DefaultArgs...)
	} else {
		c.Llama.DefaultArgs = append([]string{}, defaultArgs...)
		c.Llama.DefaultArgsSource = append([]string{}, defaultArgs...)
	}
	c.Llama.LoadTimeout = durOr(raw.Llama.LoadTimeout, 300*time.Second)
	c.Llama.StopTimeout = durOr(raw.Llama.StopTimeout, 10*time.Second)
	c.Llama.LogLines = intOr(raw.Llama.LogLines, 1000)
	c.Swap.DefaultModel = strOr(raw.Swap.DefaultModel, "")
	c.Swap.SwapCooldown = durOr(raw.Swap.SwapCooldown, 30*time.Second)
	c.Swap.WaitTimeout = durOr(raw.Swap.WaitTimeout, 210*time.Second)
	c.Swap.VRAMFreeTimeout = durOr(raw.Swap.VRAMFreeTimeout, 10*time.Second)
	c.Swap.VRAMMarginMiB = 512
	if raw.Swap.VRAMMarginMiB != nil {
		c.Swap.VRAMMarginMiB = *raw.Swap.VRAMMarginMiB
	}
	c.Swap.MaxQueue = intOr(raw.Swap.MaxQueue, 8)
	c.Swap.FailedTTL = durOr(raw.Swap.FailedTTL, 120*time.Second)
	c.Vram.Source = strOr(raw.Vram.Source, "auto")
	c.Logging.Dir = expandField(&errs, "logging.dir", strOr(raw.Logging.Dir, defaultLogDir))
	c.Logging.Level = strOr(raw.Logging.Level, "info")
	c.Logging.MaxSizeMiB = intOr(raw.Logging.MaxSizeMiB, 10)
	c.Logging.MaxFiles = intOr(raw.Logging.MaxFiles, 5)
	if raw.ModelsFile != nil {
		c.ModelsFile = expandField(&errs, "models_file", *raw.ModelsFile)
	} else {
		c.ModelsFile = filepath.Join(filepath.Dir(abs), "models.yaml") // уже абсолютный, %VAR% не раскрывается
	}
	c.ModelsDirs = expandArgs(&errs, "models_dirs", raw.ModelsDirs)

	errs = append(errs, validate(c)...)
	errs = append(errs, distinctFiles(abs, c)...)
	return c, &raw.Models, errs
}

// Model ищет модель по точному имени (с учётом регистра).
func (c *Config) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// ModelIDs — имена моделей в порядке файла.
func (c *Config) ModelIDs() []string {
	ids := make([]string, len(c.Models))
	for i, m := range c.Models {
		ids[i] = m.ID
	}
	return ids
}

// Argv — итоговая командная строка llama-server (§7.2 п. 2); её же показывает /models status.args.
func (c *Config) Argv(m Model) []string {
	argv := make([]string, 0, 7+len(c.Llama.DefaultArgs)+len(m.Args))
	argv = append(argv, c.Llama.ServerExe)
	argv = append(argv, c.Llama.DefaultArgs...)
	argv = append(argv, m.Args...)
	return append(argv, "-m", m.Path, "--host", "127.0.0.1", "--port", strconv.Itoa(c.Llama.Port))
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func durOr(p *Duration, def time.Duration) time.Duration {
	if p == nil {
		return def
	}
	return time.Duration(*p)
}
