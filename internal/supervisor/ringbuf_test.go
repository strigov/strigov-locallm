package supervisor

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestRingBufferWrapsAndKeepsLast(t *testing.T) {
	b := newRingBuffer(3)
	for i := 1; i <= 5; i++ {
		b.Add(fmt.Sprint(i))
	}
	if got := b.Last(10); !reflect.DeepEqual(got, []string{"3", "4", "5"}) {
		t.Fatalf("last %v", got)
	}
	if got := b.Last(2); !reflect.DeepEqual(got, []string{"4", "5"}) {
		t.Fatalf("last2 %v", got)
	}
}

func TestRingBufferTruncatesLongLines(t *testing.T) {
	b := newRingBuffer(2)
	b.Add(strings.Repeat("я", 2000)) // 4000 байт
	if got := b.Last(1)[0]; len(got) > maxLineBytes || !strings.HasPrefix(got, "яя") {
		t.Fatalf("len %d", len(got))
	}
}

func TestRingBufferSinceLimitsLinesAndBytes(t *testing.T) {
	b := newRingBuffer(100)
	b.Add("old run line")
	mark := b.Mark()
	for i := 0; i < 30; i++ {
		b.Add(fmt.Sprintf("line %02d", i))
	}
	tail := b.Since(mark, 20, 4096)
	if len(tail) != 20 || tail[0] != "line 10" || tail[19] != "line 29" {
		t.Fatalf("tail %v", tail)
	}
	big := newRingBuffer(100)
	m := big.Mark()
	for i := 0; i < 20; i++ {
		big.Add(strings.Repeat("x", 1000))
	}
	if got := big.Since(m, 20, 4096); len(got) != 4 {
		t.Fatalf("4 KiB cap: got %d lines", len(got))
	}
	if got := b.Since(b.Mark(), 20, 4096); len(got) != 0 {
		t.Fatalf("nothing after mark, got %v", got)
	}
}
