package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// SaveResult — ответ 200 сохранения и «Применить файл» (§9.5). Срезы не nil.
type SaveResult struct {
	ETag            string
	RestartRequired []string
	Warnings        []config.FieldError
}

// PutModels — PUT /api/admin/models, шаги 1–11 §9.5. Ошибки: ErrNotMigrated, ErrIfMatchRequired, *InvalidError,
// *config.ChangedError, *DefaultError, *scheduler.InUseError, *WriteError, ошибка Reconfigure (ErrHalted) как есть.
// Успех — файл записан, каталог автомата и live заменены.
func (c *Controller) PutModels(a Actor, ifMatch string, doc *config.ModelsDoc) (SaveResult, error) {
	if c.live.Load().ModelsMode != config.ModeFile { // шаг 1
		return SaveResult{}, c.rejected(a, "put", ErrNotMigrated)
	}
	if ifMatch == "" { // шаг 2
		return SaveResult{}, c.rejected(a, "put", ErrIfMatchRequired)
	}
	if doc == nil {
		doc = &config.ModelsDoc{}
	}
	if d := config.CheckLimits(doc); d != nil { // шаг 3: пределы до диска
		return SaveResult{}, c.rejected(a, "put", &InvalidError{Details: d})
	}
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return c.putLocked(a, ifMatch, doc)
}

func (c *Controller) putLocked(a Actor, ifMatch string, doc *config.ModelsDoc) (SaveResult, error) {
	cfg := c.live.Load()
	data, diskETag, exists, err := config.ReadModelsFile(cfg.ModelsFile) // шаг 4
	if err != nil {
		c.log.Error("models file read failed", "file", cfg.ModelsFile, "err", err)
		return SaveResult{}, &WriteError{Err: err}
	}
	if diskETag != ifMatch {
		return SaveResult{}, c.rejected(a, "put", &config.ChangedError{Path: cfg.ModelsFile, ETag: diskETag})
	}
	ref := cfg.ModelsDoc // шаг 5: опорный документ PV8
	if disk, errs := cfg.WithModelsFile(data, exists); errs == nil {
		ref = disk.ModelsDoc
	}
	// Шаг 6. Сначала правила панели: WithModels делает os.Stat новых путей, а путь панели к чужому тому не должен
	// трогать сеть (F4-D2 ledger первого плана). Ошибки MVP появятся, когда значения пройдут правила панели.
	if d := config.CheckPanel(cfg, ref, doc); len(d) > 0 {
		return SaveResult{}, c.rejected(a, "put", &InvalidError{Details: d})
	}
	next, d := cfg.WithModels(doc)
	if d != nil {
		return SaveResult{}, c.rejected(a, "put", &InvalidError{Details: d})
	}
	encoded := config.EncodeModels(next.ModelsDoc)
	if len(encoded) > config.MaxModelsFileBytes { // F5-D6: файл, который старт отверг бы по размеру
		return SaveResult{}, c.rejected(a, "put", &InvalidError{Details: []config.FieldError{{Field: "models",
			Message: fmt.Sprintf("models.yaml would be larger than %d bytes", config.MaxModelsFileBytes)}}})
	}
	if fe := config.CheckDefault(next); fe != nil { // шаг 7
		return SaveResult{}, c.rejected(a, "put", &DefaultError{Model: cfg.Swap.DefaultModel})
	}
	c.beforeReconfigure()
	if err := c.sched.Reconfigure(next.Models, true); err != nil { // шаг 8: RC0 в автомате
		return SaveResult{}, c.rejected(a, "put", err)
	}
	etag, backup, err := c.writeFile(cfg.ModelsFile, encoded, diskETag) // шаг 9
	if err != nil {
		rerr := c.sched.Reconfigure(cfg.Models, false) // откат: прежний каталог без проверки занятости
		c.log.Error("models file write failed", "file", cfg.ModelsFile, "err", err, "rolled_back", rerr == nil)
		var ce *config.ChangedError
		if errors.As(err, &ce) {
			return SaveResult{}, ce
		}
		return SaveResult{}, &WriteError{Err: err}
	}
	next.ModelsETag = etag
	c.apply(a, "put", cfg, next, backup) // шаг 10
	return c.result(next), nil           // шаг 11
}

// ReloadModels — «Применить файл» (§9.5): файл проверяется как при старте, включая CheckDefault, без правил панели;
// затем Reconfigure с проверкой занятости и шаг 10. Файл не пишется. Ошибки: ErrNotMigrated, *InvalidError,
// *scheduler.InUseError, ошибка Reconfigure как есть.
func (c *Controller) ReloadModels(a Actor) (SaveResult, error) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	cfg := c.live.Load()
	if cfg.ModelsMode != config.ModeFile {
		return SaveResult{}, c.rejected(a, "reload", ErrNotMigrated)
	}
	data, _, exists, err := config.ReadModelsFile(cfg.ModelsFile)
	if err != nil {
		c.log.Warn("models file read failed", "file", cfg.ModelsFile, "err", err)
		return SaveResult{}, c.rejected(a, "reload", &InvalidError{Details: []config.FieldError{{Field: "models_file", Message: "cannot read the file"}}})
	}
	next, d := cfg.WithModelsFile(data, exists)
	if d != nil {
		return SaveResult{}, c.rejected(a, "reload", &InvalidError{Details: d})
	}
	if err := c.sched.Reconfigure(next.Models, true); err != nil {
		return SaveResult{}, c.rejected(a, "reload", err)
	}
	c.apply(a, "reload", cfg, next, "")
	return c.result(next), nil
}

// Migrate — перенос моделей в models.yaml (§5.8). Ошибки: config.ErrAlreadyMigrated; прочие — *WriteError с
// безопасным текстом migrateError (проверка переноса или запись не удались; файлы при ошибке проверки не тронуты).
// Каталог не меняется, Reconfigure не нужен.
func (c *Controller) Migrate(a Actor) (config.MigrateResult, error) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	cfg := c.live.Load()
	n, res, err := c.migrate(cfg)
	if errors.Is(err, config.ErrAlreadyMigrated) {
		return config.MigrateResult{}, c.rejected(a, "migrate", err)
	}
	if err != nil {
		safe := migrateError(err) // исходная ошибка не пишется ни в журнал, ни в ответ
		c.log.Error("models migration failed", "file", cfg.ModelsFile, "err", safe)
		return config.MigrateResult{}, &WriteError{Err: safe}
	}
	c.live.Store(n)
	c.log.Info("models migrated", "actor", a.Kind, "session", a.Session, "file", res.ModelsFile,
		"etag_before", cfg.ModelsETag, "etag_after", res.ModelsETag, "backup", res.Backup)
	return res, nil
}

// errMigrateFiles — текст прочих ошибок переноса (чтение или запись файлов).
var errMigrateFiles = errors.New("migration failed")

// migrateError — безопасная ошибка переноса для журнала и ответа (§5.5 п. 14): config.Migrate перечитывает конфиг в
// стиле файла (fileStyle), и его ошибки содержат пути после раскрытия %VAR% (например, исчезнувший файл модели) и
// тексты системы с путями. Ошибка проверки — ровно config.ErrMigrationCheck (§5.8 п. 4), прочие — errMigrateFiles.
func migrateError(err error) error {
	if errors.Is(err, config.ErrMigrationCheck) {
		return config.ErrMigrationCheck
	}
	return errMigrateFiles
}

// apply — шаг 10 §9.5: live.Store, ForgetLogs удалённых имён, фоновое чтение заголовков, строки журнала (§13).
func (c *Controller) apply(a Actor, op string, old, next *config.Config, backup string) {
	c.live.Store(next)
	added, removed, changed := diffCatalog(old.Models, next.Models)
	if c.logs != nil && len(removed) > 0 {
		c.logs.ForgetLogs(removed)
	}
	c.Warm(old, next)
	c.log.Info("models applied", "op", op, "actor", a.Kind, "session", a.Session, "file", next.ModelsFile,
		"etag_before", old.ModelsETag, "etag_after", next.ModelsETag, "backup", backup,
		"added", strings.Join(added, ","), "removed", strings.Join(removed, ","), "changed", strings.Join(changed, ","))
	restart := ""
	if snap := c.sched.Snapshot(); snap.RestartRequired() {
		restart = snap.Current
	}
	c.log.Info("catalog changed", "added", strings.Join(added, ","), "removed", strings.Join(removed, ","),
		"changed", strings.Join(changed, ","), "restart_required", restart)
}

// result — шаг 11: ETag, restart_required по Snapshot и предупреждения §8.4 (ожидание заголовков ≤ warnTimeout,
// включая зависшие Canonical и os.Stat: cacheGet). Поэтому PUT и «Применить файл» отпускают modelsMu не позже срока.
func (c *Controller) result(next *config.Config) SaveResult {
	r := SaveResult{ETag: next.ModelsETag, RestartRequired: []string{}, Warnings: []config.FieldError{}}
	if snap := c.sched.Snapshot(); snap.RestartRequired() {
		r.RestartRequired = append(r.RestartRequired, snap.Current)
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.warnTimeout)
	defer cancel()
	for _, w := range docWarnings(ctx, c.cache, next) {
		r.Warnings = append(r.Warnings, w...)
	}
	return r
}

// diffCatalog — имена: новые (в порядке next), удалённые (в порядке old) и с другим определением запуска.
func diffCatalog(old, next []config.Model) (added, removed, changed []string) {
	prev := make(map[string]config.Model, len(old))
	for _, m := range old {
		prev[m.ID] = m
	}
	seen := make(map[string]bool, len(next))
	for _, m := range next {
		seen[m.ID] = true
		if p, ok := prev[m.ID]; !ok {
			added = append(added, m.ID)
		} else if !p.SameLaunch(m) {
			changed = append(changed, m.ID)
		}
	}
	for _, m := range old {
		if !seen[m.ID] {
			removed = append(removed, m.ID)
		}
	}
	return added, removed, changed
}

// rejected — строка журнала об отказе сохранения (§13: actor, code, число ошибок — без значений); возвращает err.
func (c *Controller) rejected(a Actor, op string, err error) error {
	problems := 0
	var inv *InvalidError
	if errors.As(err, &inv) {
		problems = len(inv.Details)
	}
	c.log.Info("models save rejected", "op", op, "actor", a.Kind, "session", a.Session, "code", codeOf(err), "problems", problems)
	return err
}

// codeOf — code ответа §9.6 для ошибки сохранения.
func codeOf(err error) string {
	var inv *InvalidError
	var def *DefaultError
	var ce *config.ChangedError
	var iu *scheduler.InUseError
	switch {
	case errors.As(err, &inv):
		return "config_invalid"
	case errors.As(err, &def):
		return "model_is_default"
	case errors.As(err, &ce):
		return "config_changed_on_disk"
	case errors.As(err, &iu):
		return "model_in_use"
	case errors.Is(err, ErrNotMigrated):
		return "models_not_migrated"
	case errors.Is(err, ErrIfMatchRequired):
		return "if_match_required"
	case errors.Is(err, config.ErrAlreadyMigrated):
		return "already_migrated"
	}
	return "internal_error"
}
