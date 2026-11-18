package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

const pausedMessage = "host is paused by the administrator; requests are not accepted until it is resumed"

// §9.3 v2: на паузе клиентские маршруты отвечают 503 host_paused, выгрузка работает.
func TestHostPausedClientRoutes(t *testing.T) {
	st := newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil)
	loadAndWait(t, st, "a")
	if err := st.sched.Pause(false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/chat/completions", `{"model":"a"}`},
		{http.MethodPost, "/api/swap", `{"model":"b"}`},
		{http.MethodPost, "/models/load", `{"model":"b"}`},
		{http.MethodGet, "/props", ""},
		{http.MethodGet, "/props?model=a", ""},
	} {
		resp, body := st.authed(c.method, c.path, c.body)
		e := wantError(t, resp, body, http.StatusServiceUnavailable, "host_paused", "server_error")
		if resp.Header.Get("x-should-retry") != "false" || e["message"] != pausedMessage {
			t.Fatalf("%s %s: x-should-retry %q, error %v", c.method, c.path, resp.Header.Get("x-should-retry"), e)
		}
	}
	if resp, body := st.authed(http.MethodPost, "/models/unload", `{"model":"a"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("/models/unload on pause: %d %s", resp.StatusCode, body)
	}
	if resp, body := st.authed(http.MethodPost, "/api/unload", ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/api/unload on pause: %d %s", resp.StatusCode, body)
	}
	if v := getJSON(t, st, "/api/status"); v["paused"] != true {
		t.Fatalf("status paused %v", v["paused"])
	}
	if err := st.sched.Resume(); err != nil {
		t.Fatal(err)
	}
	if v := getJSON(t, st, "/api/status"); v["paused"] != false {
		t.Fatalf("status after resume: paused %v", v["paused"])
	}
	if resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("after resume: %d %s", resp.StatusCode, body)
	}
}

// §6.7.2: Lease поколения, снятого принудительной выгрузкой, — 502 с текстом про администратора.
func TestForcedUnloadAnswers502Aborted(t *testing.T) {
	var st *stack
	st = newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil, withAfterGrant(func(*http.Request) {
		if err := st.sched.ForceUnload(""); err != nil {
			t.Error(err)
		}
		if err := st.sched.Unload("b"); err != nil { // барьер: RetireUpstream уже исполнен
			t.Error(err)
		}
	}))
	const aborted = `request aborted: model "a" was unloaded by the administrator`
	resp, body := st.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`)
	if e := wantError(t, resp, body, http.StatusBadGateway, "upstream_error", "server_error"); e["message"] != aborted {
		t.Fatalf("v1: %v", e)
	}
	loadAndWait(t, st, "a")
	resp, body = st.authed(http.MethodGet, "/props", "")
	if e := wantError(t, resp, body, http.StatusBadGateway, "upstream_error", "server_error"); e["message"] != aborted {
		t.Fatalf("props: %v", e)
	}
}

// Модели нет в каталоге автомата, хотя шлюз её знает (гонка с удалением, §9.3 v2): команды и Acquire — 404.
func TestCommandModelNotFoundIs404(t *testing.T) {
	st := newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil)
	st.live.Store(schedtest.Config("a", "b", "x"))
	for _, path := range []string{"/models/load", "/models/unload", "/v1/chat/completions"} {
		resp, body := st.authed(http.MethodPost, path, `{"model":"x"}`)
		e := wantError(t, resp, body, http.StatusNotFound, "model_not_found", "invalid_request_error")
		if e["message"] != `model "x" is not configured; available: a, b, x` {
			t.Fatalf("%s: %v", path, e)
		}
	}
}

// modelIDs — список id из ответа /v1/models, через запятую.
func modelIDs(t *testing.T, v map[string]any) string {
	t.Helper()
	var ids []string
	for _, m := range v["data"].([]any) {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, ",")
}

// F3-CV1 (ledger первого плана) и F3R2-D2 (ledger планировщика): каждый ответ 404 на /models/load перечисляет имена
// одного каталога, пока live.Store меняет каталог. Списки моделей теперь из Snapshot.Catalog (§6.8 v2), поэтому
// проверяется список «available»: его шлюз берёт одним live.Load(). Оба каталога наблюдаются до гонки —
// детерминированно. Писатель ждёт тикер и не вращается: при -cpu 1 он не занимает процессор (F3R2-D2). Без gcc это
// проверка на дым, а не -race.
func TestLiveStoreDuringRequests(t *testing.T) {
	st := newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil)
	ab, cde := schedtest.Config("a", "b"), schedtest.Config("c", "d", "e")
	available := func() string {
		t.Helper()
		resp, body := st.authed(http.MethodPost, "/models/load", `{"model":"zz"}`)
		e := wantError(t, resp, body, http.StatusNotFound, "model_not_found", "invalid_request_error")
		msg, _ := e["message"].(string)
		return strings.TrimPrefix(msg, `model "zz" is not configured; available: `)
	}
	seen := map[string]int{}
	for _, c := range []struct {
		cfg  *config.Config
		want string
	}{
		{ab, "a, b"},
		{cde, "c, d, e"},
	} {
		st.live.Store(c.cfg)
		if got := available(); got != c.want {
			t.Fatalf("catalog %q after Store, want %q", got, c.want)
		}
		seen[c.want]++
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(200 * time.Microsecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			if i%2 == 0 {
				st.live.Store(ab)
			} else {
				st.live.Store(cde)
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for range 200 {
		got := available()
		if got != "a, b" && got != "c, d, e" {
			t.Fatalf("mixed catalog %q", got)
		}
		seen[got]++
	}
	t.Logf("catalogs seen: %v", seen)
}
