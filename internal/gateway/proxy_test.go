package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/fakeupstream"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func upstreamFor(t *testing.T, h http.Handler) *scheduler.Upstream {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	up := scheduler.NewUpstream("a", u, 1, nil)
	t.Cleanup(func() { up.Retire(scheduler.RetireStop) })
	return up
}

func deadUpstream(t *testing.T) *scheduler.Upstream {
	t.Helper()
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", testutil.FreePort(t)))
	up := scheduler.NewUpstream("a", u, 1, nil)
	t.Cleanup(func() { up.Retire(scheduler.RetireStop) })
	return up
}

// proxyServer — как обработчик плана 2: выставляет X-Locallm-Wait-Ms и вызывает serveProxy.
func proxyServer(t *testing.T, up *scheduler.Upstream) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Locallm-Wait-Ms", "7")
		serveProxy(w, r, up)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func post(t *testing.T, u, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(u, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(resp *http.Response) []byte {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func readEvents(t *testing.T, br *bufio.Reader, n int) {
	t.Helper()
	for got := 0; got < n; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			got++
		}
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in time")
}

func decodeError(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Error == nil {
		t.Fatalf("not an error body: %s", body)
	}
	return v.Error
}

func TestWriteAPIError(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Length", "999")
	writeAPIError(rec, &apiError{Status: 409, Code: "model_cooldown", Type: "conflict_error", Message: `model "a" is <busy>`,
		Extra:   map[string]any{"loaded_model": "a", "retry_after_s": 20, "message": "must not override", "type": "must not override", "code": "must not override"},
		Headers: map[string]string{"Retry-After": "20"}})
	if rec.Code != 409 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Retry-After") != "20" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	if strings.Contains(rec.Body.String(), `\u003c`) {
		t.Fatalf("HTML escaping must be off: %s", rec.Body)
	}
	e := decodeError(t, rec.Body.Bytes())
	if e["message"] != `model "a" is <busy>` || e["type"] != "conflict_error" || e["code"] != "model_cooldown" ||
		e["loaded_model"] != "a" || e["retry_after_s"] != float64(20) {
		t.Fatalf("body %v", e)
	}
}

func TestUpstreamErrorConstructors(t *testing.T) {
	e := errUpstreamUnreachable("qwen", errors.New("dial tcp 127.0.0.1:8081: connectex: refused"))
	if e.Status != 502 || e.Code != "upstream_error" || e.Type != "server_error" ||
		e.Message != `llama-server for "qwen" is unreachable: dial tcp 127.0.0.1:8081: connectex: refused` {
		t.Fatalf("%+v", e)
	}
	e = errUpstreamExited("qwen")
	if e.Status != 502 || e.Code != "upstream_error" || e.Type != "server_error" || e.Message != `llama-server for "qwen" exited` {
		t.Fatalf("%+v", e)
	}
	e = errUpstreamAborted("qwen")
	if e.Status != 502 || e.Code != "upstream_error" || e.Type != "server_error" ||
		e.Message != `request aborted: model "qwen" was unloaded by the administrator` {
		t.Fatalf("%+v", e)
	}
	if e = errRetired(nil, "qwen"); e.Message != `llama-server for "qwen" exited` {
		t.Fatalf("nil upstream: %+v", e)
	}
	e = errHostPaused()
	if e.Status != 503 || e.Code != "host_paused" || e.Type != "server_error" || e.Headers["x-should-retry"] != "false" ||
		e.Message != "host is paused by the administrator; requests are not accepted until it is resumed" {
		t.Fatalf("%+v", e)
	}
}

func TestProxyRewritesRequestAndPassesResponse(t *testing.T) {
	type seen struct {
		auth, path, query, host string
		body                    []byte
		length                  int64
	}
	got := make(chan seen, 1)
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("Authorization"), r.URL.Path, r.URL.RawQuery, r.Host, b, r.ContentLength}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	body := []byte(`{"model":"a","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, proxyServer(t, up)+"/v1/chat/completions?x=1&y=2", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := readAll(resp)
	s := <-got
	if s.auth != "" {
		t.Fatal("Authorization reached llama-server")
	}
	if s.path != "/v1/chat/completions" || s.query != "x=1&y=2" || !bytes.Equal(s.body, body) || s.length != int64(len(body)) || s.host != up.URL.Host {
		t.Fatalf("upstream saw %+v", s)
	}
	if resp.StatusCode != 201 || string(b) != `{"ok":true}` || resp.Header.Get("X-Upstream") != "yes" || resp.Header.Get("X-Locallm-Wait-Ms") != "7" {
		t.Fatalf("client got %d %s %v", resp.StatusCode, b, resp.Header)
	}
}

func TestProxyPassesUpstreamErrorUnchanged(t *testing.T) {
	const llamaErr = `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, llamaErr)
	}))
	resp := post(t, proxyServer(t, up)+"/v1/chat/completions", `{"model":"a"}`)
	b := readAll(resp)
	if resp.StatusCode != 400 || string(b) != llamaErr || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%d %s %v", resp.StatusCode, b, resp.Header)
	}
}

func TestProxyStreamsEachEventImmediately(t *testing.T) {
	up := upstreamFor(t, fakeupstream.New(fakeupstream.Options{Chunks: 5, SSEInterval: 200 * time.Millisecond}, nil))
	start := time.Now()
	resp := post(t, proxyServer(t, up)+"/v1/chat/completions", `{"model":"a","stream":true}`)
	defer resp.Body.Close()
	var at []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			at = append(at, time.Since(start))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(at) != 6 {
		t.Fatalf("events %d, want 5 chunks + [DONE]", len(at))
	}
	if at[0] > 150*time.Millisecond {
		t.Fatalf("first event after %s: buffered", at[0])
	}
	for i := 1; i < 5; i++ {
		if d := at[i] - at[i-1]; d < 100*time.Millisecond {
			t.Fatalf("events %d and %d arrived %s apart: buffered", i-1, i, d)
		}
	}
}

func TestProxyClientCancelAbortsUpstream(t *testing.T) {
	log := &syncBuffer{}
	up := upstreamFor(t, fakeupstream.New(fakeupstream.Options{Chunks: 1000, SSEInterval: 20 * time.Millisecond}, log))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxyServer(t, up)+"/v1/chat/completions", strings.NewReader(`{"model":"a","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readEvents(t, bufio.NewReader(resp.Body), 3)
	cancel()
	resp.Body.Close()
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(log.String(), "aborted=true") })
}

func TestProxyRetireMidStreamCancelsUpstream(t *testing.T) {
	log := &syncBuffer{}
	up := upstreamFor(t, fakeupstream.New(fakeupstream.Options{Chunks: 1000, SSEInterval: 20 * time.Millisecond}, log))
	resp := post(t, proxyServer(t, up)+"/v1/chat/completions", `{"model":"a","stream":true}`)
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	readEvents(t, br, 3)
	up.Retire(scheduler.RetireStop)
	rest, _ := io.ReadAll(br) // соединение с клиентом обрывается
	if n := strings.Count(string(rest), "data: "); n > 100 {
		t.Fatalf("stream continued after Retire: %d more events", n)
	}
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(log.String(), "aborted=true") })
}

func TestProxyRetiredUpstreamAnswers502Exited(t *testing.T) {
	var hits atomic.Int32
	up := upstreamFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	up.Retire(scheduler.RetireExited)
	resp := post(t, proxyServer(t, up)+"/v1/chat/completions", `{"model":"a"}`)
	e := decodeError(t, readAll(resp))
	if resp.StatusCode != 502 || e["code"] != "upstream_error" || e["message"] != `llama-server for "a" exited` || hits.Load() != 0 {
		t.Fatalf("%d %v hits=%d", resp.StatusCode, e, hits.Load())
	}
}

func TestProxyUnreachableAnswers502(t *testing.T) {
	resp := post(t, proxyServer(t, deadUpstream(t))+"/v1/completions", `{"model":"a"}`)
	e := decodeError(t, readAll(resp))
	msg, _ := e["message"].(string)
	if resp.StatusCode != 502 || e["code"] != "upstream_error" || e["type"] != "server_error" || !strings.HasPrefix(msg, `llama-server for "a" is unreachable: `) {
		t.Fatalf("%d %v", resp.StatusCode, e)
	}
}

func TestProxyClientGoneWritesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"a"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	serveProxy(rec, req, deadUpstream(t))
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("wrote %d %q", rec.Code, rec.Body)
	}
}

func TestProxyClientCancelBeforeHeadersAbortsUpstream(t *testing.T) {
	started, aborted := make(chan struct{}), make(chan struct{})
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(aborted)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyServer(t, up)+"/v1/chat/completions", strings.NewReader(`{"model":"a","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("client error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client request did not stop")
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("non-streaming upstream request was not cancelled")
	}
}

func TestProxyRetireBeforeHeadersAnswers502Exited(t *testing.T) {
	started, aborted := make(chan struct{}), make(chan struct{})
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(aborted)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyServer(t, up)+"/v1/completions", strings.NewReader(`{"model":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		done <- result{resp, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upstream request did not start")
	}
	up.Retire(scheduler.RetireExited)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	e := decodeError(t, readAll(res.resp))
	if res.resp.StatusCode != 502 || e["message"] != `llama-server for "a" exited` {
		t.Fatalf("%d %v", res.resp.StatusCode, e)
	}
	select {
	case <-aborted:
	case <-ctx.Done():
		t.Fatal("retired upstream request was not cancelled")
	}
}
