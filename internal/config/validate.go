package config

import (
	"fmt"
	"iter"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	shardRe   = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	// Имя DNS в server.admin_hosts (§5.2 спецификации v2): буквы, цифры, '-', '.'; проверяется в нижнем регистре.
	dnsNameRe = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*$`)
	// Флаги, которыми управляет хост или которые конфликтуют с path (§5.3 п. 6), с синонимами из
	// llama-server --help (build 12128); плюс префиксы --models- и --no-models-. Сравниваются после normalizeFlag.
	reservedFlags = []string{"-m", "--model", "--host", "--port", "--api-key", "--api-key-file", "-hf", "-hfr", "--hf-repo", "-mu", "--model-url"}
	// Флаги, после которых идёт путь к файлу (§5.3 п. 5 MVP и §5.5 п. 11 спецификации v2), с синонимами из
	// llama-server --help (build 12128); сравниваются после normalizeFlag.
	fileArgFlags = map[string]bool{
		"--mmproj": true, "-mm": true, "--model-draft": true, "-md": true, "--spec-draft-model": true, "--lora": true,
		"--chat-template-file": true, "--control-vector": true, "--grammar-file": true, "-jf": true,
		"--json-schema-file": true, "-lcs": true, "--lookup-cache-static": true,
	}
	// Файловые флаги со списком через запятую (§5.5 п. 11 спецификации v2): каждая часть — отдельный путь.
	listFileArgFlags = map[string]bool{"--lora": true, "--control-vector": true}
)

// Пределы списков locallm.yaml (§5.2 спецификации v2).
const (
	maxAdminHosts = 16
	maxModelsDirs = 16
)

func validate(c *Config) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	listenPort := 0
	if host, port, err := net.SplitHostPort(c.Server.Listen); err != nil {
		add("server.listen: %q is not host:port", c.Server.Listen)
	} else {
		if ip := net.ParseIP(host); ip == nil {
			add("server.listen: host %q must be an IP literal in 127.0.0.0/8 or ::1", host)
		} else if !ip.IsLoopback() {
			add("server.listen: host %q is not a loopback address (only 127.0.0.0/8 or ::1 allowed)", host)
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			add("server.listen: port %q must be 1..65535", port)
		} else {
			listenPort = p
		}
	}
	if !filepath.IsAbs(c.Server.APIKeysFile) {
		add("server.api_keys_file: %q must be an absolute path", c.Server.APIKeysFile)
	}
	if !filepath.IsAbs(c.Server.AdminKeyFile) {
		add("server.admin_key_file: %q must be an absolute path", c.Server.AdminKeyFile)
	} else if filepath.IsAbs(c.Server.APIKeysFile) && sameFile(c.Server.AdminKeyFile, c.Server.APIKeysFile) {
		add("server.admin_key_file: must differ from server.api_keys_file")
	}
	if len(c.Server.AdminHosts) > maxAdminHosts {
		add("server.admin_hosts: at most %d entries", maxAdminHosts)
	}
	for i, h := range c.Server.AdminHosts {
		n, ok := normalizeAdminHost(h)
		if !ok {
			add("server.admin_hosts[%d]: %q is not a DNS name or an IP literal (no port)", i, h)
			continue
		}
		c.Server.AdminHosts[i] = n
	}
	if !filepath.IsAbs(c.Server.StateFile) {
		add("server.state_file: %q must be an absolute path", c.Server.StateFile)
	}

	if c.Llama.ServerExe == "" {
		add("llama.server_exe: required")
	} else {
		checkFile(&errs, "llama.server_exe", c.Llama.ServerExe)
	}
	if c.Llama.Port < 1024 || c.Llama.Port > 65535 {
		add("llama.port: %d is out of range 1024..65535", c.Llama.Port)
	} else if c.Llama.Port == listenPort {
		add("llama.port: %d must differ from the server.listen port", c.Llama.Port)
	}
	checkArgs(&errs, "llama.default_args", c.Llama.DefaultArgs)
	durRange(&errs, "llama.load_timeout", c.Llama.LoadTimeout, 10*time.Second, 0)
	durRange(&errs, "llama.stop_timeout", c.Llama.StopTimeout, time.Second, 60*time.Second)
	intRange(&errs, "llama.log_lines", int64(c.Llama.LogLines), 100, 100000)

	durRange(&errs, "swap.swap_cooldown", c.Swap.SwapCooldown, 0, 0)
	durRange(&errs, "swap.wait_timeout", c.Swap.WaitTimeout, 10*time.Second, 590*time.Second)
	durRange(&errs, "swap.vram_free_timeout", c.Swap.VRAMFreeTimeout, time.Second, 120*time.Second)
	intRange(&errs, "swap.vram_margin_mib", c.Swap.VRAMMarginMiB, 0, 4096)
	intRange(&errs, "swap.max_queue", int64(c.Swap.MaxQueue), 1, 64)
	durRange(&errs, "swap.failed_ttl", c.Swap.FailedTTL, 0, 0)

	switch c.Vram.Source {
	case "auto", "pdh", "nvidia-smi", "none":
	default:
		add("vram.source: %q must be one of auto, pdh, nvidia-smi, none", c.Vram.Source)
	}
	if !filepath.IsAbs(c.Logging.Dir) {
		add("logging.dir: %q must be an absolute path", c.Logging.Dir)
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level: %q must be one of debug, info, warn, error", c.Logging.Level)
	}
	intRange(&errs, "logging.max_size_mib", int64(c.Logging.MaxSizeMiB), 1, 1024)
	intRange(&errs, "logging.max_files", int64(c.Logging.MaxFiles), 1, 100)

	if !filepath.IsAbs(c.ModelsFile) {
		add("models_file: %q must be an absolute path", c.ModelsFile)
	}
	c.ModelsDirs = checkModelsDirs(&errs, &c.Warnings, c.ModelsDirs)

	return errs
}

// checkFile: абсолютный путь к существующему файлу. true — проверка пройдена.
func checkFile(errs *[]error, field, p string) bool {
	if !filepath.IsAbs(p) {
		*errs = append(*errs, FieldError{field, fmt.Sprintf(`%q must be an absolute path (C:\... or \\server\share\...)`, p)})
		return false
	}
	st, err := os.Stat(p)
	if err != nil {
		*errs = append(*errs, FieldError{field, fmt.Sprintf("file %q does not exist", p)})
		return false
	}
	if st.IsDir() {
		*errs = append(*errs, FieldError{field, fmt.Sprintf("%q is a directory, not a file", p)})
		return false
	}
	return true
}

// checkModelPath — путь модели (MVP §5.3 п. 3–4): абсолютный путь к существующему файлу .gguf, все шарды на месте.
// valueFree — тексты без пути (стиль API, FR1-D1 ledger первого плана): их видит клиент /api/admin/*, а путь после
// раскрытия %VAR% может содержать секрет. Иначе — тексты MVP с путём (их видит только владелец при старте).
func checkModelPath(errs *[]error, field, p string, valueFree bool) {
	if valueFree {
		if !checkFileValueFree(errs, field, p) {
			return
		}
	} else if !checkFile(errs, field, p) {
		return
	}
	if !strings.EqualFold(filepath.Ext(p), ".gguf") {
		msg := fmt.Sprintf("%q must have extension .gguf", p)
		if valueFree {
			msg = "must have extension .gguf"
		}
		*errs = append(*errs, FieldError{field, msg})
	}
	checkShards(errs, field, p, valueFree)
}

// checkFileValueFree — правила checkFile с текстами без пути (стиль API).
func checkFileValueFree(errs *[]error, field, p string) bool {
	fail := func(msg string) bool {
		*errs = append(*errs, FieldError{field, msg})
		return false
	}
	if !filepath.IsAbs(p) {
		return fail(`must be an absolute path (C:\... or \\server\share\...)`)
	}
	st, err := os.Stat(p)
	if err != nil {
		return fail("file does not exist")
	}
	if st.IsDir() {
		return fail("is a directory, not a file")
	}
	return true
}

// checkModelsDirs — корни models_dirs (§5.2 спецификации v2): абсолютный путь; существует, но не каталог — ошибка;
// не существует или недоступен — предупреждение, каталог пропускается. Возвращает канонические пути (PV1).
func checkModelsDirs(errs *[]error, warnings *[]string, dirs []string) []string {
	if len(dirs) > maxModelsDirs {
		*errs = append(*errs, fmt.Errorf("models_dirs: at most %d entries", maxModelsDirs))
	}
	var out []string
	for i, d := range dirs {
		if !filepath.IsAbs(d) {
			*errs = append(*errs, fmt.Errorf("models_dirs[%d]: %q must be an absolute path", i, d))
			continue
		}
		st, err := os.Stat(d)
		if err == nil && !st.IsDir() {
			*errs = append(*errs, fmt.Errorf("models_dirs[%d]: %q is not a directory", i, d))
			continue
		}
		var canon string
		if err == nil {
			canon, err = Canonical(d)
		}
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("models_dirs[%d]: directory %q does not exist or is not available; skipped", i, d))
			continue
		}
		out = append(out, canon)
	}
	return out
}

// normalizeAdminHost — значение server.admin_hosts в виде для сравнения с Host (§5.2, §9.1 спецификации v2):
// нижний регистр; IP — в канонической записи netip (IPv6 без скобок и в сокращённой форме, IPv4 из ::ffff:a.b.c.d —
// как IPv4; F2-D1 ledger первого плана). ok == false — не имя DNS и не IP-литерал без порта и без зоны.
func normalizeAdminHost(s string) (string, bool) {
	h := strings.ToLower(s)
	if inner, ok := strings.CutPrefix(h, "["); ok {
		inner, ok = strings.CutSuffix(inner, "]")
		if a, err := netip.ParseAddr(inner); ok && err == nil && a.Is6() && !a.Is4In6() && a.Zone() == "" {
			return a.String(), true
		}
		return "", false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", false
		}
		return a.Unmap().String(), true
	}
	if len(h) <= 253 && dnsNameRe.MatchString(h) {
		return h, true
	}
	return "", false
}

// NormalizeHost — правило normalizeAdminHost для проверки заголовков Host и Origin в шлюзе (§9.1 спецификации v2):
// значения сравниваются в той же записи, в какой хранится server.admin_hosts.
func NormalizeHost(s string) (string, bool) { return normalizeAdminHost(s) }

// distinctFiles — файлы, которые хост перезаписывает (state.json, models.yaml), не совпадают с конфигом и файлами
// ключей (F2-D2, F5-D2 ledger первого плана): иначе запись паузы или моделей заменила бы конфиг или ключи.
// Сравниваются только абсолютные пути (ошибку относительного пути даёт validate); sameFile ловит другое написание,
// имена 8.3 и жёсткие ссылки.
func distinctFiles(abs string, c *Config) []error {
	var errs []error
	check := func(field, p, other, otherName string) {
		if filepath.IsAbs(p) && filepath.IsAbs(other) && sameFile(p, other) {
			errs = append(errs, fmt.Errorf("%s: must differ from %s", field, otherName))
		}
	}
	check("server.state_file", c.Server.StateFile, abs, "the config file")
	check("server.state_file", c.Server.StateFile, c.ModelsFile, "models_file")
	check("server.state_file", c.Server.StateFile, c.Server.APIKeysFile, "server.api_keys_file")
	check("server.state_file", c.Server.StateFile, c.Server.AdminKeyFile, "server.admin_key_file")
	check("models_file", c.ModelsFile, c.Server.APIKeysFile, "server.api_keys_file")
	check("models_file", c.ModelsFile, c.Server.AdminKeyFile, "server.admin_key_file")
	return errs
}

// sameFile — один ли это файл: пути равны без учёта регистра после Clean, либо os.SameFile (имена 8.3, жёсткие ссылки).
func sameFile(a, b string) bool {
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// normalizeFlag — аргумент с именем флага в том виде, в каком его ищет llama.cpp (common/arg.cpp): у аргумента
// с префиксом "--" каждый '_' в имени (до первого '=') заменяется на '-' ("--ctx_size" -> "--ctx-size";
// проверено на llama-server build 12128, F2-D1). Значение после '=' не меняется; аргументы без "--" — как есть.
func normalizeFlag(a string) string {
	if !strings.HasPrefix(a, "--") {
		return a
	}
	name, value, hasValue := strings.Cut(a, "=")
	name = strings.ReplaceAll(name, "_", "-")
	if !hasValue {
		return name
	}
	return name + "=" + value
}

// fileArgRef — значение файлового аргумента (§5.3 п. 5 MVP, §5.5 п. 11 спецификации v2).
type fileArgRef struct {
	Index int    // номер элемента списка, в котором стоит значение
	Flag  string // флаг в каноническом написании (normalizeFlag)
	Value string // путь; у --lora и --control-vector — одна часть значения
}

// fileArgRefs — файловые значения списка args: элемент после флага или часть после '=' (форму --flag=value отвергает
// checkArgs, но путь у неё тоже проверяется). Значение --lora и --control-vector делится по запятым. missing — номера
// файловых флагов без значения (флаг последний в списке). Как в MVP, элемент-значение проверяется и как флаг.
func fileArgRefs(args []string) (refs []fileArgRef, missing []int) {
	for i, raw := range args {
		a := normalizeFlag(raw)
		var flag, value string
		idx := i
		if j := strings.IndexByte(a, '='); j > 0 && fileArgFlags[a[:j]] {
			flag, value = a[:j], a[j+1:]
		} else if fileArgFlags[a] {
			if i+1 >= len(args) {
				missing = append(missing, i)
				continue
			}
			flag, value, idx = a, args[i+1], i+1
		} else {
			continue
		}
		parts := []string{value}
		if listFileArgFlags[flag] {
			parts = strings.Split(value, ",")
		}
		for _, p := range parts {
			refs = append(refs, fileArgRef{Index: idx, Flag: flag, Value: p})
		}
	}
	return refs, missing
}

func checkArgs(errs *[]error, field string, args []string) {
	for i, raw := range args {
		at := fmt.Sprintf("%s[%d]", field, i)
		// Форма --flag=value запрещена: отвергается любой аргумент, который начинается с '-' и содержит '='
		// (решения владельца 2026-11-09, планы reject-equals-form и strict-equals-form; то же правило — PV5
		// спецификации v2). llama-server build 12128 такой аргумент не принимает и печатает его в ошибке, у аргумента
		// с "--" заменив '_' на '-' во всей строке, поэтому маскирование вывода значение не узнаёт. Значения с '=',
		// которые не начинаются с '-' (key=type:value после --override-kv), проходят. В ошибке только поле и номер
		// аргумента с нуля, как в ошибках раскрытия %VAR%: и в значении, и в имени флага может оказаться секрет из %VAR%.
		if strings.HasPrefix(raw, "-") && strings.Contains(raw, "=") {
			*errs = append(*errs, FieldError{at, "the --flag=value form is not allowed; put the value in the next list item"})
		}
		a := normalizeFlag(raw)
		for _, f := range reservedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				*errs = append(*errs, FieldError{at, fmt.Sprintf("flag %q is reserved for the host", f)})
			}
		}
		if strings.HasPrefix(a, "--models-") || strings.HasPrefix(a, "--no-models-") {
			*errs = append(*errs, FieldError{at, "router-mode flag --models-* is not allowed"})
		}
	}
	refs, missing := fileArgRefs(args)
	for _, i := range missing {
		*errs = append(*errs, FieldError{fmt.Sprintf("%s[%d]", field, i), "flag " + normalizeFlag(args[i]) + " needs a file path"})
	}
	for _, r := range refs {
		checkFileArg(errs, fmt.Sprintf("%s[%d]", field, r.Index), r.Flag, r.Value)
	}
}

// checkFileArg — те же правила, что checkFile, для значения файлового аргумента
// (--mmproj, --lora, ...): значение может быть секретом и в ошибки не попадает.
func checkFileArg(errs *[]error, field, flag, p string) {
	fail := func(msg string) { *errs = append(*errs, FieldError{field, flag + ": " + msg}) }
	if !filepath.IsAbs(p) {
		fail("value must be an absolute path")
		return
	}
	st, err := os.Stat(p)
	if err != nil {
		fail("file does not exist")
		return
	}
	if st.IsDir() {
		fail("value is a directory, not a file")
	}
}

// shards — файлы модели по порядку: (1, p), а если p — первый шард (-00001-of-N.gguf), то (2..N, путь шарда).
// Пути строятся по одному на шаг, перебор можно прервать (F4-D1, FR1-N2 ledger первого плана). Общая генерация
// имён для checkShards и CheckPanel.
func shards(p string) iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		if !yield(1, p) {
			return
		}
		mm := shardRe.FindStringSubmatch(filepath.Base(p))
		if mm == nil || mm[1] != "00001" {
			return
		}
		total, _ := strconv.Atoi(mm[2])
		prefix, ext := p[:len(p)-len(mm[0])], p[len(p)-len(".gguf"):]
		for i := 2; i <= total; i++ {
			if !yield(i, fmt.Sprintf("%s-%05d-of-%s%s", prefix, i, mm[2], ext)) {
				return
			}
		}
	}
}

// checkShards — путь составной модели указывает на первый шард, и все шарды на месте (MVP §5.3 п. 4). valueFree —
// тексты без путей, перебор останавливается на первом пропуске; иначе — тексты MVP, каждый пропуск с путём.
func checkShards(errs *[]error, field, p string, valueFree bool) {
	mm := shardRe.FindStringSubmatch(filepath.Base(p))
	if mm == nil {
		return
	}
	if mm[1] != "00001" {
		msg := fmt.Sprintf("shard %s is not the first shard (point path at ...-00001-of-%s.gguf)", mm[1], mm[2])
		if valueFree {
			msg = "is not the first shard (point path at the -00001-of-N.gguf file)"
		}
		*errs = append(*errs, FieldError{field, msg})
		return
	}
	for i, sp := range shards(p) {
		if _, err := os.Stat(sp); err != nil {
			if valueFree {
				*errs = append(*errs, FieldError{field, fmt.Sprintf("shard %d is missing", i)})
				return
			}
			*errs = append(*errs, FieldError{field, fmt.Sprintf("shard %q is missing", sp)})
		}
	}
}

// contextLength — значение последнего -c / --ctx-size (в том числе --ctx_size, как его читает llama.cpp)
// в default_args + args (§5.3 п. 7, F2-D1); 0 — неизвестен. Форму --ctx-size=N llama-server не принимает, она не читается.
func contextLength(def, args []string) (int, error) {
	all := append(append([]string{}, def...), args...)
	n := 0
	for i := 0; i < len(all); i++ {
		flag := normalizeFlag(all[i])
		if flag != "-c" && flag != "--ctx-size" {
			continue
		}
		if i+1 >= len(all) {
			return 0, fmt.Errorf("flag %s needs a value", flag)
		}
		v, err := strconv.Atoi(all[i+1])
		if err != nil || v < 0 {
			return 0, fmt.Errorf("flag %s: value is not a non-negative integer", flag)
		}
		n = v
		i++
	}
	return n, nil
}

// durRange: max == 0 — без верхней границы.
func durRange(errs *[]error, field string, v, min, max time.Duration) {
	if v < min || (max > 0 && v > max) {
		hi := "inf"
		if max > 0 {
			hi = max.String()
		}
		*errs = append(*errs, fmt.Errorf("%s: %s is out of range %s..%s", field, v, min, hi))
	}
}

func intRange(errs *[]error, field string, v, min, max int64) {
	if v < min || v > max {
		*errs = append(*errs, fmt.Errorf("%s: %d is out of range %d..%d", field, v, min, max))
	}
}
