package desktop

import (
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testInstanceName() string { return "locallm-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) }

// §11.5: имя — "locallm-host-" и 16 hex SHA-256 пути в нижнем регистре после filepath.Clean.
func TestInstanceName(t *testing.T) {
	a := InstanceName(`C:\Users\Me\AppData\Local\locallm\locallm.yaml`)
	b := InstanceName(`c:\users\me\appdata\local\locallm\.\LOCALLM.yaml`)
	c := InstanceName(`C:\Users\Me\AppData\Local\locallm\other.yaml`)
	if a != b || a == c || !regexp.MustCompile(`^locallm-host-[0-9a-f]{16}$`).MatchString(a) {
		t.Fatalf("names %q %q %q", a, b, c)
	}
}

// §11.5: второй захват — ERROR_ALREADY_EXISTS; просьба доходит до первого, ответ — до второго; повторно — тоже.
func TestInstanceSecondAsksFirst(t *testing.T) {
	name := testInstanceName()
	first, isFirst, err := AcquireInstance(name)
	if err != nil || !isFirst {
		t.Fatalf("first: %v %v", isFirst, err)
	}
	defer first.Close()
	var asked atomic.Int32
	go first.Serve(func() { asked.Add(1) })
	second, isFirst, err := AcquireInstance(name)
	if err != nil || isFirst {
		t.Fatalf("second: %v %v", isFirst, err)
	}
	defer second.Close()
	if !second.Ask(5*time.Second) || asked.Load() != 1 {
		t.Fatalf("first ask: asked %d", asked.Load())
	}
	if !second.Ask(5*time.Second) || asked.Load() != 2 {
		t.Fatalf("second ask: asked %d", asked.Load())
	}
}

// §11.5: первый не отвечает — Ask возвращает false не раньше срока.
func TestInstanceAskTimesOut(t *testing.T) {
	name := testInstanceName()
	first, _, err := AcquireInstance(name)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, _, err := AcquireInstance(name)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := time.Now()
	if second.Ask(200 * time.Millisecond) {
		t.Fatal("nobody serves the request, Ask must fail")
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("Ask gave up after %v", d)
	}
}

// Close останавливает Serve и освобождает имя; повторный Close ничего не делает.
func TestInstanceCloseStopsServeAndReleases(t *testing.T) {
	name := testInstanceName()
	first, _, err := AcquireInstance(name)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { first.Serve(func() {}); close(done) }()
	first.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
	first.Close()
	again, isFirst, err := AcquireInstance(name)
	if err != nil || !isFirst {
		t.Fatalf("after Close the name must be free: first %v, err %v", isFirst, err)
	}
	again.Close()
}
