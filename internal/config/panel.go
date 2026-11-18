package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// forbiddenPanelFlags — запретный список панели (§5.6 спецификации v2, PV2) по `llama-server --help`, build 12128.
// Аргумент сравнивается целиком после normalizeFlag; короткие формы — точно. Формы, которые выключают возможность
// (--no-agent, -no-ag, --no-ui-mcp-proxy, --no-webui-mcp-proxy), разрешены. Это список запретов, а не разрешений
// (решение владельца): при обновлении llama.cpp --help перечитывается и список дополняется.
var forbiddenPanelFlags = []string{
	// Пишут файлы.
	"--log-file", "--slot-save-path", "--log-prompts-dir", "-lcd", "--lookup-cache-dynamic",
	// Раздают файлы, исполняют инструменты.
	"--path", "--media-path", "--tools", "--tools-runtime", "-ag", "--agent", "--mcp-servers-config", "--mcp-servers-json",
	"--ui-mcp-proxy", "--webui-mcp-proxy", "--video-ffmpeg-dir", "--ui-config-file", "--webui-config-file",
	// Сеть и скачивание.
	"-dr", "--docker-repo", "-hff", "--hf-file", "-hft", "--hf-token", "--spec-draft-hf", "-hfd", "-hfrd", "--hf-repo-draft",
	"-mmu", "--mmproj-url", "--rpc", "--connect", "--connect-code", "--embd-gemma-default", "--fim-qwen-1.5b-default",
	"--fim-qwen-3b-default", "--fim-qwen-7b-default", "--fim-qwen-7b-spec", "--fim-qwen-14b-spec", "--fim-qwen-30b-default",
	"--gpt-oss-20b-default", "--gpt-oss-120b-default", "--vision-gemma-4b-default", "--vision-gemma-12b-default",
	// Конфликт с хостом.
	"--ssl-key-file", "--ssl-cert-file", "--api-prefix", "--reuse-port", "--sleep-idle-seconds",
	// Сразу завершают процесс.
	"-h", "--help", "--usage", "--version", "--list-devices", "--completion-bash", "-cl", "--cache-list",
	// Сложный формат значения: путь внутри FNAME:SCALE,... панель не проверяет.
	"--lora-scaled", "--control-vector-scaled",
}

var forbiddenPanel = func() map[string]bool {
	m := make(map[string]bool, len(forbiddenPanelFlags))
	for _, f := range forbiddenPanelFlags {
		m[f] = true
	}
	return m
}()

// Тексты CheckPanel (без значений, §5.5 п. 14) и причины HandOnly.Reasons.
const (
	msgPanelControl  = "control characters and empty values are not allowed from the panel"
	msgPanelEnv      = "environment variable references are not allowed from the panel (write %% for a literal %)" // не формат: в Sprintf не передаётся
	msgPanelPlain    = `must be a plain absolute path (no \\?\ or \\.\ prefix, no ':' after the drive letter)`
	msgPanelOutside  = "file is outside models_dirs"
	msgPanelResolve  = "cannot resolve the file path"
	msgPanelShardDir = "all shards must be in the directory of the first shard"

	reasonControl  = "control character or empty value"
	reasonEnv      = "%VAR%"
	reasonPlain    = "not a plain absolute path"
	reasonOutside  = "outside models_dirs"
	reasonResolve  = "cannot resolve the path"
	reasonShardDir = "shards in different directories"
)

// panelIssue — нарушение PV1–PV7: номер элемента списка (у пути — 0), причина для HandOnly и текст для FieldError.
type panelIssue struct {
	k       int
	reason  string
	message string
}

// canonicalPath — Canonical; тесты подменяют, чтобы считать обращения к диску.
var canonicalPath = Canonical

// onRootVolume — том пути p (буква диска или \\сервер\ресурс) совпадает с томом одного из корней, без учёта регистра.
// Только текст: путь панели на чужом томе отвергается до Canonical, который открыл бы файл на чужом сервере
// (F4-D2 ledger первого плана). Пустой том (относительный путь) — false.
func onRootVolume(roots []string, p string) bool {
	v := filepath.VolumeName(strings.ReplaceAll(p, "/", `\`))
	if v == "" {
		return false
	}
	for _, r := range roots {
		if strings.EqualFold(filepath.VolumeName(r), v) {
			return true
		}
	}
	return false
}

// checkPanelPath — путь модели по PV6, PV4, PV7 и PV1 (вместе с шардами). nil — правила выполнены. Путь на чужом томе
// отвергается без обращения к диску; шарды проверяются по одному до первого, который не разрешается.
func checkPanelPath(roots []string, raw string) *panelIssue {
	if hasControlOrEmpty(raw) {
		return &panelIssue{reason: reasonControl, message: msgPanelControl}
	}
	if envRef(raw) {
		return &panelIssue{reason: reasonEnv, message: msgPanelEnv}
	}
	p := strings.ReplaceAll(raw, "%%", "%") // так значение раскроет expand: других '%' после PV4 нет
	if !plainAbsPath(p) {
		return &panelIssue{reason: reasonPlain, message: msgPanelPlain}
	}
	if !onRootVolume(roots, p) {
		return &panelIssue{reason: reasonOutside, message: msgPanelOutside}
	}
	firstDir := ""
	for i, s := range shards(p) {
		canon, err := canonicalPath(s)
		if err != nil {
			return &panelIssue{reason: reasonResolve, message: msgPanelResolve}
		}
		if !insideRoots(roots, canon) {
			return &panelIssue{reason: reasonOutside, message: msgPanelOutside}
		}
		if i == 1 {
			firstDir = filepath.Dir(canon)
		} else if !strings.EqualFold(filepath.Dir(canon), firstDir) {
			return &panelIssue{reason: reasonShardDir, message: msgPanelShardDir}
		}
	}
	return nil
}

// checkPanelArgs — список аргументов по PV6, PV4 и PV2 (каждый элемент), затем PV7 и PV3 (значения файловых флагов,
// каждая часть --lora и --control-vector). Значение с ошибкой PV6 или PV4 как путь не проверяется.
func checkPanelArgs(roots []string, args []string) []panelIssue {
	var out []panelIssue
	bad := map[int]bool{}
	for k, a := range args {
		switch {
		case hasControlOrEmpty(a):
			out = append(out, panelIssue{k, reasonControl, msgPanelControl})
			bad[k] = true
		case envRef(a):
			out = append(out, panelIssue{k, reasonEnv, msgPanelEnv})
			bad[k] = true
		}
		if f := normalizeFlag(a); forbiddenPanel[f] {
			out = append(out, panelIssue{k, "forbidden flag " + f, "flag " + f + " cannot be set from the panel; edit models.yaml by hand"})
		}
	}
	refs, _ := fileArgRefs(args) // флаг без значения отвергает checkArgs
	for _, r := range refs {
		if bad[r.Index] {
			continue
		}
		p := strings.ReplaceAll(r.Value, "%%", "%")
		if !plainAbsPath(p) {
			out = append(out, panelIssue{r.Index, reasonPlain, r.Flag + ": value " + msgPanelPlain})
			continue
		}
		if !onRootVolume(roots, p) {
			out = append(out, panelIssue{r.Index, reasonOutside, r.Flag + ": " + msgPanelOutside})
			continue
		}
		canon, err := canonicalPath(p)
		if err != nil {
			out = append(out, panelIssue{r.Index, reasonResolve, r.Flag + ": " + msgPanelResolve})
		} else if !insideRoots(roots, canon) {
			out = append(out, panelIssue{r.Index, reasonOutside, r.Flag + ": " + msgPanelOutside})
		}
	}
	return out
}

// CheckPanel — правила PV1–PV7 для документа из панели (§5.6 спецификации v2) с исключением PV8 по опорному документу
// ref (nil — исключений нет). Дополняет WithModels: правила MVP, §5.5 и PV5 проверяет она. Корни — c.ModelsDirs.
// Поля — models[i].path, models[i].args[k], models[i].profiles[j].args[k]; тексты без значений.
func CheckPanel(c *Config, ref, doc *ModelsDoc) []FieldError {
	if doc == nil {
		return nil
	}
	paths, lists := refValues(ref)
	var errs []FieldError
	for i, m := range doc.Models {
		// PV8 нужен, чтобы ручные значения переживали сохранение из панели: рукописный путь не проверяется по PV1, PV4,
		// PV6 и PV7. Исключение только для точного совпадения с опорным (рукописным) документом; любое новое значение
		// из панели по-прежнему проходит PV6.
		if !paths[m.Path] {
			if is := checkPanelPath(c.ModelsDirs, m.Path); is != nil {
				errs = append(errs, FieldError{fmt.Sprintf("models[%d].path", i), is.message})
			}
		}
		if !listIn(m.Args, lists) {
			for _, is := range checkPanelArgs(c.ModelsDirs, m.Args) {
				errs = append(errs, FieldError{fmt.Sprintf("models[%d].args[%d]", i, is.k), is.message})
			}
		}
		for j, p := range m.Profiles {
			if listIn(p.Args, lists) {
				continue
			}
			for _, is := range checkPanelArgs(c.ModelsDirs, p.Args) {
				errs = append(errs, FieldError{fmt.Sprintf("models[%d].profiles[%d].args[%d]", i, j, is.k), is.message})
			}
		}
	}
	return errs
}

// HandOnly — какие значения модели не прошли бы PV1–PV7 без исключения PV8 (§5.6): GET /api/admin/models отдаёт их
// как path_hand_only, args_hand_only, hand_only у профиля и причины.
type HandOnly struct {
	Path     bool
	Args     bool
	Profiles []bool   // по профилям документа, в их порядке
	Reasons  []string // без повторов, в порядке появления; nil — причин нет
}

// HandOnlyMarks — метки для каждой модели документа (корни — c.ModelsDirs).
func HandOnlyMarks(c *Config, doc *ModelsDoc) []HandOnly {
	if doc == nil {
		return nil
	}
	out := make([]HandOnly, len(doc.Models))
	for i, m := range doc.Models {
		h := &out[i]
		h.Profiles = make([]bool, len(m.Profiles))
		add := func(reason string) {
			if !slices.Contains(h.Reasons, reason) {
				h.Reasons = append(h.Reasons, reason)
			}
		}
		if is := checkPanelPath(c.ModelsDirs, m.Path); is != nil {
			h.Path = true
			add(is.reason)
		}
		for _, is := range checkPanelArgs(c.ModelsDirs, m.Args) {
			h.Args = true
			add(is.reason)
		}
		for j, p := range m.Profiles {
			for _, is := range checkPanelArgs(c.ModelsDirs, p.Args) {
				h.Profiles[j] = true
				add(is.reason)
			}
		}
	}
	return out
}

// refValues — пути моделей и все списки аргументов (моделей и профилей) опорного документа (PV8).
func refValues(ref *ModelsDoc) (paths map[string]bool, lists [][]string) {
	paths = map[string]bool{}
	if ref == nil {
		return paths, nil
	}
	for _, m := range ref.Models {
		paths[m.Path] = true
		lists = append(lists, m.Args)
		for _, p := range m.Profiles {
			lists = append(lists, p.Args)
		}
	}
	return paths, lists
}

// listIn — равен ли список поэлементно одному из lists (PV8); nil и пустой список равны.
func listIn(args []string, lists [][]string) bool {
	for _, l := range lists {
		if slices.Equal(args, l) {
			return true
		}
	}
	return false
}

// hasControlOrEmpty — PV6: пустая строка или символ U+0000–U+001F, U+007F (в UTF-8 это одиночные байты).
func hasControlOrEmpty(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// envRef — PV4: в строке есть '%' вне пары "%%" (пары считаются слева направо, как в expand).
func envRef(s string) bool {
	return strings.Contains(strings.ReplaceAll(s, "%%", ""), "%")
}

// plainAbsPath — PV7: абсолютный путь без префиксов \\?\, \\.\ и \??\ (с любыми разделителями) и без ':' нигде,
// кроме буквы диска (альтернативные потоки NTFS).
func plainAbsPath(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	q := strings.ReplaceAll(p, "/", `\`)
	for _, prefix := range []string{`\\?\`, `\\.\`, `\??\`} {
		if strings.HasPrefix(q, prefix) {
			return false
		}
	}
	i := strings.LastIndexByte(p, ':')
	return i < 0 || i == 1
}

// within — лежит ли канонический путь p внутри канонического корня root: без учёта регистра и только по границе
// разделителя (C:\models2\x.gguf не внутри C:\models). Если свёртка регистра меняет длину в байтах, ответ «нет» —
// безопасная сторона.
func within(root, p string) bool {
	if len(p) <= len(root) || !strings.EqualFold(p[:len(root)], root) {
		return false
	}
	return strings.HasSuffix(root, `\`) || p[len(root)] == '\\'
}

func insideRoots(roots []string, p string) bool {
	for _, r := range roots {
		if within(r, p) {
			return true
		}
	}
	return false
}
