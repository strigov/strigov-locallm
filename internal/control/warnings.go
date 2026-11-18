package control

import (
	"context"
	"fmt"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
)

// catalogGroups — развёрнутый каталог по моделям документа: базовый вариант, затем его профили (порядок §5.4).
func catalogGroups(models []config.Model) [][]config.Model {
	var g [][]config.Model
	for _, m := range models {
		if m.Profile == "" {
			g = append(g, nil)
		}
		if len(g) > 0 {
			g[len(g)-1] = append(g[len(g)-1], m)
		}
	}
	return g
}

// headerGet — чтение заголовка через кэш; точка подмены (тесты пакета).
var headerGet = (*gguf.Cache).Get

// cacheGet — cache.Get, ожидание которого целиком ограничено ctx. Cache.Get выполняет Canonical и os.Stat до
// проверки ctx, а на недоступном сетевом пути они висят в системном вызове: без этого запрос (а у PUT и «Применить
// файл» — и modelsMu) держался бы дольше warnTimeout. Брошенный вызов завершится сам; после срока новые вызовы не
// начинаются, поэтому на один запрос висит не больше одного вызова.
func cacheGet(ctx context.Context, cache *gguf.Cache, path string) (gguf.Info, error) {
	if err := ctx.Err(); err != nil {
		return gguf.Info{}, err
	}
	type got struct {
		info gguf.Info
		err  error
	}
	ch := make(chan got, 1)
	get := headerGet
	go func() {
		info, err := get(cache, ctx, path)
		ch <- got{info, err}
	}()
	select {
	case g := <-ch:
		return g.info, g.err
	case <-ctx.Done():
		return gguf.Info{}, ctx.Err()
	}
}

// mmprojValues — номера значений --mmproj и -mm в args (элемент после флага).
func mmprojValues(args []string) []int {
	var out []int
	for k := 0; k+1 < len(args); k++ {
		if args[k] == "--mmproj" || args[k] == "-mm" {
			out = append(out, k+1)
			k++
		}
	}
	return out
}

// docWarnings — предупреждения §8.4 по моделям документа cfg.ModelsDoc (индекс — номер модели документа; у каждой
// не nil): файл модели — проектор или адаптер; значение --mmproj — не проектор; context_length больше context_train;
// заголовок не читается. Заголовки — из кэша через cacheGet; ожидание (вместе с Canonical и os.Stat внутри
// Cache.Get) ограничено ctx: не дождались — предупреждения о файле нет.
// cache == nil — предупреждений нет. Тексты без путей и значений аргументов.
func docWarnings(ctx context.Context, cache *gguf.Cache, cfg *config.Config) [][]config.FieldError {
	var n int
	if cfg.ModelsDoc != nil {
		n = len(cfg.ModelsDoc.Models)
	}
	out := make([][]config.FieldError, n)
	groups := catalogGroups(cfg.Models)
	for i := range out {
		out[i] = []config.FieldError{}
		if cache != nil && i < len(groups) {
			out[i] = modelWarnings(ctx, cache, i, groups[i])
		}
	}
	return out
}

// modelWarnings — предупреждения одной модели документа i; group — её базовый вариант и профили.
func modelWarnings(ctx context.Context, cache *gguf.Cache, i int, group []config.Model) []config.FieldError {
	out := []config.FieldError{}
	add := func(field, msg string) { out = append(out, config.FieldError{Field: field, Message: msg}) }
	base := group[0]
	pf := fmt.Sprintf("models[%d]", i)
	info, err := cacheGet(ctx, cache, base.Path)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			add(pf+".path", "cannot read the GGUF header: "+err.Error())
		}
	case info.Kind == "mmproj":
		add(pf+".path", "file is a projector (mmproj), not a model")
	case info.Kind == "adapter":
		add(pf+".path", "file is an adapter, not a model")
	}
	if err == nil && info.ContextTrain > 0 {
		for j, m := range group {
			if j > 0 && m.ContextLength == base.ContextLength {
				continue // то же значение уже названо у базового варианта
			}
			if int64(m.ContextLength) > info.ContextTrain {
				field := pf + ".args"
				if j > 0 {
					field = fmt.Sprintf("%s.profiles[%d].args", pf, j-1)
				}
				add(field, fmt.Sprintf("context length is larger than the model's training context (%d)", info.ContextTrain))
			}
		}
	}
	nBase := len(base.Args)
	for j, m := range group {
		for _, k := range mmprojValues(m.Args) {
			field := fmt.Sprintf("%s.args[%d]", pf, k)
			if j > 0 {
				if k < nBase {
					continue // значение из args модели проверено у базового варианта
				}
				field = fmt.Sprintf("%s.profiles[%d].args[%d]", pf, j-1, k-nBase)
			}
			pinfo, perr := cacheGet(ctx, cache, m.Args[k])
			switch {
			case perr != nil:
				if ctx.Err() == nil {
					add(field, "--mmproj: cannot read the GGUF header: "+perr.Error())
				}
			case pinfo.Kind != "mmproj":
				add(field, "--mmproj: file is not a projector (mmproj)")
			}
		}
	}
	return out
}

// ggufPaths — пути моделей и значений --mmproj каталога cfg (раскрытые), без повторов.
func ggufPaths(cfg *config.Config) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, m := range cfg.Models {
		add(m.Path)
		for _, k := range mmprojValues(m.Args) {
			add(m.Args[k])
		}
	}
	return out
}

// Warm — фоновое чтение заголовков путей next, которых нет в old (§8.3; шаг 7 §4.7 — old == nil, все пути каталога).
func (c *Controller) Warm(old, next *config.Config) {
	if c.cache == nil || next == nil {
		return
	}
	seen := map[string]bool{}
	if old != nil {
		for _, p := range ggufPaths(old) {
			seen[p] = true
		}
	}
	var paths []string
	for _, p := range ggufPaths(next) {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return
	}
	go func() {
		for _, p := range paths {
			_, _ = c.cache.Get(context.Background(), p)
		}
	}()
}
