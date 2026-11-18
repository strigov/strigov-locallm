package gateway

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/fakeupstream"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/supervisor"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// testAdminKey — ключ администратора стенда; в файл не пишется.
const testAdminKey = "admin-key-0123456789abcdef0123456789abcdef"

// abModels — модели a (-c 8192) и b (-c 4096) в каталоге моделей стенда.
const abModels = "  a:\n    path: '{models}\\a.gguf'\n    args: ['-c', '8192']\n  b:\n    path: '{models}\\b.gguf'\n    args: ['-c', '4096']\n"

// adminHostOptions — режим file, swap.default_model a; файлы a, b, c и проектор mmproj-a с заголовками GGUF.
func adminHostOptions() testutil.HostOptions {
	return testutil.HostOptions{DefaultModel: "a", Models: abModels, Stubs: []string{"a.gguf", "b.gguf", "c.gguf", "mmproj-a.gguf"}}
}

// fakeClock — часы входа и сессий (§9.4) стенда.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// adminStack — шлюз с /api/admin/* над конфигом testutil.NewHostDir во временном каталоге теста.
type adminStack struct {
	*stack
	h     testutil.HostDir
	ctl   *control.Controller
	auth  *control.Auth
	cache *gguf.Cache // кэш заголовков контроллера, поиска и meta в /models (WithHeaders)
	clock *fakeClock
}

// backendFunc — бэкенд и источник вывода для конфига стенда.
type backendFunc func(cfg *config.Config) (scheduler.Backend, LogSource)

// fakeBackend — фейковый бэкенд b (он же источник вывода).
func fakeBackend(b *schedtest.Backend) backendFunc {
	return func(*config.Config) (scheduler.Backend, LogSource) { return b, b }
}

// realBackend — настоящий супервизор; «llama-server» — HostOptions.ServerExe (fakeupstream.exe).
func realBackend(t *testing.T) backendFunc {
	return func(cfg *config.Config) (scheduler.Backend, LogSource) {
		sup := supervisor.New(cfg, nil)
		t.Cleanup(func() { _ = sup.Stop(context.Background()) }) // после остановки цикла: процесс не остаётся
		return sup, sup
	}
}

// newAdminStack — конфиг и models.yaml из o во временном каталоге, планировщик над бэкендом mk, контроллер, вход с
// ключом testAdminKey и подменяемыми часами. Журнал шлюза, контроллера и входа — st.logs.
func newAdminStack(t *testing.T, o testutil.HostOptions, mk backendFunc, opts ...stackOpt) *adminStack {
	t.Helper()
	h := testutil.NewHostDir(t, o)
	cfg, err := config.Load(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	b, logs := mk(cfg)
	a := &adminStack{h: h, clock: &fakeClock{t: time.Now()}}
	setup := func(s *server) {
		forget, _ := logs.(control.LogForgetter)
		a.cache = gguf.NewCache(0)
		a.ctl = control.New(control.Options{Live: s.live, Sched: s.sched, Logs: forget, Cache: a.cache,
			Scanner: gguf.NewScanner(a.cache, gguf.Limits{}, nil), Log: s.log})
		a.auth = control.NewAuth([][]byte{[]byte(testAdminKey)}, s.log)
		WithAdmin(a.ctl, a.auth)(s)
		WithHeaders(a.cache)(s)
		s.now = a.clock.Now
	}
	a.stack = newStack(t, cfg, b, logs, append([]stackOpt{setup}, opts...)...)
	return a
}

// admin — запрос к /api/admin/*: bearer "" — без Authorization; POST и PUT — с Content-Type: application/json;
// hdr — пары имя, значение поверх них ("Host" задаёт req.Host, пустое значение удаляет заголовок).
func (a *adminStack) admin(method, path, body, bearer string, hdr ...string) (*http.Response, []byte) {
	a.t.Helper()
	var h []string
	if method == http.MethodPost || method == http.MethodPut {
		h = append(h, "Content-Type", "application/json")
	}
	if bearer != "" {
		h = append(h, "Authorization", "Bearer "+bearer)
	}
	return a.req(method, path, body, append(h, hdr...)...)
}

// wantJSON — статус want, Content-Type application/json и тело-объект.
func wantJSON(t *testing.T, resp *http.Response, body []byte, want int) map[string]any {
	t.Helper()
	if resp.StatusCode != want || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q, body %s; want %d", resp.StatusCode, resp.Header.Get("Content-Type"), body, want)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

// adminJSON — запрос с ключом администратора; статус want и тело-объект.
func (a *adminStack) adminJSON(method, path, body string, want int, hdr ...string) (map[string]any, *http.Response) {
	a.t.Helper()
	resp, raw := a.admin(method, path, body, testAdminKey, hdr...)
	return wantJSON(a.t, resp, raw, want), resp
}

// login — вход по ключу администратора; токен сессии.
func (a *adminStack) login() string {
	a.t.Helper()
	resp, body := a.admin(http.MethodPost, "/api/admin/session", `{"admin_key":"`+testAdminKey+`"}`, "")
	token, _ := wantJSON(a.t, resp, body, http.StatusOK)["token"].(string)
	if token == "" {
		a.t.Fatalf("login: %s", body)
	}
	return token
}

// waitReady — модель id в Ready.
func (a *adminStack) waitReady(id string) {
	a.t.Helper()
	schedtest.Eventually(a.t, 30*time.Second, func() bool {
		sn := a.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == id
	})
}

// loadWait — POST /api/admin/load и ожидание Ready.
func (a *adminStack) loadWait(id string) {
	a.t.Helper()
	a.adminJSON(http.MethodPost, "/api/admin/load", `{"model":"`+id+`"}`, http.StatusAccepted)
	a.waitReady(id)
}

// stream — потоковый запрос к модели id с ключом клиента. После первой части канал ждёт конца тела (обрыв или
// [DONE]) и закрывается.
func (a *adminStack) stream(id string) <-chan struct{} {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.url+"/v1/chat/completions", strings.NewReader(`{"model":"`+id+`","stream":true}`))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		a.t.Fatalf("stream %s: status %d", id, resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	readEvents(a.t, br, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, br)
	}()
	return ended
}

// sessionOf — номер сессии токена в журнале: первые 8 hex SHA-256.
func sessionOf(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:4])
}

// wantSecurity — заголовки безопасности §9.1 v2 и отсутствие заголовков CORS.
func wantSecurity(t *testing.T, name string, resp *http.Response) {
	t.Helper()
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
		if got := resp.Header.Get(k); got != v {
			t.Fatalf("%s: %s = %q, want %q", name, k, got, v)
		}
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("%s: CORS header %s", name, k)
		}
	}
}

// H1: каждый маршрут /api/admin/* без Bearer, с ключом клиента, с неверным Bearer и с истёкшим токеном — 401;
// с ключом администратора и с токеном — не 401, 403 и 429; клиентские маршруты с ключом администратора и токеном — 401.
func TestAdminAuthMatrix(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	expired := a.login()
	a.clock.Add(12 * time.Hour)
	routes := []struct{ method, path, body string }{
		{http.MethodDelete, "/api/admin/session", ""},
		{http.MethodGet, "/api/admin/state", ""},
		{http.MethodPost, "/api/admin/load", `{"model":"a"}`},
		{http.MethodPost, "/api/admin/unload", `{}`},
		{http.MethodPost, "/api/admin/restart", `{"model":"a"}`},
		{http.MethodPost, "/api/admin/pause", `{}`},
		{http.MethodPost, "/api/admin/resume", `{}`},
		{http.MethodGet, "/api/admin/models", ""},
		{http.MethodPut, "/api/admin/models", `{"models":[]}`}, // без If-Match: 428, файл не меняется
		{http.MethodPost, "/api/admin/models/reload", `{}`},
		{http.MethodPost, "/api/admin/models/migrate", `{}`},
		{http.MethodGet, "/api/admin/scan", ""},
		{http.MethodGet, "/api/admin/logs", ""},
	}
	for _, rt := range routes {
		token := a.login()
		for _, c := range []struct{ name, bearer string }{
			{"no bearer", ""}, {"client key", testKey}, {"wrong bearer", "wrong-key"}, {"expired token", expired},
		} {
			resp, body := a.admin(rt.method, rt.path, rt.body, c.bearer)
			wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")
			if resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s %s, %s: WWW-Authenticate %q", rt.method, rt.path, c.name, resp.Header.Get("WWW-Authenticate"))
			}
		}
		for _, bearer := range []string{testAdminKey, token} {
			resp, body := a.admin(rt.method, rt.path, rt.body, bearer)
			if s := resp.StatusCode; s == http.StatusUnauthorized || s == http.StatusForbidden || s == http.StatusTooManyRequests {
				t.Fatalf("%s %s with administrator credentials: %d %s", rt.method, rt.path, s, body)
			}
		}
		a.clock.Add(61 * time.Second) // окно неудач §9.4 не переходит на следующий маршрут
	}
	token := a.login()
	for _, bearer := range []string{testAdminKey, token} {
		for _, p := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/models", ""}, {http.MethodGet, "/api/status", ""},
			{http.MethodPost, "/v1/chat/completions", `{"model":"a"}`}, {http.MethodPost, "/models/load", `{"model":"a"}`},
		} {
			resp, body := a.call(p.method, p.path, p.body, bearer)
			wantError(t, resp, body, http.StatusUnauthorized, "invalid_api_key", "authentication_error")
		}
	}
}

// H6 и §9.1: 415, 413 и 400 на теле запроса администратора; параметры Content-Type допустимы, пустое тело — {};
// заголовки безопасности на любом ответе, CORS нет; OPTIONS и чужой метод — 405 с Allow; точечный сегмент — 404.
func TestAdminBodyRulesAndHeaders(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for _, c := range []struct {
		name, body string
		hdr        []string
		status     int
		code, msg  string
	}{
		{"no content type", `{"model":"a"}`, []string{"Content-Type", ""}, 415, "unsupported_media_type", "Content-Type must be application/json"},
		{"text/plain", `{"model":"a"}`, []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type", "Content-Type must be application/json"},
		{"unknown field", `{"model":"a","x":1}`, nil, 400, "invalid_request", `invalid request body: unknown field "x"`},
		{"wrong type", `{"model":1}`, nil, 400, "invalid_request", `invalid request body: field "model" has the wrong type`},
		{"malformed", `{"model":`, nil, 400, "invalid_request", "invalid request body: malformed JSON"},
		{"trailing data", `{"model":"a"} {}`, nil, 400, "invalid_request", "invalid request body: unexpected data after the JSON object"},
		{"array", `["a"]`, nil, 400, "invalid_request", "invalid request body: body must be a JSON object"},
		{"null", `null`, nil, 400, "invalid_request", "invalid request body: body must be a JSON object"},
		{"compressed", `{"model":"a"}`, []string{"Content-Encoding", "gzip"}, 400, "invalid_request", "invalid request body: compressed bodies are not accepted"},
		{"no model", `{}`, nil, 400, "invalid_request", `invalid request body: field "model" is required`},
	} {
		resp, body := a.admin(http.MethodPost, "/api/admin/load", c.body, testAdminKey, c.hdr...)
		if e := wantError(t, resp, body, c.status, c.code, "invalid_request_error"); e["message"] != c.msg {
			t.Fatalf("%s: %v", c.name, e)
		}
		wantSecurity(t, c.name, resp)
	}
	if sn := a.sched.Snapshot(); sn.Phase != scheduler.PhaseEmpty {
		t.Fatalf("a rejected body started a load: %s", sn.Phase)
	}
	resp, body := a.admin(http.MethodPost, "/api/admin/load", `{"model":"a"}`, testAdminKey, "Content-Type", "application/json; charset=utf-8")
	if v := wantJSON(t, resp, body, http.StatusAccepted); v["accepted"] != true {
		t.Fatalf("load with charset: %v", v)
	}
	wantSecurity(t, "202", resp)
	resp, body = a.admin(http.MethodPost, "/api/admin/resume", "", testAdminKey)
	wantJSON(t, resp, body, http.StatusOK)

	// 413 — без сети: сервер отвечает до конца тела, клиент мог бы получить RST вместо ответа.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/load", strings.NewReader(`{"model":"`+strings.Repeat("x", 1<<20)+`"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := httptest.NewRecorder()
	a.srv.ServeHTTP(rec, req)
	if e := decodeError(t, rec.Body.Bytes()); rec.Code != http.StatusRequestEntityTooLarge || e["code"] != "request_too_large" ||
		e["type"] != "invalid_request_error" || e["message"] != "request body exceeds 1 MiB" {
		t.Fatalf("oversized body: %d %v", rec.Code, e)
	}

	for _, c := range []struct{ method, path, allow string }{
		{http.MethodOptions, "/api/admin/state", "GET"},
		{http.MethodPut, "/api/admin/state", "GET"},
		{http.MethodGet, "/api/admin/session", "DELETE, POST"},
		{http.MethodPost, "/api/admin/models", "GET, PUT"},
	} {
		resp, body := a.admin(c.method, c.path, "", testAdminKey)
		wantError(t, resp, body, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error")
		if resp.Header.Get("Allow") != c.allow {
			t.Fatalf("%s %s: Allow %q, want %q", c.method, c.path, resp.Header.Get("Allow"), c.allow)
		}
		wantSecurity(t, c.method+" "+c.path, resp)
	}
	for _, path := range []string{"/api/admin/nope", "/api/admin/%2e%2e/state"} {
		resp, body := a.admin(http.MethodGet, path, "", testAdminKey)
		wantError(t, resp, body, http.StatusNotFound, "not_found", "invalid_request_error")
	}
	resp, body = a.admin(http.MethodGet, "/api/admin/state", "", "")
	wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")
	wantSecurity(t, "401", resp)
}

// H4 и H5 через HTTP: вход по коду из трея и по ключу, ответ входа, выход, срок 12 ч, блокировка с 429 и Retry-After,
// при которой токены и коды работают; в журнале нет ключей, токенов и кодов.
func TestAdminLoginAndSessions(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	login := func(body string) (*http.Response, []byte) {
		t.Helper()
		return a.admin(http.MethodPost, "/api/admin/session", body, "")
	}
	code := a.auth.MintOTP(a.clock.Now())
	resp, body := login(`{"otp":"` + code + `"}`)
	v := wantJSON(t, resp, body, http.StatusOK)
	token, _ := v["token"].(string)
	exp, err := time.Parse(time.RFC3339, v["expires_at"].(string))
	if err != nil || token == "" || exp.Unix() != a.clock.Now().Add(12*time.Hour).Unix() {
		t.Fatalf("login answer %v", v)
	}
	wantSecurity(t, "login", resp)
	resp, body = login(`{"otp":"` + code + `"}`)
	wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")
	for _, b := range []string{`{}`, `{"otp":"x","admin_key":"y"}`, `{"otp":""}`} {
		resp, body := login(b)
		if e := wantError(t, resp, body, 400, "invalid_request", "invalid_request_error"); e["message"] != `invalid request body: exactly one of "otp" and "admin_key" is required` {
			t.Fatalf("%s: %v", b, e)
		}
	}
	keyToken := a.login()
	for _, tok := range []string{token, keyToken} {
		resp, body := a.admin(http.MethodGet, "/api/admin/state", "", tok)
		wantJSON(t, resp, body, http.StatusOK)
	}
	resp, body = a.admin(http.MethodDelete, "/api/admin/session", "", token)
	if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("logout: %d %s", resp.StatusCode, body)
	}
	resp, body = a.admin(http.MethodGet, "/api/admin/state", "", token)
	wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")

	a.clock.Add(61 * time.Second) // неудачи выше уходят из окна §9.4
	for i := range 11 {
		resp, body := login(`{"admin_key":"wrong-key"}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d %s", i, resp.StatusCode, body)
		}
	}
	resp, body = login(`{"admin_key":"` + testAdminKey + `"}`)
	e := wantError(t, resp, body, http.StatusTooManyRequests, "too_many_attempts", "rate_limit_error")
	if resp.Header.Get("Retry-After") != "60" || e["message"] != "too many failed attempts; retry in 60 s" {
		t.Fatalf("locked login: %v %v", resp.Header, e)
	}
	resp, body = a.admin(http.MethodGet, "/api/admin/state", "", testAdminKey)
	wantError(t, resp, body, http.StatusTooManyRequests, "too_many_attempts", "rate_limit_error")
	resp, body = a.admin(http.MethodGet, "/api/admin/state", "", keyToken)
	wantJSON(t, resp, body, http.StatusOK)
	resp, body = login(`{"otp":"` + a.auth.MintOTP(a.clock.Now()) + `"}`)
	wantJSON(t, resp, body, http.StatusOK)
	a.clock.Add(61 * time.Second)
	a.login()
	a.clock.Add(12 * time.Hour)
	resp, body = a.admin(http.MethodGet, "/api/admin/state", "", keyToken)
	wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")

	log := a.logs.String()
	for _, want := range []string{`msg="admin login" method=otp`, `msg="admin login" method=admin_key`,
		`msg="admin auth failed"`, `reason="invalid admin key"`, `reason="invalid otp"`, `msg="admin login locked" failures=11`} {
		if !strings.Contains(log, want) {
			t.Fatalf("log has no %q:\n%s", want, log)
		}
	}
	for _, secret := range []string{testAdminKey, "wrong-key", token, keyToken, code} {
		if strings.Contains(log, secret) {
			t.Fatalf("log contains a secret %q", secret)
		}
	}
}

// §9.5: команды — 202 с фазой после публикации Snapshot; 400 без имени, 404 для неизвестного имени; строка журнала
// команды — с actor и номером сессии; неизвестное имя в журнал не попадает.
func TestAdminCommands(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	v, _ := a.adminJSON(http.MethodPost, "/api/admin/load", `{"model":"b"}`, http.StatusAccepted)
	if v["accepted"] != true || (v["state"] != "loading" && v["state"] != "ready") {
		t.Fatalf("load: %v", v)
	}
	a.waitReady("b")
	token := a.login()
	resp, body := a.admin(http.MethodPost, "/api/admin/restart", `{"model":"b"}`, token)
	if v := wantJSON(t, resp, body, http.StatusAccepted); v["accepted"] != true {
		t.Fatalf("restart: %v", v)
	}
	for _, path := range []string{"/api/admin/load", "/api/admin/unload", "/api/admin/restart"} {
		resp, body := a.admin(http.MethodPost, path, `{"model":"zz"}`, testAdminKey)
		if e := wantError(t, resp, body, http.StatusNotFound, "model_not_found", "invalid_request_error"); e["message"] != `model "zz" is not configured; available: a, b` {
			t.Fatalf("%s: %v", path, e)
		}
	}
	resp, body = a.admin(http.MethodPost, "/api/admin/restart", `{}`, testAdminKey)
	wantError(t, resp, body, http.StatusBadRequest, "invalid_request", "invalid_request_error")
	a.waitReady("b")
	a.adminJSON(http.MethodPost, "/api/admin/unload", `{}`, http.StatusAccepted)
	schedtest.Eventually(t, 5*time.Second, func() bool { return a.sched.Snapshot().Phase == scheduler.PhaseEmpty })
	log := a.logs.String()
	for _, want := range []string{
		`msg="admin command" action=load actor=admin_api session="" model=b force=false`,
		`msg="admin command" action=restart actor=admin_api session=` + sessionOf(token) + ` model=b force=false`,
		`msg="admin command" action=unload actor=admin_api session="" model="" force=false`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("log has no %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "zz") {
		t.Fatalf("an unknown model name reached the log:\n%s", log)
	}
}

// H13: пауза через API — клиентские маршруты 503 host_paused с x-should-retry: false, выгрузка работает, /api/status
// и /api/admin/state показывают паузу, state.json записан; снятие паузы тоже записано.
func TestAdminPause(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	a.loadWait("a")
	v, _ := a.adminJSON(http.MethodPost, "/api/admin/pause", `{}`, http.StatusOK)
	if v["paused"] != true || v["persisted"] != true {
		t.Fatalf("pause: %v", v)
	}
	if st, ok, err := config.ReadState(a.h.State); err != nil || !ok || !st.Paused || st.By != control.KindAdmin {
		t.Fatalf("state.json after pause: %+v %v %v", st, ok, err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/chat/completions", `{"model":"a"}`},
		{http.MethodPost, "/api/swap", `{"model":"b"}`},
		{http.MethodPost, "/models/load", `{"model":"b"}`},
		{http.MethodGet, "/props", ""},
	} {
		resp, body := a.authed(c.method, c.path, c.body)
		e := wantError(t, resp, body, http.StatusServiceUnavailable, "host_paused", "server_error")
		if resp.Header.Get("x-should-retry") != "false" || e["message"] != pausedMessage {
			t.Fatalf("%s %s: x-should-retry %q, error %v", c.method, c.path, resp.Header.Get("x-should-retry"), e)
		}
	}
	if resp, body := a.authed(http.MethodPost, "/models/unload", `{"model":"a"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("/models/unload on pause: %d %s", resp.StatusCode, body)
	}
	if s := getJSON(t, a.stack, "/api/status"); s["paused"] != true {
		t.Fatalf("/api/status paused %v", s["paused"])
	}
	if s, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); s["paused"] != true || s["pause_saved"] != true {
		t.Fatalf("/api/admin/state: paused %v, pause_saved %v", s["paused"], s["pause_saved"])
	}
	v, _ = a.adminJSON(http.MethodPost, "/api/admin/resume", `{}`, http.StatusOK)
	if v["paused"] != false || v["persisted"] != true {
		t.Fatalf("resume: %v", v)
	}
	if st, ok, err := config.ReadState(a.h.State); err != nil || !ok || st.Paused {
		t.Fatalf("state.json after resume: %+v %v %v", st, ok, err)
	}
	if resp, body := a.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("after resume: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(a.logs.String(), `msg="pause state saved"`) {
		t.Fatalf("log:\n%s", a.logs.String())
	}
}

// H13: state.json не записать (на его месте каталог) — пауза действует, persisted: false, pause_saved: false.
func TestAdminPauseNotPersisted(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	if err := os.Mkdir(a.h.State, 0o755); err != nil {
		t.Fatal(err)
	}
	v, _ := a.adminJSON(http.MethodPost, "/api/admin/pause", `{}`, http.StatusOK)
	if v["paused"] != true || v["persisted"] != false {
		t.Fatalf("pause: %v", v)
	}
	if s, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); s["paused"] != true || s["pause_saved"] != false {
		t.Fatalf("/api/admin/state: paused %v, pause_saved %v", s["paused"], s["pause_saved"])
	}
	if !strings.Contains(a.logs.String(), `msg="pause state not saved"`) {
		t.Fatalf("log:\n%s", a.logs.String())
	}
}

// H14 и F3-D1 (ledger планировщика): принудительная выгрузка и принудительная пауза через API обрывают поток
// fakeupstream (--sse-interval 100ms --chunks 100) не позже чем через 1 с после ответа команды; Lease, выданный, но
// не начавший проксирование (хук G13), получает 502 с текстом §9.6.
func TestAdminForcedUnload(t *testing.T) {
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: fakeupstream.New(fakeupstream.Options{ModelID: "a", SSEInterval: 100 * time.Millisecond, Chunks: 100}, nil)})
	a := newAdminStack(t, adminHostOptions(), fakeBackend(b))
	for _, c := range []struct {
		name, path, body string
		status           int
	}{
		{"unload", "/api/admin/unload", `{"force":true}`, http.StatusAccepted},
		{"pause", "/api/admin/pause", `{"force":true}`, http.StatusOK},
	} {
		a.loadWait("a")
		ended := a.stream("a")
		a.adminJSON(http.MethodPost, c.path, c.body, c.status)
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatalf("forced %s: the stream is still open 1 s after the command", c.name)
		}
	}
	a.adminJSON(http.MethodPost, "/api/admin/resume", `{}`, http.StatusOK)
	// Строка F1 «forced unload» пишет планировщик в свой журнал (schedtest.Start его отбрасывает); её проверяют
	// тесты контроллера (TestForcedPauseAbortsLeases), поэтому здесь её число не сверяем.

	var g *adminStack
	g = newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()), withAfterGrant(func(*http.Request) {
		admin := control.Actor{Kind: control.KindAdmin}
		if err := g.ctl.Unload(admin, "", true); err != nil {
			t.Error(err)
		}
		if err := g.ctl.Unload(admin, "b", false); err != nil { // барьер: RetireUpstream уже исполнен
			t.Error(err)
		}
	}))
	resp, body := g.authed(http.MethodPost, "/v1/chat/completions", `{"model":"a"}`)
	if e := wantError(t, resp, body, http.StatusBadGateway, "upstream_error", "server_error"); e["message"] != `request aborted: model "a" was unloaded by the administrator` {
		t.Fatalf("lease before proxying: %v", e)
	}
}

// §9.5: GET /api/admin/logs — как /api/logs, только с Bearer администратора.
func TestAdminLogs(t *testing.T) {
	b := schedtest.New()
	a := newAdminStack(t, adminHostOptions(), fakeBackend(b))
	a.loadWait("a")
	b.AppendLog("a", "line 1", "line 2")
	resp, body := a.admin(http.MethodGet, "/api/admin/logs?model=a&tail=2", "", testAdminKey)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || string(body) != "line 1\nline 2\n" {
		t.Fatalf("logs: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	resp, body = a.admin(http.MethodGet, "/api/admin/logs?model=zz", "", testAdminKey)
	wantError(t, resp, body, http.StatusNotFound, "model_not_found", "invalid_request_error")
	resp, body = a.call(http.MethodGet, "/api/admin/logs", "", testKey)
	wantError(t, resp, body, http.StatusUnauthorized, "invalid_admin_credentials", "authentication_error")
}
