package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

func TestG9APISwap(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
		resp, body := st.authed(http.MethodPost, "/api/swap", `{"model":"a"}`)
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		if ms, ok := v["waited_ms"].(float64); resp.StatusCode != 200 || v["model"] != "a" || v["state"] != "ready" || !ok || ms < 0 {
			t.Fatalf("swap: %d %s", resp.StatusCode, body)
		}
		schedtest.Eventually(t, 2*time.Second, func() bool {
			sn := st.sched.Snapshot()
			return sn.Phase == scheduler.PhaseReady && sn.Current == "a" && sn.Inflight == 0
		})
	})
	t.Run("load failure is reported fresh, never cached", func(t *testing.T) {
		b := schedtest.New()
		b.SetPlan("a", schedtest.Plan{Fail: true})
		st := newStack(t, schedtest.Config("a"), b, nil)
		for range 2 {
			resp, body := st.authed(http.MethodPost, "/api/swap", `{"model":"a"}`)
			if e := wantError(t, resp, body, 503, "model_load_failed", "server_error"); e["cached"] != false {
				t.Fatalf("explicit swap must retry the load: %v", e)
			}
		}
		if s := b.Starts(); len(s) != 2 {
			t.Fatalf("starts %v, want two attempts", s)
		}
	})
	t.Run("timeout keeps the intent", func(t *testing.T) {
		cfg := schedtest.Config("a")
		cfg.Swap.WaitTimeout = time.Second
		b := schedtest.New()
		gate := make(chan struct{})
		b.SetPlan("a", schedtest.Plan{Gate: gate})
		st := newStack(t, cfg, b, nil)
		resp, body := st.authed(http.MethodPost, "/api/swap", `{"model":"a"}`)
		wantError(t, resp, body, 504, "swap_wait_timeout", "server_error")
		if in := st.sched.Snapshot().Intent; in == nil || *in != (scheduler.Intent{Kind: scheduler.IntentLoad, Model: "a"}) {
			t.Fatalf("intent %v must survive the timeout", in)
		}
		close(gate)
		schedtest.Eventually(t, 5*time.Second, func() bool {
			sn := st.sched.Snapshot()
			return sn.Phase == scheduler.PhaseReady && sn.Current == "a" && sn.Intent == nil
		})
	})
	t.Run("cooldown does not apply", func(t *testing.T) {
		cfg := schedtest.Config("a", "b")
		cfg.Swap.SwapCooldown = time.Hour
		st := newStack(t, cfg, schedtest.New(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l, err := st.sched.Acquire(ctx, "a", false)
		if err != nil {
			t.Fatal(err)
		}
		done := sendAsync(st.url+"/api/swap", `{"model":"b"}`)
		schedtest.Eventually(t, 2*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseDraining })
		l.Release()
		if r := await(t, done, 5*time.Second); r.code != 200 || !strings.Contains(string(r.body), `"model":"b"`) {
			t.Fatalf("swap: %d %s", r.code, r.body)
		}
	})
	t.Run("bad requests", func(t *testing.T) {
		st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
		resp, body := st.authed(http.MethodPost, "/api/swap", `{}`)
		wantError(t, resp, body, 400, "invalid_request", "invalid_request_error")
		resp, body = st.authed(http.MethodPost, "/api/swap", `{"model":"zz"}`)
		wantError(t, resp, body, 404, "model_not_found", "invalid_request_error")
	})
	t.Run("unload answers 202", func(t *testing.T) {
		st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
		resp, _ := st.authed(http.MethodPost, "/api/swap", `{"model":"a"}`)
		if resp.StatusCode != 200 {
			t.Fatalf("swap %d", resp.StatusCode)
		}
		resp, body := st.authed(http.MethodPost, "/api/unload", "")
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		// фаза после обработки команды: Stopping; фейковый бэкенд останавливается за микросекунды, поэтому допустимы и следующие
		if resp.StatusCode != 202 || v["accepted"] != true || !slices.Contains([]any{"stopping", "wait_vram", "empty"}, v["state"]) {
			t.Fatalf("unload: %d %s", resp.StatusCode, body)
		}
		schedtest.Eventually(t, 2*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseEmpty })
	})
	t.Run("unload rejects oversized and compressed bodies without unloading", func(t *testing.T) {
		st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
		resp, _ := st.authed(http.MethodPost, "/api/swap", `{"model":"a"}`)
		if resp.StatusCode != 200 {
			t.Fatalf("swap %d", resp.StatusCode)
		}
		schedtest.Eventually(t, 2*time.Second, func() bool {
			sn := st.sched.Snapshot()
			return sn.Phase == scheduler.PhaseReady && sn.Current == "a" && sn.Inflight == 0
		})
		// apiUnload отвечает после обработки Unload(""), поэтому выполненная выгрузка видна в Snapshot сразу после ответа
		stillLoaded := func(what string) {
			t.Helper()
			if sn := st.sched.Snapshot(); sn.Phase != scheduler.PhaseReady || sn.Current != "a" || sn.Intent != nil {
				t.Fatalf("%s must not unload: phase %s, current %q, intent %v", what, sn.Phase, sn.Current, sn.Intent)
			}
		}

		// 413 — без сети, как в G3: сервер отвечает до конца тела, клиент может получить RST вместо ответа.
		req := httptest.NewRequest(http.MethodPost, "/api/unload", strings.NewReader(strings.Repeat("x", 33<<20)))
		req.Header.Set("Authorization", "Bearer "+testKey)
		rec := httptest.NewRecorder()
		st.srv.ServeHTTP(rec, req)
		if rec.Code != 413 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("oversized unload: %d %v", rec.Code, rec.Header())
		}
		if e := decodeError(t, rec.Body.Bytes()); e["code"] != "request_too_large" || e["type"] != "invalid_request_error" {
			t.Fatalf("oversized unload: %v", e)
		}
		stillLoaded("an oversized /api/unload body")

		greq, _ := http.NewRequest(http.MethodPost, st.url+"/api/unload", strings.NewReader(`{}`))
		greq.Header.Set("Authorization", "Bearer "+testKey)
		greq.Header.Set("Content-Encoding", "gzip")
		gresp, err := http.DefaultClient.Do(greq)
		if err != nil {
			t.Fatal(err)
		}
		wantError(t, gresp, readAll(gresp), 400, "invalid_request", "invalid_request_error")
		stillLoaded("a compressed /api/unload body")
	})
}

func TestAPIStatus(t *testing.T) {
	cfg := schedtest.Config("a", "b", "c")
	cfg.Version = "0.1.0-test"
	cfg.Swap.SwapCooldown = 30 * time.Second
	b := schedtest.New()
	b.SetPlan("c", schedtest.Plan{Fail: true})
	st := newStack(t, cfg, b, nil)

	s := getJSON(t, st, "/api/status")
	vr, _ := s["vram"].(map[string]any)
	if s["version"] != "0.1.0-test" || s["state"] != "empty" || s["model"] != nil || s["target"] != nil || s["pid"] != nil ||
		s["intent"] != nil || s["inflight"] != float64(0) || s["cooldown_remaining_ms"] != float64(0) ||
		len(s["queue"].([]any)) != 0 || len(s["failed"].([]any)) != 0 {
		t.Fatalf("empty status %v", s)
	}
	if vr["source"] != "fake" || vr["usage_mib"] != float64(1234) || vr["baseline_mib"] != nil || vr["margin_mib"] != float64(512) ||
		vr["last_wait_ms"] != nil || vr["last_wait_timed_out"] != nil {
		t.Fatalf("empty vram %v", vr)
	}
	if _, err := time.Parse(time.RFC3339, s["state_since"].(string)); err != nil || !strings.HasSuffix(s["state_since"].(string), "Z") {
		t.Fatalf("state_since %v", s["state_since"])
	}

	if err := st.sched.Load("c"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseEmpty && len(sn.Failed) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := st.sched.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	s = getJSON(t, st, "/api/status")
	vr = s["vram"].(map[string]any)
	failed := s["failed"].([]any)
	if s["state"] != "ready" || s["model"] != "a" || s["target"] != nil || s["pid"] != float64(1002) || s["inflight"] != float64(1) ||
		s["cooldown_remaining_ms"] != float64(30000) || s["intent"] != nil || len(failed) != 1 {
		t.Fatalf("ready status %v", s)
	}
	f := failed[0].(map[string]any)
	if ms, _ := f["retry_in_ms"].(float64); f["model"] != "c" || f["exit_code"] != float64(1) ||
		f["message"] != "error: failed to load model" || ms <= 0 || ms > 120000 {
		t.Fatalf("failed entry %v", f)
	}
	if vr["baseline_mib"] != float64(900) || vr["last_wait_timed_out"] != false {
		t.Fatalf("ready vram %v", vr)
	}
	if _, ok := vr["last_wait_ms"].(float64); !ok {
		t.Fatalf("last_wait_ms %v", vr["last_wait_ms"])
	}

	go func() {
		if lb, err := st.sched.Acquire(ctx, "b", true); err == nil {
			lb.Release()
		}
	}()
	schedtest.Eventually(t, 2*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseDraining })
	s = getJSON(t, st, "/api/status")
	queue := s["queue"].([]any)
	intent, _ := s["intent"].(map[string]any)
	if s["state"] != "draining" || s["target"] != "b" || s["cooldown_remaining_ms"] != float64(0) || len(queue) != 1 ||
		intent["action"] != "load" || intent["model"] != "b" {
		t.Fatalf("draining status %v", s)
	}
	if q := queue[0].(map[string]any); q["model"] != "b" || q["waiters"] != float64(1) || q["oldest_wait_ms"].(float64) < 0 {
		t.Fatalf("queue %v", q)
	}
	l.Release()
}

func TestAPILogs(t *testing.T) {
	cfg := schedtest.Config("a", "b")
	cfg.Llama.LogLines = 3
	b := schedtest.New()
	st := newStack(t, cfg, b, b)
	text := func(path string) string {
		t.Helper()
		resp, body := st.authed(http.MethodGet, path, "")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatalf("GET %s: %d %q %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
		return string(body)
	}
	if got := text("/api/logs"); got != "" {
		t.Fatalf("nothing launched yet: %q", got)
	}
	resp, body := st.authed(http.MethodGet, "/api/logs?model=zz", "")
	wantError(t, resp, body, 404, "model_not_found", "invalid_request_error")
	if err := st.sched.Load("a"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseReady })
	b.AppendLog("a", "line 1", "line 2", "line 3", "line 4")
	for path, want := range map[string]string{
		"/api/logs":                "line 2\nline 3\nline 4\n", // Current, tail 200 ограничен log_lines = 3
		"/api/logs?model=a&tail=1": "line 4\n",
		"/api/logs?tail=abc":       "line 2\nline 3\nline 4\n", // неверный tail — значение по умолчанию
		"/api/logs?model=b":        "",                         // b ещё не запускалась
	} {
		if got := text(path); got != want {
			t.Fatalf("GET %s: %q, want %q", path, got, want)
		}
	}
}
