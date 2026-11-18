package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"

	"gopkg.in/yaml.v3"
)

var (
	ErrAlreadyMigrated = errors.New("models are already in models_file")
	ErrMigrationCheck  = errors.New("migration check failed")
)

// migrateWrite — точка подмены для тестов (сбой второй записи).
var migrateWrite = WriteFileAtomic

// MigrateResult — итог переноса (ответ 200 POST /api/admin/models/migrate, §5.8 спецификации v2).
type MigrateResult struct {
	ModelsFile string // путь models.yaml
	Backup     string // копия .bak прежнего locallm.yaml
	ModelsETag string // ETag записанного models.yaml
}

// Migrate переносит models: из locallm.yaml в models_file (§5.8); только в режиме legacy, иначе ErrAlreadyMigrated.
// models_file, который указывает на сам locallm.yaml (в любом написании пути, sameFile), — ErrMigrationCheck до
// чтения и записи: иначе первая запись заменила бы locallm.yaml документом моделей. Новые файлы собираются из узлов yaml.v3 с отступом 2 (комментарии сохраняются) и проверяются до записи: тот же
// развёрнутый каталог и те же глобальные настройки, что у c; иначе ошибка с ErrMigrationCheck и ничего не записано.
// Затем пишется models.yaml (существующий уходит в .bak), потом locallm.yaml с If-Match прочитанного. Сбой второй
// записи оставляет locallm.yaml с models: режим legacy при следующем старте, перенос можно повторить. Возвращает
// конфиг в режиме file с тем же каталогом.
func Migrate(c *Config) (*Config, MigrateResult, error) {
	if c.ModelsMode != ModeLegacy {
		return nil, MigrateResult{}, ErrAlreadyMigrated
	}
	abs, err := filepath.Abs(c.Path)
	if err != nil {
		return nil, MigrateResult{}, err
	}
	if sameFile(abs, c.ModelsFile) { // n.ModelsFile равен c.ModelsFile: это проверяет sameGlobals ниже
		return nil, MigrateResult{}, fmt.Errorf("%w: models_file is the same file as locallm.yaml", ErrMigrationCheck)
	}
	for _, f := range []struct{ name, path string }{
		{"server.api_keys_file", c.Server.APIKeysFile},
		{"server.admin_key_file", c.Server.AdminKeyFile},
		{"server.state_file", c.Server.StateFile},
	} { // F5-D1: models.yaml не пишется поверх ключей и state.json
		if filepath.IsAbs(f.path) && sameFile(f.path, c.ModelsFile) {
			return nil, MigrateResult{}, fmt.Errorf("%w: models_file is the same file as %s", ErrMigrationCheck, f.name)
		}
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return nil, MigrateResult{}, err
	}
	mainETag := ETag(data, true)
	newMain, modelsData, err := splitModels(data)
	if err != nil {
		return nil, MigrateResult{}, fmt.Errorf("%w: %v", ErrMigrationCheck, err)
	}
	n, err := load(c.Path, newMain, func(string) ([]byte, error) { return modelsData, nil })
	if err != nil {
		return nil, MigrateResult{}, fmt.Errorf("%w: the new files do not load: %v", ErrMigrationCheck, err)
	}
	n.Version = c.Version
	switch {
	case n.ModelsMode != ModeFile:
		return nil, MigrateResult{}, fmt.Errorf("%w: the new locallm.yaml still has models", ErrMigrationCheck)
	case !sameGlobals(c, n):
		return nil, MigrateResult{}, fmt.Errorf("%w: global settings differ", ErrMigrationCheck)
	case !reflect.DeepEqual(c.Models, n.Models):
		return nil, MigrateResult{}, fmt.Errorf("%w: model catalog differs", ErrMigrationCheck)
	}
	modelsETag, err := fileETag(n.ModelsFile)
	if err != nil {
		return nil, MigrateResult{}, err
	}
	if _, _, err := migrateWrite(n.ModelsFile, modelsData, modelsETag); err != nil {
		return nil, MigrateResult{}, fmt.Errorf("write %s: %w", n.ModelsFile, err)
	}
	_, backup, err := migrateWrite(c.Path, newMain, mainETag)
	if err != nil {
		return nil, MigrateResult{}, fmt.Errorf("write %s: %w", c.Path, err)
	}
	n.ModelsETag = ETag(modelsData, true)
	return n, MigrateResult{ModelsFile: n.ModelsFile, Backup: backup, ModelsETag: n.ModelsETag}, nil
}

// splitModels делит locallm.yaml на новый locallm.yaml без пары models и models.yaml с ней (§5.8 шаги 1–3).
func splitModels(data []byte) (newMain, modelsData []byte, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, errors.New(safeYAMLError(err.Error()))
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("locallm.yaml is not a mapping")
	}
	top := root.Content[0]
	idx := -1
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "models" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, nil, errors.New("locallm.yaml has no models key")
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"},
		{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
		top.Content[idx], top.Content[idx+1],
	}}
	body, err := encodeNode(doc)
	if err != nil {
		return nil, nil, err
	}
	top.Content = append(top.Content[:idx:idx], top.Content[idx+2:]...)
	if newMain, err = encodeNode(&root); err != nil {
		return nil, nil, err
	}
	return newMain, append([]byte(modelsHeader), body...), nil
}

// encodeNode — узел yaml.v3 текстом с отступом 2.
func encodeNode(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// sameGlobals — совпадают ли глобальные поля (всё, кроме моделей, режима, ETag и предупреждений).
func sameGlobals(a, b *Config) bool {
	x, y := *a, *b
	for _, c := range []*Config{&x, &y} {
		c.ModelsMode, c.ModelsDoc, c.Models, c.ModelsETag, c.Warnings = "", nil, nil, "", nil
	}
	return reflect.DeepEqual(x, y)
}

// fileETag — ETag файла на диске (нет файла — "absent").
func fileETag(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ETag(nil, false), nil
	}
	if err != nil {
		return "", err
	}
	return ETag(data, true), nil
}
