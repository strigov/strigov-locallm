package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration — строка в формате time.ParseDuration. Число без единицы — ошибка (кроме 0).
// Ошибка возвращается как *yaml.TypeError, чтобы yaml.v3 продолжил разбор и собрал все ошибки.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	fail := func(format string, a ...any) error {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: ", n.Line) + fmt.Sprintf(format, a...)}}
	}
	if n.Kind != yaml.ScalarNode {
		return fail("duration must be a string like 30s")
	}
	if n.Value == "0" {
		*d = 0
		return nil
	}
	if n.Tag == "!!int" || n.Tag == "!!float" {
		return fail("duration has no unit (write e.g. 30s)")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fail("invalid duration (use e.g. 30s or 5m)")
	}
	*d = Duration(v)
	return nil
}

type rawConfig struct {
	Server     rawServer  `yaml:"server"`
	Llama      rawLlama   `yaml:"llama"`
	Swap       rawSwap    `yaml:"swap"`
	Vram       rawVram    `yaml:"vram"`
	Logging    rawLogging `yaml:"logging"`
	ModelsFile *string    `yaml:"models_file"`
	ModelsDirs []string   `yaml:"models_dirs"`
	Models     yaml.Node  `yaml:"models"`
}

type rawServer struct {
	Listen       *string  `yaml:"listen"`
	APIKeysFile  *string  `yaml:"api_keys_file"`
	AdminKeyFile *string  `yaml:"admin_key_file"`
	AdminHosts   []string `yaml:"admin_hosts"`
	StateFile    *string  `yaml:"state_file"`
}

type rawLlama struct {
	ServerExe   *string   `yaml:"server_exe"`
	Port        *int      `yaml:"port"`
	DefaultArgs *[]string `yaml:"default_args"`
	LoadTimeout *Duration `yaml:"load_timeout"`
	StopTimeout *Duration `yaml:"stop_timeout"`
	LogLines    *int      `yaml:"log_lines"`
}

type rawSwap struct {
	DefaultModel    *string   `yaml:"default_model"`
	SwapCooldown    *Duration `yaml:"swap_cooldown"`
	WaitTimeout     *Duration `yaml:"wait_timeout"`
	VRAMFreeTimeout *Duration `yaml:"vram_free_timeout"`
	VRAMMarginMiB   *int64    `yaml:"vram_margin_mib"`
	MaxQueue        *int      `yaml:"max_queue"`
	FailedTTL       *Duration `yaml:"failed_ttl"`
}

type rawVram struct {
	Source *string `yaml:"source"`
}

type rawLogging struct {
	Dir        *string `yaml:"dir"`
	Level      *string `yaml:"level"`
	MaxSizeMiB *int    `yaml:"max_size_mib"`
	MaxFiles   *int    `yaml:"max_files"`
}

// Формы текстов ошибок yaml.v3 v3.0.1, в которые попадает значение.
var (
	// Ошибка типа: "line 3: cannot unmarshal !!str `TOKEN123` into []string" (скаляр до 10 символов целиком,
	// длиннее — первые 7 и "..."; у !!seq и !!map значения нет). Жадное .* берёт значение до последнего " into ".
	yamlUnmarshalRe = regexp.MustCompile("(?s)^(line \\d+: cannot unmarshal \\S+)(?: .*)?( into \\S+)$")
	// Ошибка тега: "yaml: cannot decode !!str `TOKEN123` as a !!int" (значение целиком, без номера строки).
	yamlResolveRe = regexp.MustCompile("(?s)^yaml: cannot decode (\\S+) .* as a (\\S+)$")
	// Ошибки сканера и парсера: постоянные тексты после номера строки, значений в них нет.
	yamlSyntaxRe = regexp.MustCompile(`^yaml: line \d+: `)
	yamlLineRe   = regexp.MustCompile(`^(?:yaml: )?line \d+: `)
)

// safeYAMLError — текст ошибки разбора locallm.yaml без значений (§5.5 п. 14 спецификации v2): у ошибки типа и
// тега значение вырезается, остаются строка, тег и ожидаемый тип. Ошибки синтаксиса и тексты без кавычек (ключ не из
// схемы — "line N: field X not found in type …", ошибки Duration) остаются как есть. Прочее — "invalid value" с
// номером строки, если он есть: лучше потерять подробность, чем показать значение.
func safeYAMLError(msg string) string {
	if m := yamlUnmarshalRe.FindStringSubmatch(msg); m != nil {
		return m[1] + m[2]
	}
	if m := yamlResolveRe.FindStringSubmatch(msg); m != nil {
		return "yaml: cannot decode a " + m[1] + " value as a " + m[2]
	}
	if yamlSyntaxRe.MatchString(msg) || !strings.ContainsAny(msg, "`'\"") {
		return msg
	}
	if line := yamlLineRe.FindString(msg); line != "" {
		return line + "invalid value"
	}
	return "invalid value"
}
