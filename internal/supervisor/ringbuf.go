package supervisor

import (
	"strings"
	"sync"
)

const maxLineBytes = 2048 // строка длиннее 2 КиБ обрезается (§7.5)

// ringBuffer — последние N строк вывода одной модели за всё время жизни хоста.
type ringBuffer struct {
	mu    sync.Mutex
	lines []string
	next  int
	n     int
	total uint64
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &ringBuffer{lines: make([]string, capacity)}
}

func (b *ringBuffer) Add(line string) {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes]
	}
	line = strings.ToValidUTF8(line, "")
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines[b.next] = line
	b.next = (b.next + 1) % len(b.lines)
	if b.n < len(b.lines) {
		b.n++
	}
	b.total++
}

// Mark — номер следующей строки; Since(mark) вернёт строки, добавленные после него.
func (b *ringBuffer) Mark() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *ringBuffer) Last(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastLocked(n)
}

// Since — не больше n последних строк после mark, суммарно не больше maxBytes.
func (b *ringBuffer) Since(mark uint64, n, maxBytes int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	avail := int(b.total - mark)
	if avail < n {
		n = avail
	}
	lines := b.lastLocked(n)
	size, start := 0, len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		size += len(lines[i]) + 1
		if size > maxBytes {
			break
		}
		start = i
	}
	return lines[start:]
}

func (b *ringBuffer) lastLocked(n int) []string {
	if n > b.n {
		n = b.n
	}
	if n < 0 {
		n = 0
	}
	out := make([]string, n)
	start := (b.next - n + len(b.lines)) % len(b.lines)
	for i := 0; i < n; i++ {
		out[i] = b.lines[(start+i)%len(b.lines)]
	}
	return out
}
