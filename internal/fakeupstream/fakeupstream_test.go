package fakeupstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestParseFlagsDefaults(t *testing.T) {
	c, err := ParseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.Options.Chunks != 20 || c.Options.Status != 200 || c.Options.ModelID != "fake" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestParseFlagsLlamaCompat(t *testing.T) {
	c, err := ParseFlags([]string{"--loading", "2s", "-m", `C:\x\my model.gguf`, "--host", "127.0.0.1", "--port", "9123"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9123" || c.Options.ModelID != "my model" || c.Options.Loading != 2*time.Second {
		t.Fatalf("got %+v", c)
	}
}

func TestParseFlagsUnknownFlagIsError(t *testing.T) {
	var errOut bytes.Buffer
	_, err := ParseFlags([]string{"--bogus"}, &errOut)
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if got := errOut.String(); strings.Count(got, "flag provided but not defined: -bogus") != 1 ||
		strings.Count(got, "Usage of fakeupstream:") != 1 {
		t.Fatalf("expected one error and one usage message, got %q", got)
	}
}

func TestParseFlagsValidationErrorsArePrinted(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unexpected argument", []string{"extra"}, `unexpected arguments: ["extra"]`},
		{"invalid listen", []string{"--listen", "invalid"}, "--listen: address invalid: missing port in address"},
		{"zero chunks", []string{"--chunks", "0"}, "--chunks must be >= 1"},
		{"negative chunks", []string{"--chunks", "-1"}, "--chunks must be >= 1"},
		{"status below range", []string{"--status", "99"}, "--status must be in 100..599"},
		{"status above range", []string{"--status", "600"}, "--status must be in 100..599"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var errOut bytes.Buffer
			_, err := ParseFlags(c.args, &errOut)
			if err == nil || err.Error() != c.want {
				t.Fatalf("error=%v, want %q", err, c.want)
			}
			if got, want := errOut.String(), "fakeupstream: "+c.want+"\n"; got != want {
				t.Fatalf("diagnostic=%q, want %q", got, want)
			}
		})
	}
}

func TestParseFlagsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var errOut bytes.Buffer
			_, err := ParseFlags([]string{arg}, &errOut)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("error=%v, want flag.ErrHelp", err)
			}
			if got := errOut.String(); strings.Count(got, "Usage of fakeupstream:") != 1 || strings.HasPrefix(got, "fakeupstream:") {
				t.Fatalf("expected help without an error diagnostic, got %q", got)
			}
		})
	}
}

func TestHealthModelsAndRequestLog(t *testing.T) {
	log := &syncBuffer{}
	srv := httptest.NewServer(New(Options{ModelID: "m1"}, log))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"status":"ok"}` {
		t.Fatalf("health: %d %s", resp.StatusCode, b)
	}
	resp, err = http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"id":"m1"`) {
		t.Fatalf("models: %s", b)
	}
	if !strings.Contains(log.String(), "GET /health 200") {
		t.Fatalf("request log: %q", log.String())
	}
}

func TestLoadingAnswers503OnAllPaths(t *testing.T) {
	srv := httptest.NewServer(New(Options{Loading: 300 * time.Millisecond}, io.Discard))
	defer srv.Close()
	for _, p := range []string{"/health", "/v1/models"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 503 || string(b) != loadingBody {
			t.Fatalf("%s during loading: %d %s", p, resp.StatusCode, b)
		}
	}
	time.Sleep(350 * time.Millisecond)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after loading: %d", resp.StatusCode)
	}
}

func TestDelayHeadersAndJSONResponse(t *testing.T) {
	srv := httptest.NewServer(New(Options{DelayHeaders: 200 * time.Millisecond, Chunks: 3}, io.Discard))
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Fatalf("headers came after %s, want >= 200ms", el)
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "tok1 tok2 tok3" {
		t.Fatalf("body: %+v", out)
	}
}

func TestSSEChunksAndInterval(t *testing.T) {
	srv := httptest.NewServer(New(Options{SSEInterval: 30 * time.Millisecond, Chunks: 5}, io.Discard))
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", strings.NewReader(`{"model":"fake","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	events, done := 0, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			done = true
		} else if strings.HasPrefix(line, "data: {") {
			events++
		}
	}
	if events != 5 || !done {
		t.Fatalf("events=%d done=%v", events, done)
	}
	if el := time.Since(start); el < 110*time.Millisecond {
		t.Fatalf("stream took %s, want >= 4 intervals", el)
	}
}

func TestForcedStatusHeaders(t *testing.T) {
	srv := httptest.NewServer(New(Options{Status: 409, RetryAfter: 5, ShouldRetry: "false"}, io.Discard))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 || resp.Header.Get("Retry-After") != "5" || resp.Header.Get("x-should-retry") != "false" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	var e struct {
		Error struct{ Code int } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != 409 {
		t.Fatalf("error body code %d", e.Error.Code)
	}
	h, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	h.Body.Close()
	if h.StatusCode != 200 {
		t.Fatalf("--status must not affect /health, got %d", h.StatusCode)
	}
}

func TestClientAbortIsLogged(t *testing.T) {
	log := &syncBuffer{}
	srv := httptest.NewServer(New(Options{SSEInterval: 50 * time.Millisecond, Chunks: 50}, log))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(log.String(), "aborted=true") })
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func runWithExit(t *testing.T, o Options) (int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codes := make(chan int, 1)
	errOut := &syncBuffer{}
	addr := freeAddr(t) // на тестовой горутине: freeAddr может вызвать t.Fatal
	go func() { _ = Run(ctx, Config{Listen: addr, Options: o}, io.Discard, errOut, func(c int) { codes <- c }) }()
	select {
	case c := <-codes:
		return c, errOut.String()
	case <-time.After(5 * time.Second):
		t.Fatal("exit not called")
	}
	return 0, ""
}

func TestRunFailLoadPrintsFiveLinesAndExits1(t *testing.T) {
	code, errOut := runWithExit(t, Options{FailLoad: true, Loading: 100 * time.Millisecond})
	if code != 1 || strings.Count(errOut, "simulated load failure") != 5 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestRunCrashAfterExits3(t *testing.T) {
	code, _ := runWithExit(t, Options{CrashAfter: 100 * time.Millisecond})
	if code != 3 {
		t.Fatalf("code=%d", code)
	}
}
