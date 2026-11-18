package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/fakeupstream"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// send — POST с верным ключом без *testing.T (для горутин).
func send(u, body string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, readAll(resp), nil
}

type sent struct {
	code int
	body []byte
	err  error
}

func sendAsync(u, body string) <-chan sent {
	ch := make(chan sent, 1)
	go func() {
		c, b, err := send(u, body)
		ch <- sent{c, b, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan sent, d time.Duration) sent {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r
	case <-time.After(d):
		t.Fatalf("no response within %v", d)
	}
	return sent{}
}

func TestG2BodyBytesAndAuthorization(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	var gotAuth, gotQuery string
	var gotLen int64
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, gotAuth, gotQuery, gotLen = body, r.Header.Get("Authorization"), r.URL.RawQuery, r.ContentLength
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})})
	st := newStack(t, schedtest.Config("a"), b, nil)
	msg := `{"unknown_field":{"nested":[1,2.50,"é"],"empty":null},"model":"a","text":"` + strings.Repeat("Ж🙂x", 1_500_000) + `"}`
	resp, body := st.authed(http.MethodPost, "/v1/chat/completions?x=1", msg)
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` || resp.Header.Get("X-Locallm-Wait-Ms") == "" {
		t.Fatalf("proxy: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got, []byte(msg)) || gotLen != int64(len(msg)) {
		t.Fatalf("upstream got %d bytes (Content-Length %d), sent %d", len(got), gotLen, len(msg))
	}
	if gotAuth != "" || gotQuery != "x=1" {
		t.Fatalf("Authorization %q must be stripped, query %q kept", gotAuth, gotQuery)
	}
}

func TestG3BodyLimitsAndModelErrors(t *testing.T) {
	b := schedtest.New()
	st := newStack(t, schedtest.Config("a", "b"), b, nil)

	// 413 — без сети: сервер отвечает до конца тела, клиент может получить RST вместо ответа.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"a","x":"`+strings.Repeat("x", 33<<20)+`"}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	st.srv.ServeHTTP(rec, req)
	if rec.Code != 413 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("413: %d %v", rec.Code, rec.Header())
	}
	if e := decodeError(t, rec.Body.Bytes()); e["code"] != "request_too_large" || e["type"] != "invalid_request_error" ||
		e["message"] != "request body exceeds 32 MiB" {
		t.Fatalf("413: %v", e)
	}

	for _, bad := range []string{``, `not json`, `[1,2]`, `"a"`, `{}`, `{"model":1}`, `{"model":""}`, `{"model":null}`, `{"model":["a"]}`} {
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", bad)
		e := wantError(t, resp, body, 400, "invalid_request", "invalid_request_error")
		if e["message"] != `request body must be a JSON object with a string "model" field` {
			t.Fatalf("%q: %v", bad, e)
		}
	}

	greq, _ := http.NewRequest(http.MethodPost, st.url+"/v1/chat/completions", strings.NewReader(`{"model":"a"}`))
	greq.Header.Set("Authorization", "Bearer "+testKey)
	greq.Header.Set("Content-Encoding", "gzip")
	gresp, err := http.DefaultClient.Do(greq)
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, gresp, readAll(gresp), 400, "invalid_request", "invalid_request_error")

	resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"zz"}`)
	e := wantError(t, resp, body, 404, "model_not_found", "invalid_request_error")
	if e["message"] != `model "zz" is not configured; available: a, b` {
		t.Fatalf("404: %v", e)
	}
	if s := b.Starts(); len(s) != 0 {
		t.Fatalf("no model may start on rejected requests: %v", s)
	}
}

func TestG4SSEEventsAreNotDelayed(t *testing.T) {
	base := time.Now()
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		for range 20 {
			fmt.Fprintf(w, "data: %d\n\n", time.Since(base).Nanoseconds())
			_ = rc.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})})
	st := newStack(t, schedtest.Config("a"), b, nil)
	req, _ := http.NewRequest(http.MethodPost, st.url+"/v1/chat/completions", strings.NewReader(`{"model":"a","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var lat []time.Duration
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || v == "[DONE]" {
			continue
		}
		ns, _ := strconv.ParseInt(v, 10, 64)
		lat = append(lat, time.Since(base)-time.Duration(ns))
	}
	if len(lat) != 20 {
		t.Fatalf("got %d events, want 20", len(lat))
	}
	slices.Sort(lat)
	// §14.4 G4: каждое событие — не позже 20 мс после отправки upstream.
	if worst := lat[len(lat)-1]; worst > 20*time.Millisecond {
		t.Fatalf("event latency: max %v exceeds 20ms per-event limit (median %v)", worst, lat[len(lat)/2])
	}
}

func TestG5ClientAbortReachesUpstream(t *testing.T) {
	st, sup := newRealStack(t, map[string][]string{"a": {"--sse-interval", "100ms", "--chunks", "50"}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, st.url+"/v1/chat/completions",
		strings.NewReader(`{"model":"a","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(resp))
	}
	readEvents(t, bufio.NewReader(resp.Body), 3)
	cancel()
	resp.Body.Close()
	waitFor(t, 5*time.Second, func() bool {
		lines, _ := sup.Logs("a", 1000)
		return slices.ContainsFunc(lines, func(l string) bool {
			return strings.Contains(l, "POST /v1/chat/completions") && strings.Contains(l, "aborted=true")
		})
	})
	waitFor(t, 5*time.Second, func() bool { return st.sched.Snapshot().Inflight == 0 })
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(accessLine(st.logs.String(), "/v1/chat/completions"), "status=499")
	})
}

func TestG6SchedulerErrors(t *testing.T) {
	t.Run("409 cooldown", func(t *testing.T) {
		cfg := schedtest.Config("a", "b")
		cfg.Swap.SwapCooldown = 30 * time.Second
		st := newStack(t, cfg, schedtest.New(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := st.sched.Acquire(ctx, "a", false)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"b"}`)
		e := wantError(t, resp, body, 409, "model_cooldown", "conflict_error")
		if resp.Header.Get("Retry-After") != "30" || e["loaded_model"] != "a" || e["retry_after_s"] != float64(30) ||
			e["message"] != `model "a" is in use; switching to "b" is blocked for 30 s (swap_cooldown)` {
			t.Fatalf("409: %v %v", resp.Header, e)
		}
		if logs := st.logs.String(); !strings.Contains(logs, `msg="request rejected"`) || !strings.Contains(logs, "code=model_cooldown") {
			t.Fatalf("409 must be logged:\n%s", logs)
		}
	})
	t.Run("429 queue full", func(t *testing.T) {
		cfg := schedtest.Config("a", "b")
		cfg.Swap.MaxQueue = 1
		b := schedtest.New()
		gate := make(chan struct{})
		b.SetPlan("a", schedtest.Plan{Gate: gate})
		st := newStack(t, cfg, b, nil)
		first := sendAsync(st.url+"/v1/chat/completions", `{"model":"a"}`)
		schedtest.Eventually(t, 5*time.Second, func() bool { return len(st.sched.Snapshot().Queue) == 1 })
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"b"}`)
		e := wantError(t, resp, body, 429, "queue_full", "rate_limit_error")
		if resp.Header.Get("Retry-After") != "10" || e["message"] != "swap queue is full (1 waiting requests)" {
			t.Fatalf("429: %v %v", resp.Header, e)
		}
		close(gate)
		if r := await(t, first, 5*time.Second); r.code != 200 {
			t.Fatalf("queued request: %d %s", r.code, r.body)
		}
		if !strings.Contains(st.logs.String(), "code=queue_full") {
			t.Fatal("429 must be logged")
		}
	})
	t.Run("503 load failed, then cached", func(t *testing.T) {
		b := schedtest.New()
		b.SetPlan("a", schedtest.Plan{Fail: true})
		st := newStack(t, schedtest.Config("a"), b, nil)
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`)
		e := wantError(t, resp, body, 503, "model_load_failed", "server_error")
		if resp.Header.Get("x-should-retry") != "false" || e["model"] != "a" || e["exit_code"] != float64(1) || e["cached"] != false ||
			e["message"] != `model "a" failed to load (exit code 1): error: failed to load model` {
			t.Fatalf("503: %v %v", resp.Header, e)
		}
		resp, body = st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`)
		e = wantError(t, resp, body, 503, "model_load_failed", "server_error")
		if e["cached"] != true ||
			e["message"] != `model "a" failed to load (exit code 1): error: failed to load model; cached for 120 s, load it explicitly to retry` {
			t.Fatalf("cached 503: %v", e)
		}
		if s := b.Starts(); len(s) != 1 {
			t.Fatalf("cached failure must not start the model again: %v", s)
		}
		if !strings.Contains(st.logs.String(), "code=model_load_failed") {
			t.Fatal("cached 503 must be logged")
		}
	})
	t.Run("504 swap wait timeout", func(t *testing.T) {
		cfg := schedtest.Config("a")
		cfg.Swap.WaitTimeout = time.Second
		b := schedtest.New()
		gate := make(chan struct{})
		b.SetPlan("a", schedtest.Plan{Gate: gate})
		st := newStack(t, cfg, b, nil)
		defer close(gate)
		start := time.Now()
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`)
		e := wantError(t, resp, body, 504, "swap_wait_timeout", "server_error")
		if e["message"] != `model "a" was not ready within 1 s (state: loading)` || time.Since(start) < time.Second {
			t.Fatalf("504: %v after %v", e, time.Since(start))
		}
		if !strings.Contains(st.logs.String(), "code=swap_wait_timeout") {
			t.Fatal("504 must be logged")
		}
	})
}

func TestG10UpstreamDeadBeforeHeaders(t *testing.T) {
	st, sup := newRealStack(t, map[string][]string{"a": {"--delay-headers", "10s"}}, nil)
	done := sendAsync(st.url+"/v1/chat/completions", `{"model":"a","messages":[]}`)
	waitFor(t, 15*time.Second, func() bool { return st.sched.Snapshot().Inflight == 1 })
	time.Sleep(300 * time.Millisecond) // запрос дошёл до fakeupstream и ждёт заголовков
	testutil.KillPID(sup.Status().PID)
	r := await(t, done, 10*time.Second)
	if r.code != 502 {
		t.Fatalf("status %d: %s", r.code, r.body)
	}
	e := decodeError(t, r.body)
	if e["code"] != "upstream_error" || e["type"] != "server_error" || !strings.HasPrefix(fmt.Sprint(e["message"]), `llama-server for "a" `) {
		t.Fatalf("502: %v", e)
	}
}

func TestG11WaitTimeoutDoesNotLimitStream(t *testing.T) {
	cfg := schedtest.Config("a")
	cfg.Swap.WaitTimeout = time.Second
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: fakeupstream.New(fakeupstream.Options{ModelID: "a", SSEInterval: 100 * time.Millisecond, Chunks: 30}, nil)})
	st := newStack(t, cfg, b, nil)
	start := time.Now()
	resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a","stream":true,"messages":[]}`)
	if resp.StatusCode != 200 || strings.Count(string(body), "data: ") != 31 || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("stream: %d, %d events", resp.StatusCode, strings.Count(string(body), "data: "))
	}
	if d := time.Since(start); d < 2500*time.Millisecond {
		t.Fatalf("stream took %v, want about 3 s", d)
	}
}

func TestG12FirstRequestAfterSwapSucceeds(t *testing.T) {
	st, _ := newRealStack(t, nil, nil)
	for i, m := range []string{"a", "b", "a", "b"} {
		resp, body := st.authed(http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[]}`, m))
		var v struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &v)
		if resp.StatusCode != 200 || v.Model != m || resp.Header.Get("X-Locallm-Wait-Ms") == "" {
			t.Fatalf("request %d (%s): %d %s", i, m, resp.StatusCode, body)
		}
	}
	if sn := st.sched.Snapshot(); sn.Gen != 4 || len(sn.Failed) != 0 {
		t.Fatalf("snapshot %+v", sn)
	}
}

func TestG13StaleLeaseNeverReachesNewProcess(t *testing.T) {
	granted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(*http.Request) { once.Do(func() { close(granted); <-release }) }
	st, sup := newRealStack(t, map[string][]string{"a": {"--crash-after", "2s"}}, nil, withAfterGrant(hook))
	done := sendAsync(st.url+"/v1/chat/completions", `{"model":"a","messages":[]}`)
	select {
	case <-granted:
	case <-time.After(15 * time.Second):
		t.Fatal("request for a was not granted")
	}
	waitFor(t, 10*time.Second, func() bool { _, failed := st.sched.Snapshot().Failed["a"]; return failed })
	if err := st.sched.Load("b"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == "b"
	})
	close(release)
	r := await(t, done, 10*time.Second)
	if r.code != 502 {
		t.Fatalf("status %d: %s", r.code, r.body)
	}
	if e := decodeError(t, r.body); e["code"] != "upstream_error" || e["message"] != `llama-server for "a" exited` {
		t.Fatalf("502: %v", e)
	}
	lines, _ := sup.Logs("b", 1000)
	for _, l := range lines {
		if strings.Contains(l, "POST /v1/chat/completions") {
			t.Fatalf("stale request reached the process of b: %s", l)
		}
	}
}
