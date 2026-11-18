package control

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler"
)

// StateView — данные GET /api/admin/state (§9.5) без форматирования.
type StateView struct {
	Snap        *scheduler.Snapshot
	Cfg         *config.Config
	PauseSaved  bool
	AppliedETag string // "" — режим legacy
	DiskETag    string // "" — режим legacy или файл не читается
	OutOfSync   bool
}

// diskStamp — ETag файла при известных пути, размере и времени записи.
type diskStamp struct {
	path  string
	size  int64
	mtime time.Time
	etag  string
}

// State — Snapshot, применённый конфиг, pause_saved и сверка ETag применённого файла с файлом на диске.
func (c *Controller) State() StateView {
	cfg := c.live.Load()
	v := StateView{Snap: c.sched.Snapshot(), Cfg: cfg, PauseSaved: c.pauseSaved.Load()}
	if cfg.ModelsMode == config.ModeFile {
		v.AppliedETag = cfg.ModelsETag
		v.DiskETag = c.diskETag(cfg.ModelsFile)
		v.OutOfSync = v.DiskETag != v.AppliedETag
	}
	return v
}

// diskETag — ETag файла на диске; пересчитывается, только если размер или время записи изменились. Нет файла —
// "absent"; не читается — "".
func (c *Controller) diskETag(path string) string {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.ETag(nil, false)
	}
	if err != nil {
		return ""
	}
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	if d := c.disk; d.path == path && d.size == st.Size() && d.mtime.Equal(st.ModTime()) {
		return d.etag
	}
	_, etag, exists, err := config.ReadModelsFile(path)
	if err != nil {
		return ""
	}
	if !exists {
		return config.ETag(nil, false)
	}
	c.disk = diskStamp{path: path, size: st.Size(), mtime: st.ModTime(), etag: etag}
	return etag
}

// ModelsView — данные GET /api/admin/models (§9.5). Срезы не nil.
type ModelsView struct {
	Mode, ModelsFile        string
	ModelsDirs, DefaultArgs []string
	DefaultModel            string
	ETag                    string // ETag файла на диске; "" — legacy или файл не читается
	OutOfSync               bool
	DiskErrors              []config.FieldError
	Models                  []ModelView
}

// ModelView — модель документа; строки — до раскрытия %VAR%. Срезы не nil.
type ModelView struct {
	ID, Path        string
	PathHandOnly    bool
	Args            []string
	ArgsHandOnly    bool
	Profiles        []ProfileView
	HandOnlyReasons []string
	Info            *gguf.Info // nil — заголовок не прочитан
	Warnings        []config.FieldError
}

// ProfileView — профиль модели. Args не nil.
type ProfileView struct {
	Name     string
	Args     []string
	HandOnly bool
}

// Models — документ файла на диске, если он проходит проверку старта (WithModelsFile), иначе применённый документ
// и ошибки файла в DiskErrors (тексты без значений). Метки hand_only — по правилам панели; info — из кэша GGUF;
// warnings — §8.4. Ожидание заголовков ограничено ctx и warnTimeout.
func (c *Controller) Models(ctx context.Context) ModelsView {
	cfg := c.live.Load()
	v := ModelsView{Mode: cfg.ModelsMode, ModelsFile: cfg.ModelsFile, ModelsDirs: orEmpty(cfg.ModelsDirs),
		DefaultArgs: orEmpty(cfg.Llama.DefaultArgsSource), DefaultModel: cfg.Swap.DefaultModel,
		DiskErrors: []config.FieldError{}, Models: []ModelView{}}
	show := cfg
	if cfg.ModelsMode == config.ModeFile {
		data, etag, exists, err := config.ReadModelsFile(cfg.ModelsFile)
		if err != nil {
			v.DiskErrors = append(v.DiskErrors, config.FieldError{Field: "models_file", Message: "cannot read the file"})
		} else {
			v.ETag = etag
			if disk, errs := cfg.WithModelsFile(data, exists); errs != nil {
				v.DiskErrors = errs
			} else {
				show = disk
			}
		}
		v.OutOfSync = v.ETag != cfg.ModelsETag
	}
	doc := show.ModelsDoc
	if doc == nil {
		doc = &config.ModelsDoc{}
	}
	marks := config.HandOnlyMarks(show, doc)
	groups := catalogGroups(show.Models)
	wctx, cancel := context.WithTimeout(ctx, c.warnTimeout)
	defer cancel()
	warns := docWarnings(wctx, c.cache, show)
	for i, m := range doc.Models {
		mv := ModelView{ID: m.ID, Path: m.Path, PathHandOnly: marks[i].Path, Args: orEmpty(m.Args), ArgsHandOnly: marks[i].Args,
			Profiles: []ProfileView{}, HandOnlyReasons: orEmpty(marks[i].Reasons), Warnings: warns[i]}
		for j, p := range m.Profiles {
			mv.Profiles = append(mv.Profiles, ProfileView{Name: p.Name, Args: orEmpty(p.Args), HandOnly: marks[i].Profiles[j]})
		}
		if c.cache != nil && i < len(groups) {
			if info, err := cacheGet(wctx, c.cache, groups[i][0].Path); err == nil {
				mv.Info = &info
			}
		}
		v.Models = append(v.Models, mv)
	}
	return v
}

// ScanFile — файл поиска с именами моделей каталога, которые его используют. MMProj и UsedBy не nil.
type ScanFile struct {
	gguf.ScanModel
	UsedBy []string
}

// ScanView — данные GET /api/admin/scan (§9.5). Срезы не nil.
type ScanView struct {
	Roots     []string
	Truncated bool
	Elapsed   time.Duration
	Files     []ScanFile
	Errors    []gguf.ScanError
}

// scanWait — сколько обработчик ждёт весь поиск, вместе с used_by: срок обхода плюс запас (FR1-N1 ledger
// планировщика, критерий 3). Обход, застрявший в системном вызове, этим не прерывается; следующие вызовы
// присоединяются к нему и тоже ждут не дольше scanWait. Так же устроен расчёт used_by (usedByWait).
var scanWait = gguf.DefaultLimits.Timeout + 5*time.Second

// canonical — config.Canonical для used_by; точка подмены (тесты пакета).
var canonical = config.Canonical

// Scan — поиск .gguf в models_dirs (§8.2). Не дождались обхода — truncated и ошибка "scan did not finish in time"
// без файлов; не дождались used_by — файлы с пустыми used_by, truncated и та же ошибка. Весь вызов — не дольше
// scanWait.
func (c *Controller) Scan(ctx context.Context) ScanView {
	cfg := c.live.Load()
	v := ScanView{Roots: orEmpty(slices.Clone(cfg.ModelsDirs)), Files: []ScanFile{}, Errors: []gguf.ScanError{}}
	if c.scanner == nil || len(cfg.ModelsDirs) == 0 {
		return v
	}
	start := c.now()
	wctx, cancel := context.WithTimeout(ctx, scanWait)
	defer cancel()
	res, err := c.scanner.Scan(wctx, cfg.ModelsDirs)
	v.Elapsed = c.now().Sub(start)
	if err != nil {
		v.Truncated = true
		v.Errors = append(v.Errors, gguf.ScanError{Message: "scan did not finish in time"})
		return v
	}
	v.Truncated = res.Truncated
	used, ok := c.usedByWait(wctx, cfg) // тот же срок: Canonical путей каталога может висеть на сетевом пути
	for _, m := range res.Models {
		f := ScanFile{ScanModel: m, UsedBy: []string{}}
		f.MMProj = orEmpty(slices.Clone(m.MMProj)) // срезы результата общие: копия
		if ids := used[strings.ToLower(filepath.Clean(m.Path))]; ids != nil {
			f.UsedBy = ids
		}
		v.Files = append(v.Files, f)
	}
	v.Errors = append(v.Errors, res.Errors...)
	if !ok {
		v.Truncated = true
		v.Errors = append(v.Errors, gguf.ScanError{Message: "scan did not finish in time"})
	}
	return v
}

// usedByWait — usedBy(cfg) с ожиданием, ограниченным ctx. Canonical не принимает ctx и на недоступном сетевом пути
// модели, добавленной вручную, висит в системном вызове, поэтому расчёт идёт в отдельной горутине. usedSem пускает
// не больше одного расчёта сразу: зависший расчёт не плодит новых, следующие вызовы ждут его в пределах своего срока.
// false — не дождались.
func (c *Controller) usedByWait(ctx context.Context, cfg *config.Config) (map[string][]string, bool) {
	select {
	case c.usedSem <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	ch := make(chan map[string][]string, 1)
	go func() {
		defer func() { <-c.usedSem }()
		ch <- usedBy(cfg)
	}()
	select {
	case m := <-ch:
		return m, true
	case <-ctx.Done():
		return nil, false
	}
}

// usedBy — канонический путь в нижнем регистре → базовые имена моделей каталога, которые используют файл как модель
// или как значение --mmproj (порядок каталога, без повторов). Пути поиска уже канонические: корни models_dirs
// канонические, а ссылки внутри корней не обходятся.
func usedBy(cfg *config.Config) map[string][]string {
	out := map[string][]string{}
	canon := map[string]string{}
	add := func(p, id string) {
		k, ok := canon[p]
		if !ok {
			cp, err := canonical(p)
			if err == nil {
				k = strings.ToLower(cp)
			}
			canon[p] = k
		}
		if k != "" && !slices.Contains(out[k], id) {
			out[k] = append(out[k], id)
		}
	}
	for _, m := range cfg.Models {
		add(m.Path, m.Base)
		for _, k := range mmprojValues(m.Args) {
			add(m.Args[k], m.Base)
		}
	}
	return out
}

// orEmpty — s или пустой срез вместо nil (в JSON — [], а не null).
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
