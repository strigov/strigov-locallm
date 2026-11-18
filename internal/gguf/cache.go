package gguf

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
)

// CacheSize — предел записей кэша (§8.3).
const CacheSize = 4096

// PartialTTL — сколько живёт запись с Info.Partial (чтение остановлено лимитом §8.1, например сроком 2 с на медленном
// диске) от конца чтения; потом файл читается заново (решение F4-D2). Полная запись живёт до изменения файла.
const PartialTTL = time.Minute

// ReadyRecheck — не чаще чем раз в столько Ready проверяет на диске файл, о котором его спрашивают.
const ReadyRecheck = time.Minute

// Cache — Info по ключу (канонический путь, размер, время записи) (§8.3). Не больше limit записей; при переполнении
// вытесняются самые старые по времени добавления. Одновременные Get одного ключа ждут одно чтение. Таблица paths
// связывает строку пути из Get и Ready с последним ключом этой строки: по ней Ready отвечает без обращения к диску.
type Cache struct {
	limit int
	read  func(ctx context.Context, path string) (Info, error) // ReadInfo; тесты подменяют
	now   func() time.Time                                     // time.Now; тесты подменяют

	mu       sync.Mutex
	entries  map[cacheKey]*list.Element // значения — *cacheEntry
	order    *list.List                 // от старых к новым
	inflight map[cacheKey]*cacheCall
	paths    map[string]pathState // не больше limit строк
}

type cacheKey struct {
	path  string // канонический
	size  int64
	mtime int64 // UnixNano
}

type cacheEntry struct {
	key  cacheKey
	info Info
	at   time.Time // конец чтения
}

type cacheCall struct {
	done chan struct{}
	info Info
	err  error
}

// pathState — что известно о строке пути: ключ последнего успешного os.Stat (known), время последней проверки и
// busy — идёт фоновая проверка Ready.
type pathState struct {
	key     cacheKey
	known   bool
	checked time.Time
	busy    bool
}

// NewCache — кэш на size записей; size <= 0 — CacheSize.
func NewCache(size int) *Cache {
	if size <= 0 {
		size = CacheSize
	}
	return &Cache{limit: size, read: ReadInfo, now: time.Now, entries: map[cacheKey]*list.Element{}, order: list.New(),
		inflight: map[cacheKey]*cacheCall{}, paths: map[string]pathState{}}
}

// Get — Info файла из кэша или чтением (§8.3). Изменённый файл (размер или время записи) и запись Partial старше
// PartialTTL читаются заново. Отмена ctx прекращает только ожидание: начатое чтение завершится и попадёт в кэш.
// Ошибки не кэшируются и не содержат пути. Строка path запоминается для Ready.
func (c *Cache) Get(ctx context.Context, path string) (Info, error) {
	canon, err := config.Canonical(path)
	if err != nil {
		c.forget(path)
		return Info{}, err
	}
	st, err := os.Stat(canon)
	if err != nil {
		c.forget(path)
		return Info{}, fmt.Errorf("stat: %w", unwrapPath(err))
	}
	key := cacheKey{path: canon, size: st.Size(), mtime: st.ModTime().UnixNano()}
	now := c.now()
	c.mu.Lock()
	ps := c.paths[path]
	ps.key, ps.known, ps.checked = key, true, now
	c.setPath(path, ps)
	if el, ok := c.entries[key]; ok && fresh(el.Value.(*cacheEntry), now) {
		c.mu.Unlock()
		return el.Value.(*cacheEntry).info, nil
	}
	call, ok := c.inflight[key]
	if !ok {
		call = &cacheCall{done: make(chan struct{})}
		c.inflight[key] = call
		go c.fill(key, call)
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.info, call.err
	case <-ctx.Done():
		return Info{}, ctx.Err()
	}
}

// fill — одно чтение ключа; у чтения свои лимиты (§8.1), ctx вызывающего его не отменяет. Новая запись заменяет
// просроченную запись того же ключа и встаёт в конец порядка вытеснения.
func (c *Cache) fill(key cacheKey, call *cacheCall) {
	call.info, call.err = c.read(context.Background(), key.path)
	at := c.now()
	c.mu.Lock()
	delete(c.inflight, key)
	if call.err == nil {
		if old, ok := c.entries[key]; ok {
			c.order.Remove(old)
		}
		c.entries[key] = c.order.PushBack(&cacheEntry{key: key, info: call.info, at: at})
		for c.order.Len() > c.limit {
			old := c.order.Front()
			c.order.Remove(old)
			delete(c.entries, old.Value.(*cacheEntry).key)
		}
	}
	c.mu.Unlock()
	close(call.done)
}

// fresh — запись годна: полная или Partial моложе PartialTTL (F4-D2).
func fresh(e *cacheEntry, now time.Time) bool {
	return !e.info.Partial || now.Sub(e.at) < PartialTTL
}

// Ready — Info файла path без обращения к диску (§8.3: обработчики /models никогда не ждут файла). ok — у строки
// path есть запись в кэше (от последнего Get этой строки или фоновой проверки). Если строку ещё не проверяли или
// проверяли ReadyRecheck назад и раньше, Ready запускает одну фоновую проверку (Get); её итог видят следующие вызовы.
func (c *Cache) Ready(path string) (Info, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	ps := c.paths[path]
	var info Info
	ok := false
	if ps.known {
		if el, hit := c.entries[ps.key]; hit {
			info, ok = el.Value.(*cacheEntry).info, true
		}
	}
	if !ps.busy && (ps.checked.IsZero() || now.Sub(ps.checked) >= ReadyRecheck) {
		ps.busy = true
		c.setPath(path, ps)
		go c.refresh(path)
	}
	return info, ok
}

// refresh — фоновая проверка Ready: Get строки path (ошибка оставляет строку без записи), затем отметка времени.
func (c *Cache) refresh(path string) {
	_, _ = c.Get(context.Background(), path)
	now := c.now()
	c.mu.Lock()
	ps := c.paths[path]
	ps.busy, ps.checked = false, now
	c.setPath(path, ps)
	c.mu.Unlock()
}

// forget — файла по строке path нет (Canonical или os.Stat не прошли): Ready больше не отдаёт его запись.
func (c *Cache) forget(path string) {
	c.mu.Lock()
	if ps, ok := c.paths[path]; ok {
		ps.known = false
		c.paths[path] = ps
	}
	c.mu.Unlock()
}

// setPath — запись строки пути под c.mu. Новая строка в полной таблице (limit строк) сначала очищает таблицу: поиск
// по большим каталогам не растит её без предела, а забытые строки Ready проверит заново.
func (c *Cache) setPath(path string, ps pathState) {
	if _, ok := c.paths[path]; !ok && len(c.paths) >= c.limit {
		clear(c.paths)
	}
	c.paths[path] = ps
}

// Len — число записей.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
