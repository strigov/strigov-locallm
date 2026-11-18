package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// Режимы моделей (§5.1 спецификации v2).
const (
	ModeFile   = "file"   // в locallm.yaml нет ключа models: модели читаются из models_file
	ModeLegacy = "legacy" // модели в locallm.yaml, как в MVP
)

// MaxModelsFileBytes — предел размера models.yaml (§5.5 п. 10 спецификации v2).
const MaxModelsFileBytes = 1 << 20

// Остальные пределы §5.5 п. 10.
const (
	maxModels      = 256
	maxProfiles    = 32
	maxClientNames = 512
	maxArgs        = 256
	maxArgBytes    = 4 << 10
)

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ModelsDoc — документ моделей (§4.4 спецификации v2): models.yaml, models: в locallm.yaml (legacy) или тело
// PUT /api/admin/models. Строки — до раскрытия %VAR%. В документе, прочитанном из YAML, пустые списки — nil.
type ModelsDoc struct {
	Models []DocModel `json:"models"`
}

type DocModel struct {
	ID       string       `json:"id"`
	Path     string       `json:"path"`
	Args     []string     `json:"args"`
	Profiles []DocProfile `json:"profiles"`
}

type DocProfile struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

// clone — глубокая копия документа; nil-списки остаются nil.
func (d *ModelsDoc) clone() *ModelsDoc {
	if d == nil {
		return nil
	}
	out := &ModelsDoc{Models: slices.Clone(d.Models)}
	for i := range out.Models {
		m := &out.Models[i]
		m.Args = slices.Clone(m.Args)
		m.Profiles = slices.Clone(m.Profiles)
		for j := range m.Profiles {
			m.Profiles[j].Args = slices.Clone(m.Profiles[j].Args)
		}
	}
	return out
}

// ParseModels разбирает models.yaml (§5.3): обязательный version: 1; без models каталог пуст. При ошибках документ nil.
func ParseModels(data []byte) (*ModelsDoc, []error) {
	if len(data) > MaxModelsFileBytes {
		return nil, []error{fmt.Errorf("file is larger than %d bytes", MaxModelsFileBytes)}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, []error{fmt.Errorf("yaml: %s", safeYAMLError(err.Error()))}
	}
	if root.Kind == 0 || len(root.Content) == 0 { // пустой файл или только комментарии
		return nil, []error{errors.New("version: required (write version: 1)")}
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, []error{fmt.Errorf("line %d: must be a mapping with version and models", top.Line)}
	}
	var errs []error
	var version, models *yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(top.Content); i += 2 {
		k, v := top.Content[i], top.Content[i+1]
		if seen[k.Value] {
			errs = append(errs, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value))
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "version":
			version = v
		case "models":
			models = v
		default:
			errs = append(errs, fmt.Errorf("line %d: unknown key %q", k.Line, k.Value))
		}
	}
	if version == nil {
		errs = append(errs, errors.New("version: required (write version: 1)"))
	} else if version.Kind != yaml.ScalarNode || version.ShortTag() != "!!int" || version.Value != "1" {
		errs = append(errs, fmt.Errorf("version (line %d): must be 1", version.Line))
	}
	doc := &ModelsDoc{}
	if models != nil {
		var merrs []error
		doc, merrs = decodeModelsNode(models)
		errs = append(errs, merrs...)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return doc, nil
}

// decodeModelsNode разбирает узел models (§5.3): один декодер для models.yaml и для models: в locallm.yaml (legacy).
// Порядок файла сохраняется; повторы и неизвестные ключи — ошибки (yaml.v3 не проверяет повторы в yaml.Node).
// Отсутствующий узел и null — пустой документ. При ошибках документ nil.
func decodeModelsNode(n *yaml.Node) (*ModelsDoc, []error) {
	doc := &ModelsDoc{}
	if n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") {
		return doc, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, []error{fmt.Errorf("models (line %d): must be a mapping of model id to settings", n.Line)}
	}
	var errs []error
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		id := k.Value
		if seen[id] {
			errs = append(errs, fmt.Errorf("models.%s (line %d): duplicate model id", id, k.Line))
			continue
		}
		seen[id] = true
		m := DocModel{ID: id}
		switch {
		case v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null":
		case v.Kind != yaml.MappingNode:
			errs = append(errs, fmt.Errorf("models.%s (line %d): must be a mapping with path, args and profiles", id, v.Line))
		default:
			keys := map[string]bool{}
			for j := 0; j+1 < len(v.Content); j += 2 {
				fk, fv := v.Content[j], v.Content[j+1]
				if keys[fk.Value] {
					errs = append(errs, fmt.Errorf("models.%s (line %d): duplicate key %q", id, fk.Line, fk.Value))
					continue
				}
				keys[fk.Value] = true
				switch fk.Value {
				case "path":
					if err := fv.Decode(&m.Path); err != nil { // текст err не берётся: см. decodeArgs
						errs = append(errs, fmt.Errorf("models.%s.path (line %d): must be a string", id, fv.Line))
					}
				case "args":
					m.Args = decodeArgs(&errs, "models."+id+".args", fv)
				case "profiles":
					m.Profiles = decodeProfiles(&errs, id, fv)
				default:
					errs = append(errs, fmt.Errorf("models.%s (line %d): unknown key %q", id, fk.Line, fk.Value))
				}
			}
		}
		doc.Models = append(doc.Models, m)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return doc, nil
}

// decodeArgs — список строк; пустой список — nil. Текст ошибки yaml.v3 в сообщение не берётся: в ошибки типа
// («cannot unmarshal !!str `TOKEN123` into []string») и тега («cannot decode !!str `TOKEN123` as a !!int») v3.0.1
// вставляет само значение (§5.5 п. 14 спецификации v2). Вместо него — поле, строка и ожидаемый тип.
func decodeArgs(errs *[]error, field string, n *yaml.Node) []string {
	var a []string
	if err := n.Decode(&a); err != nil {
		*errs = append(*errs, fmt.Errorf("%s (line %d): must be a list of strings", field, n.Line))
		return nil
	}
	if len(a) == 0 {
		return nil
	}
	return a
}

// decodeProfiles — profiles: имя → список аргументов (§5.4) в порядке файла; пустая карта и null — nil.
func decodeProfiles(errs *[]error, id string, n *yaml.Node) []DocProfile {
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		*errs = append(*errs, fmt.Errorf("models.%s.profiles (line %d): must be a mapping of profile name to an argument list", id, n.Line))
		return nil
	}
	var out []DocProfile
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if seen[k.Value] {
			*errs = append(*errs, fmt.Errorf("models.%s.profiles.%s (line %d): duplicate profile name", id, k.Value, k.Line))
			continue
		}
		seen[k.Value] = true
		out = append(out, DocProfile{Name: k.Value, Args: decodeArgs(errs, "models."+id+".profiles."+k.Value, v)})
	}
	return out
}

// fieldStyle — как называть поля в ошибках каталога и писать тексты ошибок пути.
type fieldStyle int

const (
	fileStyle    fieldStyle = iota // старт и перенос: поля по ключам файла, тексты путей MVP (с путём)
	apiStyle                       // PUT /api/admin/models: поля по индексам документа, тексты путей без значений
	fileAPIStyle                   // «Применить файл» и GET /api/admin/models: поля по ключам файла, тексты без значений
)

// valueFree — тексты ошибок пути без значений (FR1-D1 ledger первого плана): их видит клиент API.
func (s fieldStyle) valueFree() bool { return s != fileStyle }

// model — поле модели: "models.<id>" или "models[i]"; к нему добавляются ".path", ".args", ".profiles".
func (s fieldStyle) model(i int, m DocModel) string {
	if s == apiStyle {
		return fmt.Sprintf("models[%d]", i)
	}
	return "models." + m.ID
}

// id — поле имени модели: "models.<id>" или "models[i].id".
func (s fieldStyle) id(i int, m DocModel) string {
	if s == apiStyle {
		return fmt.Sprintf("models[%d].id", i)
	}
	return "models." + m.ID
}

// profileName — поле имени профиля: "models.<id>.profiles.<имя>" или "models[i].profiles[j].name".
func (s fieldStyle) profileName(i int, m DocModel, j int, p DocProfile) string {
	if s == apiStyle {
		return fmt.Sprintf("models[%d].profiles[%d].name", i, j)
	}
	return "models." + m.ID + ".profiles." + p.Name
}

// profileArgs — поле списка профиля (к нему добавляется "[k]"): "models.<id>.profiles.<имя>" или "models[i].profiles[j].args".
func (s fieldStyle) profileArgs(i int, m DocModel, j int, p DocProfile) string {
	if s == apiStyle {
		return fmt.Sprintf("models[%d].profiles[%d].args", i, j)
	}
	return "models." + m.ID + ".profiles." + p.Name
}

// catalog — развёрнутый каталог (§5.4): модели в порядке документа, у каждой базовый вариант, затем профили.
// Строки раскрываются (%VAR%), проверяются правила MVP §5.3 и §5.5 п. 9–11 спецификации v2. Ошибки — FieldError.
func catalog(doc *ModelsDoc, def []string, style fieldStyle) ([]Model, []error) {
	var errs []error
	var out []Model
	if len(doc.Models) > maxModels {
		errs = append(errs, FieldError{"models", fmt.Sprintf("at most %d models", maxModels)})
	}
	owner := map[string]string{} // имя для клиентов → поле, которое его заняло
	claim := func(name, field string) {
		if prev, ok := owner[name]; ok {
			errs = append(errs, FieldError{field, fmt.Sprintf("name %q is already used by %s", name, prev)})
			return
		}
		owner[name] = field
	}
	for i, dm := range doc.Models {
		mf := style.model(i, dm)
		idField := style.id(i, dm)
		if !modelIDRe.MatchString(dm.ID) {
			errs = append(errs, FieldError{idField, fmt.Sprintf("invalid model id (must match %s)", modelIDRe)})
		}
		claim(dm.ID, idField)
		path := ""
		if dm.Path == "" {
			errs = append(errs, FieldError{mf + ".path", "required"})
		} else {
			n := len(errs)
			path = expandField(&errs, mf+".path", dm.Path)
			if len(errs) == n {
				checkModelPath(&errs, mf+".path", path, style.valueFree())
			}
		}
		margs := checkList(&errs, mf+".args", dm.Args)
		ctx, ctxErr := contextLength(def, margs)
		if ctxErr != nil {
			errs = append(errs, FieldError{mf, ctxErr.Error()})
		}
		out = append(out, Model{ID: dm.ID, Base: dm.ID, Path: path, Args: append([]string{}, margs...), ContextLength: ctx})
		if len(dm.Profiles) > maxProfiles {
			errs = append(errs, FieldError{mf + ".profiles", fmt.Sprintf("at most %d profiles", maxProfiles)})
		}
		for j, dp := range dm.Profiles {
			nameField := style.profileName(i, dm, j, dp)
			name := dm.ID + "-" + dp.Name
			if !profileNameRe.MatchString(dp.Name) {
				errs = append(errs, FieldError{nameField, fmt.Sprintf("invalid profile name (must match %s)", profileNameRe)})
			} else if len(name) > 64 {
				errs = append(errs, FieldError{nameField, fmt.Sprintf("client name %q is longer than 64 characters", name)})
			}
			claim(name, nameField)
			argsField := style.profileArgs(i, dm, j, dp)
			all := append(append([]string{}, margs...), checkList(&errs, argsField, dp.Args)...)
			pctx := ctx
			if ctxErr == nil { // ошибка -c в args модели уже названа у модели
				var err error
				if pctx, err = contextLength(def, all); err != nil {
					errs = append(errs, FieldError{argsField, err.Error()})
				}
			}
			out = append(out, Model{ID: name, Base: dm.ID, Profile: dp.Name, Path: path, Args: all, ContextLength: pctx})
		}
	}
	if len(out) > maxClientNames {
		errs = append(errs, FieldError{"models", fmt.Sprintf("at most %d client names (models and profiles together)", maxClientNames)})
	}
	return out, errs
}

// checkList — список аргументов модели или профиля: пределы §5.5 п. 10, раскрытие %VAR%, правила MVP §5.3 п. 4–6
// и §5.5 п. 11. Возвращает раскрытый список.
func checkList(errs *[]error, field string, args []string) []string {
	checkListLimits(errs, field, args)
	out := expandArgs(errs, field, args)
	checkArgs(errs, field, out)
	return out
}

// checkListLimits — пределы §5.5 п. 10 для одного списка аргументов (args модели, args профиля, llama.default_args):
// не больше maxArgs элементов, каждый не длиннее maxArgBytes байт до раскрытия %VAR%. Значения в текст не попадают.
func checkListLimits(errs *[]error, field string, args []string) {
	if len(args) > maxArgs {
		*errs = append(*errs, FieldError{field, fmt.Sprintf("at most %d arguments", maxArgs)})
	}
	for k, a := range args {
		if len(a) > maxArgBytes {
			*errs = append(*errs, FieldError{fmt.Sprintf("%s[%d]", field, k), fmt.Sprintf("argument is longer than %d bytes", maxArgBytes)})
		}
	}
}

// WithModels — копия конфига с каталогом из документа API (§4.4, §5.9 спецификации v2): глобальные поля те же
// (неглубокая копия: они не меняются всё время жизни процесса), документ копируется глубоко. Проверяются правила
// MVP §5.3 и §5.5, кроме п. 13 (CheckDefault). Поля ошибок — в стиле документа API.
func (c *Config) WithModels(doc *ModelsDoc) (*Config, []FieldError) {
	d := doc.clone()
	if d == nil {
		d = &ModelsDoc{}
	}
	models, errs := catalog(d, c.Llama.DefaultArgs, apiStyle)
	if len(errs) > 0 {
		return nil, fieldErrors(errs)
	}
	n := *c
	n.ModelsDoc = d
	n.Models = models
	return &n, nil
}

// CheckLimits — пределы §5.5 п. 10 для документа API до любых обращений к диску (шаг 3 PUT, §9.5 спецификации v2):
// число моделей, профилей и имён для клиентов, число и длина аргументов. Поля — в стиле документа API, значения
// в текст не попадают. nil — пределы соблюдены.
func CheckLimits(doc *ModelsDoc) []FieldError {
	if doc == nil {
		return nil
	}
	var errs []error
	if len(doc.Models) > maxModels {
		errs = append(errs, FieldError{"models", fmt.Sprintf("at most %d models", maxModels)})
	}
	names := len(doc.Models)
	for i, m := range doc.Models {
		checkListLimits(&errs, fmt.Sprintf("models[%d].args", i), m.Args)
		if len(m.Profiles) > maxProfiles {
			errs = append(errs, FieldError{fmt.Sprintf("models[%d].profiles", i), fmt.Sprintf("at most %d profiles", maxProfiles)})
		}
		for j, p := range m.Profiles {
			checkListLimits(&errs, fmt.Sprintf("models[%d].profiles[%d].args", i, j), p.Args)
		}
		names += len(m.Profiles)
	}
	if names > maxClientNames {
		errs = append(errs, FieldError{"models", fmt.Sprintf("at most %d client names (models and profiles together)", maxClientNames)})
	}
	if len(errs) == 0 {
		return nil
	}
	return fieldErrors(errs)
}

// ReadModelsFile читает models_file (§5.7 спецификации v2; старт, PUT, «Применить файл», GET /api/admin/models).
// В памяти остаётся не больше MaxModelsFileBytes+1 байт: больший файл отвергнет ParseModels по размеру. ETag считается
// потоком по всему файлу — тот же, что сверяет WriteFileAtomic (F3-D1 ledger первого плана). Файла нет —
// (nil, "absent", false, nil).
func ReadModelsFile(path string) (data []byte, etag string, exists bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ETag(nil, false), false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	defer f.Close()
	h := sha256.New()
	buf := &capBuffer{max: MaxModelsFileBytes + 1}
	if _, err := io.Copy(io.MultiWriter(h, buf), f); err != nil {
		return nil, "", false, err
	}
	return buf.b, `"sha256:` + hex.EncodeToString(h.Sum(nil)) + `"`, true, nil
}

// capBuffer — первые max байт потока; остальное отбрасывается без ошибки.
type capBuffer struct {
	b   []byte
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// readModelsLimited — чтение models_file при старте (Load): как ReadModelsFile; файла нет — fs.ErrNotExist.
func readModelsLimited(path string) ([]byte, error) {
	data, _, exists, err := ReadModelsFile(path)
	if err == nil && !exists {
		return nil, fs.ErrNotExist
	}
	return data, err
}

// WithModelsFile — копия конфига с каталогом из байтов models_file (§5.9 спецификации v2: «Применить файл», опорный
// документ PV8, GET /api/admin/models): правила старта, включая CheckDefault; режим file; ETag — от data. exists ==
// false — файла нет, каталог пуст. Поля — по ключам файла, тексты путей без значений; ошибки разбора — с полем
// models_file. nil ошибок — проверка пройдена.
func (c *Config) WithModelsFile(data []byte, exists bool) (*Config, []FieldError) {
	doc := &ModelsDoc{}
	if exists {
		d, errs := ParseModels(data)
		if errs != nil {
			out := make([]FieldError, len(errs))
			for i, err := range errs {
				out[i] = FieldError{"models_file", err.Error()}
			}
			return nil, out
		}
		doc = d
	}
	models, errs := catalog(doc, c.Llama.DefaultArgs, fileAPIStyle)
	if len(errs) > 0 {
		return nil, fieldErrors(errs)
	}
	n := *c
	n.ModelsMode, n.ModelsDoc, n.Models, n.ModelsETag = ModeFile, doc, models, ETag(data, exists)
	if fe := CheckDefault(&n); fe != nil {
		return nil, []FieldError{*fe}
	}
	return &n, nil
}

// fieldErrors — ошибки каталога как []FieldError (catalog пишет только FieldError; прочие — без поля).
func fieldErrors(errs []error) []FieldError {
	out := make([]FieldError, len(errs))
	for i, err := range errs {
		var fe FieldError
		if errors.As(err, &fe) {
			out[i] = fe
		} else {
			out[i] = FieldError{Message: err.Error()}
		}
	}
	return out
}

// CheckDefault — swap.default_model есть в развёрнутом каталоге (§5.5 п. 13 спецификации v2). nil — правило выполнено.
func CheckDefault(c *Config) *FieldError {
	if d := c.Swap.DefaultModel; d != "" {
		if _, ok := c.Model(d); !ok {
			return &FieldError{"swap.default_model", fmt.Sprintf("%q is not a key of models or a <model>-<profile> name", d)}
		}
	}
	return nil
}

// loadModels выбирает режим (§5.1), читает документ моделей и строит каталог. n — узел models из locallm.yaml
// (Kind 0 — ключа нет); readModels читает models_file (перенос подставляет новый файл из памяти).
func loadModels(c *Config, n *yaml.Node, readModels func(string) ([]byte, error)) []error {
	var doc *ModelsDoc
	var errs []error
	prefix := ""
	if n.Kind == 0 {
		c.ModelsMode = ModeFile
		if !filepath.IsAbs(c.ModelsFile) {
			return nil // ошибку models_file уже дал validate
		}
		prefix = c.ModelsFile + ": "
		data, err := readModels(c.ModelsFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			doc = &ModelsDoc{}
			c.ModelsETag = ETag(nil, false)
		case err != nil:
			return []error{fmt.Errorf("models_file %s: %w", c.ModelsFile, err)}
		default:
			c.ModelsETag = ETag(data, true)
			if doc, errs = ParseModels(data); errs != nil {
				return prefixErrors(prefix, errs)
			}
		}
	} else {
		c.ModelsMode = ModeLegacy
		if filepath.IsAbs(c.ModelsFile) {
			if _, err := os.Stat(c.ModelsFile); err == nil {
				c.Warnings = append(c.Warnings, fmt.Sprintf("models_file %s is not read: locallm.yaml still has models (legacy mode)", c.ModelsFile))
			}
		}
		if doc, errs = decodeModelsNode(n); errs != nil {
			return errs
		}
		if len(doc.Models) == 0 {
			return []error{errors.New("models: at least one model is required")}
		}
	}
	models, cerrs := catalog(doc, c.Llama.DefaultArgs, fileStyle)
	if len(cerrs) > 0 {
		return prefixErrors(prefix, cerrs)
	}
	c.ModelsDoc, c.Models = doc, models
	if fe := CheckDefault(c); fe != nil {
		return []error{*fe}
	}
	return nil
}

// prefixErrors — ошибки файла моделей с его путём впереди (режим file).
func prefixErrors(prefix string, errs []error) []error {
	if prefix == "" {
		return errs
	}
	out := make([]error, len(errs))
	for i, err := range errs {
		out[i] = fmt.Errorf("%s%w", prefix, err)
	}
	return out
}
