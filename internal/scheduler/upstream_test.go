package scheduler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportSettings(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	up := NewUpstream("a", u, 7, nil)
	tr := up.Transport
	if tr.Proxy != nil || !tr.DisableCompression || tr.ResponseHeaderTimeout != 0 || tr.MaxIdleConnsPerHost != 4 {
		t.Fatalf("transport %+v", tr)
	}
	if up.Model != "a" || up.PID != 7 || up.URL != u || up.Retired() || up.Context().Err() != nil {
		t.Fatalf("upstream %+v", up)
	}
}

func TestRetireBlocksNewConnections(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	up := NewUpstream("a", u, 1, nil)
	// Без keep-alive второй запрос гарантированно идёт через dial (проверка Retire),
	// а не через соединение, которое readLoop мог ещё не вернуть в пул к моменту
	// CloseIdleConnections внутри Retire.
	up.Transport.DisableKeepAlives = true
	client := &http.Client{Transport: up.Transport}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	up.Retire(RetireStop)
	if up.Context().Err() == nil || !up.Retired() {
		t.Fatal("Retire must cancel Context and set Retired")
	}
	if _, err := client.Get(srv.URL + "/again"); !errors.Is(err, errUpstreamRetired) {
		t.Fatalf("after Retire: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits %d, want 1", hits.Load())
	}
}

func TestRetireDuringDialClosesConnection(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		once.Do(func() { close(entered) })
		<-release
		return c, err
	}
	up := newUpstream("a", u, 1, nil, dial)
	errc := make(chan error, 1)
	go func() {
		_, err := (&http.Client{Transport: up.Transport}).Get(srv.URL)
		errc <- err
	}()
	<-entered
	up.Retire(RetireStop)
	close(release)
	if err := <-errc; !errors.Is(err, errUpstreamRetired) {
		t.Fatalf("want errUpstreamRetired, got %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("request reached the server %d times", hits.Load())
	}
}

func TestRetireIsIdempotent(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	up := NewUpstream("a", u, 1, nil)
	if up.RetireReason() != 0 {
		t.Fatalf("fresh upstream reason %d", up.RetireReason())
	}
	up.Retire(RetireExited)
	up.Retire(RetireForced) // причина первого вызова сохраняется
	select {
	case <-up.Context().Done():
	default:
		t.Fatal("context not cancelled")
	}
	if up.RetireReason() != RetireExited || !up.Retired() {
		t.Fatalf("reason %d, want RetireExited", up.RetireReason())
	}
	up2 := NewUpstream("a", u, 2, nil)
	if up2.Retire(0); up2.RetireReason() != RetireStop {
		t.Fatalf("Retire(0): reason %d, want RetireStop", up2.RetireReason())
	}
}

// Отмена Context публикует причину даже наблюдателю в другой горутине.
func TestRetireReasonVisibleOnCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   RetireReason
		want RetireReason
	}{
		{"stop", RetireStop, RetireStop},
		{"exited", RetireExited, RetireExited},
		{"forced", RetireForced, RetireForced},
		{"zero", 0, RetireStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := url.Parse("http://127.0.0.1:1")
			up := NewUpstream("a", u, 1, nil)
			seen := make(chan RetireReason, 1)
			go func() {
				<-up.Context().Done()
				seen <- up.RetireReason()
			}()
			up.Retire(tc.in)
			up.Retire(RetireExited)
			if got := <-seen; got != tc.want || up.RetireReason() != tc.want {
				t.Fatalf("reason on cancellation %d, final %d, want %d", got, up.RetireReason(), tc.want)
			}
		})
	}
}
