package gguf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testLimits = Limits{Depth: 6, Entries: 1000, Files: 1000, Workers: 4, Timeout: time.Hour}

func scanOK(t *testing.T, s *Scanner, roots ...string) ScanResult {
	t.Helper()
	res, err := s.Scan(context.Background(), roots)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

func paths(res ScanResult) []string {
	var out []string
	for _, m := range res.Models {
		out = append(out, m.Path)
	}
	return out
}

func projFile(t *testing.T, dir, name string) string {
	t.Helper()
	return newFile(3).str("general.type", "mmproj").str("general.architecture", "clip").write(t, dir, name)
}

func shardFile(t *testing.T, dir, name string, count uint16) string {
	t.Helper()
	g := newFile(3).str("general.architecture", "qwen")
	g.kv("split.count", tUint16, func() { g.w(count) })
	return g.write(t, dir, name)
}

func junkFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// junction — как в internal/config/canon_windows_test.go: mklink /J не требует прав администратора.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v\n%s", link, target, err, out)
	}
}

// §8.2: модели, проекторы своего каталога, только шард 00001 с shards_ok, ошибки без пути, порядок обхода.
func TestScanFindsModelsShardsAndProjectors(t *testing.T) {
	root, root2 := t.TempDir(), t.TempDir()
	model := modelFile(t, filepath.Join(root, "a"), "model.gguf")
	proj := projFile(t, filepath.Join(root, "a"), "mmproj-x.gguf")
	junkFile(t, filepath.Join(root, "a"), "notes.txt")
	q := shardFile(t, filepath.Join(root, "b"), "q-00001-of-00002.gguf", 2)
	junkFile(t, filepath.Join(root, "b"), "q-00002-of-00002.gguf")
	r := shardFile(t, filepath.Join(root, "c"), "r-00001-of-00003.gguf", 3)
	junkFile(t, filepath.Join(root, "c"), "r-00002-of-00003.gguf")
	bad := junkFile(t, root, "bad.gguf")
	d := modelFile(t, root2, "d.gguf")
	missing := filepath.Join(root, "missing")

	res := scanOK(t, NewScanner(NewCache(0), testLimits, nil), root, missing, root2)
	if want := []string{proj, model, q, r, d}; !slices.Equal(paths(res), want) {
		t.Fatalf("models %q, want %q", paths(res), want)
	}
	if res.Truncated {
		t.Fatal("truncated")
	}
	byPath := map[string]ScanModel{}
	for _, m := range res.Models {
		byPath[m.Path] = m
	}
	if m := byPath[model]; m.Info.Kind != "model" || !m.ShardsOK || !slices.Equal(m.MMProj, []string{proj}) {
		t.Fatalf("model %+v", m)
	}
	if m := byPath[proj]; m.Info.Kind != "mmproj" || m.MMProj != nil {
		t.Fatalf("projector %+v", m)
	}
	q1, _ := os.Stat(q)
	if m := byPath[q]; m.Info.SplitCount != 2 || !m.ShardsOK || m.Info.Size != q1.Size()+int64(len("not a model")) ||
		len(m.MMProj) != 0 {
		t.Fatalf("complete split %+v", m)
	}
	if m := byPath[r]; m.Info.SplitCount != 3 || m.ShardsOK {
		t.Fatalf("missing shard %+v", m)
	}
	errs := map[string]string{}
	for _, e := range res.Errors {
		errs[e.Path] = e.Message
		if strings.Contains(strings.ToLower(e.Message), strings.ToLower(filepath.Base(root))) {
			t.Fatalf("error message contains the path: %q", e.Message)
		}
	}
	if len(errs) != 2 || errs[missing] == "" || errs[bad] != ErrNotGGUF.Error() {
		t.Fatalf("errors %q", errs)
	}
}

// F4-PM2: размер составной модели пересчитывается при каждом поиске: добавленный шард меняет сумму, хотя
// заголовок шарда 00001 взят из кэша и его ключ не изменился.
func TestScanSplitSizeFollowsShards(t *testing.T) {
	dir := t.TempDir()
	first := shardFile(t, dir, "s-00001-of-00002.gguf", 2)
	s1, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScanner(NewCache(0), testLimits, nil)
	res := scanOK(t, sc, dir)
	if len(res.Models) != 1 || res.Models[0].ShardsOK || res.Models[0].Info.Size != s1.Size() {
		t.Fatalf("missing second shard: %+v", res.Models)
	}
	second := junkFile(t, dir, "s-00002-of-00002.gguf")
	s2, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	res = scanOK(t, sc, dir) // тот же сканер и кэш: заголовок 00001 — из кэша
	if len(res.Models) != 1 {
		t.Fatalf("models %+v", res.Models)
	}
	m := res.Models[0]
	if !m.ShardsOK || m.Info.Size != s1.Size()+s2.Size() {
		t.Fatalf("after adding the second shard: %+v", m)
	}
}

// Ссылки и junction внутри корня не обходятся и в выдачу не попадают (§8.2).
func TestScanSkipsJunctions(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	m := modelFile(t, filepath.Join(root, "real"), "m.gguf")
	modelFile(t, outside, "x.gguf")
	junction(t, filepath.Join(root, "link"), filepath.Join(root, "real"))
	junction(t, filepath.Join(root, "out"), outside)
	res := scanOK(t, NewScanner(NewCache(0), testLimits, nil), root)
	if got := paths(res); !slices.Equal(got, []string{m}) || len(res.Errors) != 0 || res.Truncated {
		t.Fatalf("models %q, errors %v, truncated %v", got, res.Errors, res.Truncated)
	}
	fi, err := os.Lstat(filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !reparse(fi) {
		t.Fatal("junction is not a reparse point")
	}
}

// Глубина: каталог глубже лимита — truncated, соседи обходятся; записи и файлы — обход останавливается.
func TestScanLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		root := t.TempDir()
		m2 := modelFile(t, filepath.Join(root, "1", "2"), "m2.gguf")
		modelFile(t, filepath.Join(root, "1", "2", "3"), "m3.gguf")
		mz := modelFile(t, filepath.Join(root, "z"), "mz.gguf")
		l := testLimits
		l.Depth = 2
		res := scanOK(t, NewScanner(NewCache(0), l, nil), root)
		if got := paths(res); !slices.Equal(got, []string{m2, mz}) || !res.Truncated {
			t.Fatalf("models %q, truncated %v", got, res.Truncated)
		}
	})
	flat := func(t *testing.T) (string, []string) {
		root := t.TempDir()
		var ps []string
		for i := range 10 {
			ps = append(ps, modelFile(t, root, fmt.Sprintf("f%02d.gguf", i)))
		}
		return root, ps
	}
	t.Run("entries", func(t *testing.T) {
		root, ps := flat(t)
		l := testLimits
		l.Entries = 5
		res := scanOK(t, NewScanner(NewCache(0), l, nil), root)
		if got := paths(res); !slices.Equal(got, ps[:5]) || !res.Truncated {
			t.Fatalf("models %q, truncated %v", got, res.Truncated)
		}
	})
	t.Run("files", func(t *testing.T) {
		root, ps := flat(t)
		l := testLimits
		l.Files = 3
		res := scanOK(t, NewScanner(NewCache(0), l, nil), root)
		if got := paths(res); !slices.Equal(got, ps[:3]) || !res.Truncated {
			t.Fatalf("models %q, truncated %v", got, res.Truncated)
		}
		l.Files = 10 // ровно на лимите — не truncated
		if res := scanOK(t, NewScanner(NewCache(0), l, nil), root); len(res.Models) != 10 || res.Truncated {
			t.Fatalf("at the limit: %d models, truncated %v", len(res.Models), res.Truncated)
		}
	})
}

// F4-D1: неположительные поля Limits заменяются значениями DefaultLimits; Entries < 0 раньше не оставлял
// ReadDir ни одной записи, и пустой каталог обходился вечно.
func TestScanDefaultLimitsForBadValues(t *testing.T) {
	l := Limits{Depth: 0, Entries: -1, Files: -2, Workers: -1, Timeout: -time.Second}
	s := NewScanner(NewCache(0), l, nil)
	if s.limits != DefaultLimits {
		t.Fatalf("limits %+v, want %+v", s.limits, DefaultLimits)
	}
	type out struct {
		res ScanResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.Scan(context.Background(), []string{t.TempDir()})
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		if len(o.res.Models) != 0 || len(o.res.Errors) != 0 || o.res.Truncated {
			t.Fatalf("empty dir: %+v", o.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan of an empty dir did not finish")
	}
}

// Срок 30 с по подменяемым часам: истёк при обходе, между чтениями или перед проверкой шардов — truncated,
// найденное до срока в выдаче.
func TestScanTimeout(t *testing.T) {
	root := t.TempDir()
	for i := range 20 {
		modelFile(t, root, fmt.Sprintf("f%02d.gguf", i))
	}
	l := testLimits
	l.Timeout = 30 * time.Second
	t.Run("walk", func(t *testing.T) {
		var clock atomic.Int64 // каждый вызов часов — плюс 2 с
		now := func() time.Time { return time.Unix(clock.Add(2), 0) }
		res := scanOK(t, NewScanner(NewCache(0), l, now), root)
		if !res.Truncated || len(res.Models) >= 20 {
			t.Fatalf("%d models, truncated %v", len(res.Models), res.Truncated)
		}
	})
	t.Run("read", func(t *testing.T) {
		var clock, late atomic.Int64 // часы стоят, каждое чтение — плюс 10 с; срок — 30 с от нуля
		now := func() time.Time { return time.Unix(clock.Load(), 0) }
		c := NewCache(0)
		read := c.read
		c.read = func(ctx context.Context, path string) (Info, error) {
			if clock.Add(10) > 30 { // чтение началось на сроке или после него
				late.Add(1)
			}
			return read(ctx, path)
		}
		one := l
		one.Workers = 1
		res := scanOK(t, NewScanner(c, one, now), root)
		if !res.Truncated || len(res.Models) != 3 || len(res.Errors) != 0 || late.Load() != 0 {
			t.Fatalf("%d models, %d errors, truncated %v, %d reads after the deadline",
				len(res.Models), len(res.Errors), res.Truncated, late.Load())
		}
	})
	t.Run("shards", func(t *testing.T) {
		// Заголовок составной модели прочитан до срока, но срок истёк к проверке шардов: stat шардов не делается,
		// модель в выдачу не попадает, truncated. Одиночный файл, прочитанный до срока, остаётся в выдаче.
		dir := t.TempDir()
		m := modelFile(t, dir, "a.gguf")
		shardFile(t, dir, "s-00001-of-00002.gguf", 2)
		junkFile(t, dir, "s-00002-of-00002.gguf")
		var clock atomic.Int64
		now := func() time.Time { return time.Unix(clock.Load(), 0) }
		c := NewCache(0)
		read := c.read
		c.read = func(ctx context.Context, path string) (Info, error) {
			in, err := read(ctx, path)
			if strings.EqualFold(filepath.Base(path), "s-00001-of-00002.gguf") {
				clock.Store(30) // чтение закончилось ровно на сроке
			}
			return in, err
		}
		one := l
		one.Workers = 1
		res := scanOK(t, NewScanner(c, one, now), dir)
		if got := paths(res); !slices.Equal(got, []string{m}) || !res.Truncated || len(res.Errors) != 0 {
			t.Fatalf("models %q, errors %v, truncated %v", got, res.Errors, res.Truncated)
		}
	})
}

// F4-PM1: после срока корни больше не открываются — ошибку чтения даёт только корень до срока, поиск
// останавливается с truncated. Часы: каждый вызов — плюс 15 с, срок — 30 с: после первого корня часы на сроке.
func TestScanStopsOpeningRootsAfterDeadline(t *testing.T) {
	dir := t.TempDir()
	no1 := filepath.Join(dir, "no1")
	no2 := filepath.Join(dir, "no2")
	no3 := filepath.Join(dir, "no3")
	var clock atomic.Int64
	l := testLimits
	l.Timeout = 30 * time.Second
	s := NewScanner(NewCache(0), l, func() time.Time { return time.Unix(clock.Add(15), 0) })
	res := scanOK(t, s, no1, no2, no3)
	if !res.Truncated {
		t.Fatal("walk past the deadline must be truncated")
	}
	if len(res.Errors) != 1 || res.Errors[0].Path != no1 {
		t.Fatalf("errors %+v: roots after the deadline must not be opened", res.Errors)
	}
}

// F4-PM1: walker.dir после срока не собирает файлы и останавливает обход; срок проверяется перед открытием
// каталога и перед каждой порцией записей — пустой каталог после срока не читается.
func TestWalkerDirStopsAfterDeadline(t *testing.T) {
	root := t.TempDir()
	a := modelFile(t, filepath.Join(root, "a"), "m1.gguf")
	modelFile(t, filepath.Join(root, "b"), "m2.gguf") // после срока не собирается
	var clock atomic.Int64                            // 0 — до срока, 1 — срок истёк
	s := NewScanner(NewCache(0), testLimits, func() time.Time { return time.Unix(clock.Load(), 0) })
	w := &walker{s: s, deadline: time.Unix(1, 0)}
	w.dir(filepath.Join(root, "a"), 0)
	if len(w.files) != 1 || w.files[0] != a {
		t.Fatalf("before the deadline: files %q", w.files)
	}
	clock.Store(1)
	w.dir(filepath.Join(root, "b"), 0)
	if len(w.files) != 1 || w.files[0] != a {
		t.Fatalf("after the deadline: files %q", w.files)
	}
	if !w.stopped || !w.res.Truncated {
		t.Fatalf("stopped %v, truncated %v", w.stopped, w.res.Truncated)
	}
	// Срок истекает между открытием пустого каталога и его первой порцией записей: порция не читается, обход
	// останавливается. Записей нет, проверку на записях не пройти: снятие проверки перед порцией красит тест.
	var ticks atomic.Int64
	s2 := NewScanner(NewCache(0), testLimits, func() time.Time { return time.Unix(ticks.Add(1), 0) })
	w2 := &walker{s: s2, deadline: time.Unix(2, 0)}
	w2.dir(t.TempDir(), 0)
	if !w2.stopped || !w2.res.Truncated {
		t.Fatalf("empty dir on the deadline: stopped %v, truncated %v", w2.stopped, w2.res.Truncated)
	}
}

// Одновременно идёт один поиск: второй вызов ждёт результат первого, отменённый — только своё ожидание.
func TestScanSingleflight(t *testing.T) {
	root := t.TempDir()
	modelFile(t, root, "m.gguf")
	c := NewCache(0)
	gate := make(chan struct{})
	reads := countReads(c, gate)
	s := NewScanner(c, testLimits, nil)
	type out struct {
		res ScanResult
		err error
	}
	counts := func() (started, joined int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.started, s.joined
	}
	other := t.TempDir() // корни второго вызова не учитываются
	first, second := make(chan out, 1), make(chan out, 1)
	go func() { r, err := s.Scan(context.Background(), []string{root}); first <- out{r, err} }()
	waitFor(t, "first read", func() bool { return reads.Load() == 1 })
	go func() { r, err := s.Scan(context.Background(), []string{other}); second <- out{r, err} }()
	waitFor(t, "second scan to join", func() bool { _, j := counts(); return j == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Scan(ctx, []string{root}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	close(gate)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || len(a.res.Models) != 1 || len(b.res.Models) != 1 ||
		&a.res.Models[0] != &b.res.Models[0] {
		t.Fatalf("first %+v, second %+v", a, b)
	}
	if started, joined := counts(); started != 1 || joined != 2 {
		t.Fatalf("started %d, joined %d", started, joined)
	}
	scanOK(t, s, root) // поиск закончен — следующий вызов начинает новый, заголовок уже в кэше
	if started, _ := counts(); started != 2 || reads.Load() != 1 {
		t.Fatalf("started %d, reads %d", started, reads.Load())
	}
}
