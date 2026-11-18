package scheduler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

func leaseUpstream(t *testing.T) *Upstream {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	up := NewUpstream("a", u, 1, nil)
	t.Cleanup(func() { up.Retire(RetireStop) })
	return up
}

func TestLeaseValidUntilRetire(t *testing.T) {
	up := leaseUpstream(t)
	l := &Lease{Model: "a", Gen: 1, Upstream: up}
	if !l.Valid() || l.Context().Err() != nil {
		t.Fatal("fresh lease must be valid")
	}
	up.Retire(RetireStop)
	if l.Valid() {
		t.Fatal("Lease.Valid() must be false after Retire")
	}
	if l.Context().Err() == nil {
		t.Fatal("lease context must be cancelled by Retire")
	}
}

func TestLeaseWithoutUpstreamIsInvalid(t *testing.T) {
	l := &Lease{Model: "a"}
	if l.Valid() || l.Context().Err() == nil {
		t.Fatal("lease without upstream must be invalid and cancelled")
	}
}

func TestLeaseReleaseRunsOnce(t *testing.T) {
	var n atomic.Int32
	l := &Lease{release: func() { n.Add(1) }}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Release() }()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("release ran %d times", n.Load())
	}
	(&Lease{}).Release() // без функции — не паникует
}
