package gguf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countReads подменяет чтение кэша: считает вызовы; gate != nil — чтение ждёт его закрытия.
func countReads(c *Cache, gate <-chan struct{}) *atomic.Int32 {
	n := new(atomic.Int32)
	read := c.read
	c.read = func(ctx context.Context, path string) (Info, error) {
		n.Add(1)
		if gate != nil {
			<-gate
		}
		return read(ctx, path)
	}
	return n
}

// waitFor ждёт условия до 5 с.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(end) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func modelFile(t *testing.T, dir, name string) string {
	t.Helper()
	return newFile(3).str("general.architecture", "llama").str("general.name", name).write(t, dir, name)
}

func getOK(t *testing.T, c *Cache, p string) Info {
	t.Helper()
	in, err := c.Get(context.Background(), p)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return in
}

// Разные написания одного файла — один ключ; одновременные Get ждут одно чтение (§8.3).
func TestCacheReadsFileOnce(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	gate := make(chan struct{})
	reads := countReads(c, gate)
	paths := []string{p, strings.ToUpper(p), filepath.Join(filepath.Dir(p), ".", "M.GGUF")}
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := c.Get(context.Background(), paths[i%len(paths)])
			if err == nil && in.Kind != "model" {
				err = fmt.Errorf("kind %q", in.Kind)
			}
			errs <- err
		}()
	}
	waitFor(t, "first read", func() bool { return reads.Load() > 0 })
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("reads %d, want 1", n)
	}
	if c.Len() != 1 {
		t.Fatalf("Len %d", c.Len())
	}
}

// Новый размер или время записи — новый ключ и новое чтение.
func TestCacheKeyChangesWithFile(t *testing.T) {
	dir := t.TempDir()
	p := modelFile(t, dir, "m.gguf")
	c := NewCache(0)
	reads := countReads(c, nil)
	getOK(t, c, p)
	getOK(t, c, p)
	if n := reads.Load(); n != 1 {
		t.Fatalf("same file: reads %d", n)
	}
	newFile(3).str("general.architecture", "llama").str("general.name", "longer name").write(t, dir, "m.gguf")
	if in := getOK(t, c, p); in.Name != "longer name" || reads.Load() != 2 {
		t.Fatalf("new size: %+v, reads %d", in, reads.Load())
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	mt := st.ModTime().Add(-time.Hour)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	getOK(t, c, p)
	if n := reads.Load(); n != 3 {
		t.Fatalf("new mtime: reads %d", n)
	}
}

// Сверх лимита вытесняется самая старая по добавлению запись; попадание порядок не меняет: повторный Get a
// не спасает его от вытеснения — тест отличает вытеснение по добавлению от LRU (MoveToBack на попадании красит его).
func TestCacheEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	a, b, x := modelFile(t, dir, "a.gguf"), modelFile(t, dir, "b.gguf"), modelFile(t, dir, "x.gguf")
	c := NewCache(2)
	reads := countReads(c, nil)
	getOK(t, c, a) // добавление a
	getOK(t, c, b) // добавление b
	getOK(t, c, a) // попадание: порядок добавления не меняется
	getOK(t, c, x) // добавление x вытесняет a — самого старого по добавлению, а не b
	if n := reads.Load(); n != 3 || c.Len() != 2 {
		t.Fatalf("reads %d, Len %d", n, c.Len())
	}
	getOK(t, c, a) // a вытеснен, несмотря на недавнее попадание, — читается снова, вытесняет b
	if n := reads.Load(); n != 4 {
		t.Fatalf("after a: reads %d", n)
	}
	getOK(t, c, b) // b вытеснен — читается снова
	if n := reads.Load(); n != 5 || c.Len() != 2 {
		t.Fatalf("after b: reads %d, Len %d", n, c.Len())
	}
}

// Ошибка чтения не кэшируется.
func TestCacheDoesNotCacheErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.gguf")
	if err := os.WriteFile(p, []byte("NOPE and more bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCache(0)
	reads := countReads(c, nil)
	for range 2 {
		if _, err := c.Get(context.Background(), p); !errors.Is(err, ErrNotGGUF) {
			t.Fatalf("err %v", err)
		}
	}
	if n := reads.Load(); n != 2 || c.Len() != 0 {
		t.Fatalf("reads %d, Len %d", n, c.Len())
	}
}

// Отмена ctx прекращает ожидание; начатое чтение завершается и попадает в кэш.
func TestCacheGetCancelled(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	gate := make(chan struct{})
	reads := countReads(c, gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, p)
		done <- err
	}()
	waitFor(t, "read", func() bool { return reads.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	close(gate)
	if in := getOK(t, c, p); in.Kind != "model" || reads.Load() != 1 {
		t.Fatalf("%+v, reads %d", in, reads.Load())
	}
}

// Ошибки Get не содержат пути (§5.5 п. 14): путь может прийти из значения --mmproj.
func TestCacheErrorsHaveNoPath(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.gguf")
	if err := os.WriteFile(bad, []byte("GGUF"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCache(0)
	for _, p := range []string{filepath.Join(dir, "missing.gguf"), bad, dir} {
		_, err := c.Get(context.Background(), p)
		if err == nil {
			t.Fatalf("%s: no error", p)
		}
		if msg := err.Error(); strings.Contains(strings.ToLower(msg), strings.ToLower(filepath.Base(dir))) ||
			strings.Contains(msg, ".gguf") {
			t.Fatalf("error contains the path: %q", msg)
		}
	}
}

// testClock — подменяемые часы кэша.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (k *testClock) now() time.Time      { k.mu.Lock(); defer k.mu.Unlock(); return k.t }
func (k *testClock) add(d time.Duration) { k.mu.Lock(); k.t = k.t.Add(d); k.mu.Unlock() }

// useClock ставит кэшу часы теста (15 ноября 2026, 12:00 UTC).
func useClock(c *Cache) *testClock {
	k := &testClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	c.now = k.now
	return k
}

// partialReads подменяет чтение: Info с Partial, Name — номер чтения ("1", "2", …); счётчик — число чтений.
func partialReads(c *Cache) *atomic.Int32 {
	n := new(atomic.Int32)
	c.read = func(context.Context, string) (Info, error) {
		return Info{Kind: "model", Name: fmt.Sprint(n.Add(1)), Partial: true}, nil
	}
	return n
}

// readyWithin — Ready, который вернулся быстрее 1 с (не ждёт чтения файла).
func readyWithin(t *testing.T, c *Cache, p string) (Info, bool) {
	t.Helper()
	type result struct {
		info Info
		ok   bool
	}
	done := make(chan result, 1)
	go func() {
		in, ok := c.Ready(p)
		done <- result{in, ok}
	}()
	select {
	case r := <-done:
		return r.info, r.ok
	case <-time.After(time.Second):
		t.Fatal("Ready waits for the file")
		return Info{}, false
	}
}

// idle ждёт конца фоновой проверки строки p.
func idle(t *testing.T, c *Cache, p string) {
	t.Helper()
	waitFor(t, "background check of "+p, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return !c.paths[p].busy
	})
}

// §8.3: Ready не ждёт чтения. Первый вызов — ok == false и одно фоновое чтение; вызовы, пока чтение идёт, второго не
// начинают; после чтения — Info.
func TestCacheReadyDoesNotWait(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	useClock(c)
	gate := make(chan struct{})
	reads := countReads(c, gate)
	if _, ok := readyWithin(t, c, p); ok {
		t.Fatal("first Ready: ok")
	}
	waitFor(t, "background read", func() bool { return reads.Load() == 1 })
	for range 3 {
		if _, ok := readyWithin(t, c, p); ok {
			t.Fatal("Ready during the read: ok")
		}
	}
	close(gate)
	idle(t, c, p)
	in, ok := readyWithin(t, c, p)
	if !ok || in.Name != "m.gguf" || reads.Load() != 1 {
		t.Fatalf("after the read: %+v %v, reads %d", in, ok, reads.Load())
	}
}

// После Get той же строки Ready сразу отдаёт Info: без чтения и без фоновой проверки.
func TestCacheReadyAfterGet(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	useClock(c)
	reads := countReads(c, nil)
	getOK(t, c, p)
	in, ok := readyWithin(t, c, p)
	c.mu.Lock()
	busy := c.paths[p].busy
	c.mu.Unlock()
	if !ok || in.Name != "m.gguf" || busy || reads.Load() != 1 {
		t.Fatalf("Ready: %+v %v, busy %v, reads %d", in, ok, busy, reads.Load())
	}
}

// Изменённый файл: до ReadyRecheck — прежний Info без проверки; в ReadyRecheck — прежний Info и одна фоновая
// проверка; после неё — новый Info.
func TestCacheReadyRechecksChangedFile(t *testing.T) {
	dir := t.TempDir()
	p := modelFile(t, dir, "m.gguf")
	c := NewCache(0)
	clock := useClock(c)
	reads := countReads(c, nil)
	getOK(t, c, p)
	newFile(3).str("general.architecture", "llama").str("general.name", "replaced file").write(t, dir, "m.gguf")
	clock.add(ReadyRecheck - time.Second)
	if in, ok := readyWithin(t, c, p); !ok || in.Name != "m.gguf" {
		t.Fatalf("before the recheck: %+v %v", in, ok)
	}
	idle(t, c, p)
	if n := reads.Load(); n != 1 {
		t.Fatalf("before the recheck: reads %d", n)
	}
	clock.add(time.Second)
	if in, ok := readyWithin(t, c, p); !ok || in.Name != "m.gguf" {
		t.Fatalf("at the recheck: %+v %v", in, ok)
	}
	waitFor(t, "new Info", func() bool {
		in, ok := c.Ready(p)
		return ok && in.Name == "replaced file"
	})
	idle(t, c, p)
	if n := reads.Load(); n != 2 {
		t.Fatalf("after the recheck: reads %d", n)
	}
}

// F4-D2: запись Partial живёт PartialTTL от конца чтения, потом файл читается заново — и через Get, и через Ready;
// новая запись заменяет старую. Полная запись не перечитывается.
func TestCachePartialExpires(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	clock := useClock(c)
	reads := partialReads(c)
	if in := getOK(t, c, p); in.Name != "1" {
		t.Fatalf("first Get: %+v", in)
	}
	clock.add(PartialTTL - time.Second)
	if in := getOK(t, c, p); in.Name != "1" || reads.Load() != 1 {
		t.Fatalf("before PartialTTL: %+v, reads %d", in, reads.Load())
	}
	clock.add(time.Second)
	if in := getOK(t, c, p); in.Name != "2" || reads.Load() != 2 || c.Len() != 1 {
		t.Fatalf("at PartialTTL: %+v, reads %d, Len %d", in, reads.Load(), c.Len())
	}
	clock.add(PartialTTL)
	if in, ok := readyWithin(t, c, p); !ok || in.Name != "2" {
		t.Fatalf("Ready at PartialTTL: %+v %v", in, ok)
	}
	waitFor(t, "re-read by Ready", func() bool {
		in, ok := c.Ready(p)
		return ok && in.Name == "3"
	})
	idle(t, c, p)
	if n := reads.Load(); n != 3 || c.Len() != 1 {
		t.Fatalf("after Ready: reads %d, Len %d", n, c.Len())
	}

	full := NewCache(0)
	fullClock := useClock(full)
	fullReads := countReads(full, nil)
	getOK(t, full, p)
	fullClock.add(10 * PartialTTL)
	getOK(t, full, p)
	if n := fullReads.Load(); n != 1 {
		t.Fatalf("full Info re-read: reads %d", n)
	}
}

// Файл не GGUF: Ready — ok == false; новое чтение — не раньше ReadyRecheck, а не на каждый опрос.
func TestCacheReadyRetriesFailedReadAfterRecheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.gguf")
	if err := os.WriteFile(p, []byte("NOPE and more bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCache(0)
	clock := useClock(c)
	reads := countReads(c, nil)
	for range 5 {
		if _, ok := readyWithin(t, c, p); ok {
			t.Fatal("bad file: ok")
		}
		idle(t, c, p)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("before the recheck: reads %d", n)
	}
	clock.add(ReadyRecheck)
	if _, ok := readyWithin(t, c, p); ok {
		t.Fatal("bad file after the recheck: ok")
	}
	idle(t, c, p)
	if n := reads.Load(); n != 2 {
		t.Fatalf("after the recheck: reads %d", n)
	}
}

// Файла нет — ok == false без чтений.
func TestCacheReadyMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.gguf")
	c := NewCache(0)
	useClock(c)
	reads := countReads(c, nil)
	for range 3 {
		if _, ok := readyWithin(t, c, p); ok {
			t.Fatal("missing file: ok")
		}
		idle(t, c, p)
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("reads %d", n)
	}
}

// Удалённый файл: до проверки — прежний Info, после неё — ok == false.
func TestCacheReadyForgetsDeletedFile(t *testing.T) {
	p := modelFile(t, t.TempDir(), "m.gguf")
	c := NewCache(0)
	clock := useClock(c)
	getOK(t, c, p)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	clock.add(ReadyRecheck)
	if in, ok := readyWithin(t, c, p); !ok || in.Name != "m.gguf" {
		t.Fatalf("before the check: %+v %v", in, ok)
	}
	idle(t, c, p)
	if _, ok := readyWithin(t, c, p); ok {
		t.Fatal("deleted file: ok")
	}
}

// Таблица путей не больше предела кэша: новая строка в полной таблице сначала её очищает.
func TestCachePathsBounded(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(2)
	useClock(c)
	for i := range 4 {
		getOK(t, c, modelFile(t, dir, fmt.Sprintf("m%d.gguf", i)))
		c.mu.Lock()
		n := len(c.paths)
		c.mu.Unlock()
		if n > 2 {
			t.Fatalf("after %d files: %d paths", i+1, n)
		}
	}
}
