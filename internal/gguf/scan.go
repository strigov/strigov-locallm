package gguf

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Limits — лимиты поиска (§8.2).
type Limits struct {
	Depth   int           // глубина каталогов от корня
	Entries int           // записей каталогов всего
	Files   int           // файлов .gguf
	Workers int           // параллельных чтений заголовков
	Timeout time.Duration // на весь поиск
}

// DefaultLimits — значения §8.2.
var DefaultLimits = Limits{Depth: 6, Entries: 50_000, Files: 2_000, Workers: 4, Timeout: 30 * time.Second}

// ScanResult — итог поиска. Срезы общие для всех, кто дождался одного поиска: не изменять.
type ScanResult struct {
	Models    []ScanModel
	Errors    []ScanError
	Truncated bool // сработал лимит
}

// ScanModel — файл .gguf; у составной модели — только шард 00001.
type ScanModel struct {
	Path     string
	Info     Info
	ShardsOK bool     // все шарды на месте; у одиночного файла — true
	MMProj   []string // у Kind == "model": проекторы из того же каталога
}

// ScanError — ошибка чтения каталога или файла; Message не содержит пути.
type ScanError struct {
	Path, Message string
}

// Scanner — поиск в models_dirs (§8.2). Одновременно идёт один поиск: второй вызов присоединяется к нему.
type Scanner struct {
	cache  *Cache
	limits Limits
	now    func() time.Time

	mu      sync.Mutex
	cur     *scanCall
	started int // начатых обходов (проверка singleflight в тестах)
	joined  int // вызовов, присоединившихся к идущему обходу (то же)
}

type scanCall struct {
	done chan struct{}
	res  ScanResult
}

// NewScanner — now == nil: time.Now; неположительные поля Limits заменяются значениями DefaultLimits
// (Entries <= 0 иначе не оставляет ReadDir ни одной записи, и обход не кончается).
func NewScanner(c *Cache, l Limits, now func() time.Time) *Scanner {
	if now == nil {
		now = time.Now
	}
	if l.Depth <= 0 {
		l.Depth = DefaultLimits.Depth
	}
	if l.Entries <= 0 {
		l.Entries = DefaultLimits.Entries
	}
	if l.Files <= 0 {
		l.Files = DefaultLimits.Files
	}
	if l.Workers <= 0 {
		l.Workers = DefaultLimits.Workers
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultLimits.Timeout
	}
	return &Scanner{cache: c, limits: l, now: now}
}

// Scan обходит корни (§8.2). Если поиск уже идёт, вызов ждёт его результат (его корни не учитываются). Ошибка —
// только ctx.Err() ожидания: сам обход ограничен своими лимитами и уходом вызывающего не отменяется. Корень
// открывается как указан; ссылки и junction внутри корня не обходятся и в выдачу не попадают.
func (s *Scanner) Scan(ctx context.Context, roots []string) (ScanResult, error) {
	s.mu.Lock()
	c := s.cur
	if c == nil {
		c = &scanCall{done: make(chan struct{})}
		s.cur = c
		s.started++
		go s.run(c, slices.Clone(roots))
	} else {
		s.joined++
	}
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.res, nil
	case <-ctx.Done():
		return ScanResult{}, ctx.Err()
	}
}

func (s *Scanner) run(c *scanCall, roots []string) {
	w := &walker{s: s, deadline: s.now().Add(s.limits.Timeout)}
	for _, root := range roots {
		if w.stopped {
			break
		}
		w.dir(root, 0)
	}
	w.collect(w.read())
	c.res = w.res
	s.mu.Lock()
	s.cur = nil
	s.mu.Unlock()
	close(c.done)
}

// walker — состояние одного обхода; dir и read работают в одной горутине.
type walker struct {
	s        *Scanner
	deadline time.Time
	entries  int
	files    []string
	stopped  bool
	res      ScanResult
}

type fileResult struct {
	info    Info
	err     error
	skipped bool // срок истёк до начала чтения: файл в выдачу не попадает
}

func (w *walker) stop() {
	w.stopped = true
	w.res.Truncated = true
}

func (w *walker) expired() bool { return !w.s.now().Before(w.deadline) }

func (w *walker) fail(path string, err error) {
	w.res.Errors = append(w.res.Errors, ScanError{Path: path, Message: unwrapPath(err).Error()})
}

// dirBatch — сколько записей каталога читается за раз: большой каталог не читается целиком сверх лимита записей.
const dirBatch = 256

// dir — каталог глубины depth (корень — 0). Глубже limits.Depth — truncated, соседние каталоги обходятся дальше;
// лимит записей, файлов или времени останавливает обход: срок проверяется перед открытием каталога (корни — тоже
// каталоги) и перед каждой порцией записей, поэтому после срока не открываются и пустые каталоги. Записи читаются
// порциями не больше остатка лимита записей плюс одна (она и даёт truncated); каждая порция сортируется по имени.
func (w *walker) dir(path string, depth int) {
	if w.expired() {
		w.stop()
		return
	}
	f, err := os.Open(path)
	if err != nil {
		w.fail(path, err)
		return
	}
	defer f.Close()
	for !w.stopped {
		if w.expired() {
			w.stop()
			return
		}
		ents, err := f.ReadDir(min(dirBatch, w.s.limits.Entries-w.entries+1))
		slices.SortFunc(ents, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		w.entriesOf(path, depth, ents) // прочитанные до ошибки записи всё равно обходятся
		if err != nil {
			if !errors.Is(err, io.EOF) {
				w.fail(path, err)
			}
			return
		}
	}
}

// entriesOf — одна порция записей каталога path.
func (w *walker) entriesOf(path string, depth int, ents []os.DirEntry) {
	for _, d := range ents {
		if w.stopped {
			return
		}
		if w.expired() {
			w.stop()
			return
		}
		if w.entries++; w.entries > w.s.limits.Entries {
			w.stop()
			return
		}
		p := filepath.Join(path, d.Name())
		fi, err := d.Info() // без перехода по ссылке
		if err != nil {
			w.fail(p, err)
			continue
		}
		switch {
		case reparse(fi): // ссылки, junction и прочие точки повторной обработки
		case fi.IsDir():
			if depth+1 > w.s.limits.Depth {
				w.res.Truncated = true
				continue
			}
			w.dir(p, depth+1)
		case fi.Mode().IsRegular() && strings.EqualFold(filepath.Ext(p), ".gguf"):
			if m := shardRe.FindStringSubmatch(d.Name()); m != nil && m[1] != "00001" {
				continue // в выдаче только шард 00001
			}
			if len(w.files) >= w.s.limits.Files {
				w.stop()
				return
			}
			w.files = append(w.files, p)
		}
	}
}

// read — заголовки через кэш в limits.Workers горутинах. Срок проверяется в обработчике непосредственно перед
// каждым чтением: после срока новые чтения не начинаются, даже если задание было отправлено до срока.
func (w *walker) read() []fileResult {
	out := make([]fileResult, len(w.files))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(1, w.s.limits.Workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if w.expired() {
					out[i].skipped = true
					continue
				}
				out[i].info, out[i].err = w.s.cache.Get(context.Background(), w.files[i])
			}
		}()
	}
	sent := 0
	for ; sent < len(w.files) && !w.expired(); sent++ {
		jobs <- sent
	}
	close(jobs)
	wg.Wait()
	for i := sent; i < len(out); i++ {
		out[i].skipped = true
	}
	return out
}

// collect — модели в порядке обхода, ошибки чтения, shards_ok и проекторы своего каталога. Файлы, чтение которых
// не началось до срока, в выдачу не попадают и дают truncated. Проверка шардов (stat) — тоже в пределах срока
// поиска: срок проверяется перед проверкой шардов каждой составной модели и перед каждым stat; истёк — дальше диск
// не трогается, модель с непроверенными шардами в выдачу не попадает и даёт truncated. Сборка без диска (одиночные
// файлы, проекторы) идёт и после срока: их заголовки уже прочитаны.
func (w *walker) collect(rs []fileResult) {
	proj := map[string][]string{} // каталог → проекторы
	for i, p := range w.files {
		if !rs[i].skipped && rs[i].err == nil && rs[i].info.Kind == "mmproj" {
			dir := filepath.Dir(p)
			proj[dir] = append(proj[dir], p)
		}
	}
	for i, p := range w.files {
		r := rs[i]
		if r.skipped {
			w.res.Truncated = true
			continue
		}
		if r.err != nil {
			w.res.Errors = append(w.res.Errors, ScanError{Path: p, Message: r.err.Error()})
			continue
		}
		m := ScanModel{Path: p, Info: r.info, ShardsOK: true}
		if r.info.SplitCount > 1 {
			late := false // срок истёк до или во время проверки шардов — ShardsOK неизвестен
			total, ok := shardSum(p, r.info.SplitCount, func() bool {
				late = late || w.expired()
				return late
			})
			if late {
				w.res.Truncated = true
				continue
			}
			m.ShardsOK = ok
			if total > 0 {
				m.Info.Size = total // сумма шардов сейчас: заголовок из кэша мог быть прочитан при другом наборе
			}
		}
		if r.info.Kind == "model" {
			m.MMProj = slices.Clone(proj[filepath.Dir(p)])
		}
		w.res.Models = append(w.res.Models, m)
	}
}
