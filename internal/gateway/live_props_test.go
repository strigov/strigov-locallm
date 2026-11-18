package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

// F3R2-D1 (ledger планировщика): GET /props проверяет имя по live.Load() на каждый запрос, а не по конфигу старта.
func TestPropsChecksLiveCatalog(t *testing.T) {
	st := newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil)
	st.live.Store(schedtest.Config("b"))
	resp, body := st.authed(http.MethodGet, "/props?model=a", "")
	if e := wantError(t, resp, body, http.StatusNotFound, "model_not_found", "invalid_request_error"); e["message"] != `model "a" is not configured; available: b` {
		t.Fatalf("props after live.Store: %v", e)
	}
}

// §6.8 v2: имена и порядок /v1/models, /models и списка failed в /api/status — из Snapshot.Catalog, а не из live.
func TestModelsListFromSnapshotCatalog(t *testing.T) {
	b := schedtest.New()
	b.SetPlan("x", schedtest.Plan{Fail: true})
	st := newStack(t, schedtest.Config("a", "b"), b, b)
	next := schedtest.Config("a", "b", "x")
	st.live.Store(next)
	for _, path := range []string{"/v1/models", "/models"} {
		if got := modelIDs(t, getJSON(t, st, path)); got != "a,b" {
			t.Fatalf("%s before Reconfigure: %q", path, got)
		}
	}
	if err := st.sched.Reconfigure(next.Models, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/models", "/models"} {
		if got := modelIDs(t, getJSON(t, st, path)); got != "a,b,x" {
			t.Fatalf("%s after Reconfigure: %q", path, got)
		}
	}
	if err := st.sched.Load("x"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseEmpty && len(sn.Failed) == 1
	})
	st.live.Store(schedtest.Config("a", "b")) // в live x нет, в каталоге автомата — есть
	failed := getJSON(t, st, "/api/status")["failed"].([]any)
	if len(failed) != 1 || failed[0].(map[string]any)["model"] != "x" {
		t.Fatalf("failed %v", failed)
	}
}

// F3-D2 (ledger планировщика): поколение, снятое принудительной выгрузкой (RetireForced), — 502 с текстом §9.6 v2
// и на входе serveProxy, и в ErrorHandler (запрос ушёл, заголовков ответа ещё нет).
func TestProxyForcedRetireAnswers502Aborted(t *testing.T) {
	const aborted = `request aborted: model "a" was unloaded by the administrator`
	retired := upstreamFor(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached a retired upstream")
	}))
	retired.Retire(scheduler.RetireForced)
	resp := post(t, proxyServer(t, retired)+"/v1/chat/completions", `{"model":"a"}`)
	if e := decodeError(t, readAll(resp)); resp.StatusCode != http.StatusBadGateway || e["message"] != aborted {
		t.Fatalf("serveProxy entry: %d %v", resp.StatusCode, e)
	}

	started := make(chan struct{})
	up := upstreamFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	target := proxyServer(t, up) + "/v1/completions"
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post(target, "application/json", strings.NewReader(`{"model":"a"}`))
		done <- result{resp, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not start")
	}
	up.Retire(scheduler.RetireForced)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if e := decodeError(t, readAll(res.resp)); res.resp.StatusCode != http.StatusBadGateway || e["message"] != aborted {
		t.Fatalf("error handler: %d %v", res.resp.StatusCode, e)
	}
}
