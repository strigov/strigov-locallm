package supervisor

import (
	"reflect"
	"testing"

	"github.com/strigov/strigov-locallm/internal/config"
)

// §6.8 спецификации v2: буферы удалённых имён забываются, буфер работающего процесса — нет.
func TestSupervisorForgetLogs(t *testing.T) {
	s := New(&config.Config{Llama: config.Llama{LogLines: 100}}, nil)
	for _, id := range []string{"a", "b", "c"} {
		s.bufs[id] = newRingBuffer(100)
		s.bufs[id].Add("line " + id)
	}
	s.last = "b"
	s.cur = &proc{model: "c"}
	s.ForgetLogs([]string{"b", "c", "zzz"})
	if _, ok := s.Logs("b", 10); ok {
		t.Fatal("b kept")
	}
	if _, ok := s.Logs("", 10); ok {
		t.Fatal("last model kept")
	}
	if l, ok := s.Logs("c", 10); !ok || !reflect.DeepEqual(l, []string{"line c"}) {
		t.Fatalf("running c: %q %v", l, ok)
	}
	if _, ok := s.Logs("a", 10); !ok {
		t.Fatal("a forgotten")
	}
}
