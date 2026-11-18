package gateway

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/ui"
)

// wantUIHeaders — заголовки безопасности и CSP §9.1 у ответа /ui; заголовков CORS нет.
func wantUIHeaders(t *testing.T, name string, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
			"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"Cache-Control": "no-cache",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s: %s = %q; want %q", name, k, got, v)
		}
	}
	for k := range resp.Header {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("%s: CORS header %s", name, k)
		}
	}
}

// noFollow — клиент, который не идёт по перенаправлению.
var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// uiReq — запрос без тела и без перехода по 308; rawPath уходит как есть (закодированные сегменты сохраняются);
// hdr — пары имя, значение ("Host" задаёт req.Host).
func uiReq(t *testing.T, st *stack, method, rawPath string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, st.url+rawPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, readAll(resp)
}

// H3: с WithAdmin каждый файл ui.Files() отдаётся без ключа с типом по расширению и заголовками §9.1; /ui/ и
// /ui/?otp=… — index.html; неверный Bearer не мешает.
func TestUIServesEmbeddedFiles(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	types := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8"}
	check := func(rawPath, file string, hdr ...string) {
		t.Helper()
		want, err := fs.ReadFile(ui.Files(), file)
		if err != nil {
			t.Fatal(err)
		}
		resp, body := uiReq(t, a.stack, http.MethodGet, rawPath, hdr...)
		ctype := types[path.Ext(file)]
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ctype || string(body) != string(want) {
			t.Fatalf("GET %s: %d %q, %d bytes; want 200 %q, %d bytes", rawPath, resp.StatusCode,
				resp.Header.Get("Content-Type"), len(body), ctype, len(want))
		}
		wantUIHeaders(t, rawPath, resp)
	}
	entries, err := fs.ReadDir(ui.Files(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 6 {
		t.Fatalf("%d panel files", len(entries))
	}
	for _, e := range entries {
		check("/ui/"+e.Name(), e.Name())
	}
	check("/ui/", "index.html")
	check("/ui/?otp=abc", "index.html")
	check("/ui/app.js", "app.js", "Authorization", "Bearer not-a-key")
}

// /ui → 308 на /ui/ с тем же query.
func TestUIRedirectsToSlash(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for raw, loc := range map[string]string{"/ui": "/ui/", "/ui?otp=abc": "/ui/?otp=abc"} {
		resp, _ := uiReq(t, a.stack, http.MethodGet, raw)
		if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != loc {
			t.Fatalf("GET %s: %d, Location %q; want 308 %q", raw, resp.StatusCode, resp.Header.Get("Location"), loc)
		}
		wantUIHeaders(t, raw, resp)
	}
}

// Неизвестный файл, другое расширение, подкаталог, точечные и закодированные сегменты — 404 not_found.
func TestUIUnknownFiles404(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for _, raw := range []string{"/ui/missing.js", "/ui/index.htm", "/ui/APP.JS", "/ui/favicon.ico", "/ui/sub/app.js",
		"/ui//app.js", "/ui/app.js/", "/ui/.hidden.js", "/ui/%2e", "/ui/%2e%2e/app.js", "/ui/..%2fapp.js",
		"/ui/..%5capp.js", "/ui/%2e%2e%2fhealth"} {
		resp, body := uiReq(t, a.stack, http.MethodGet, raw)
		wantError(t, resp, body, http.StatusNotFound, "not_found", "invalid_request_error")
		wantUIHeaders(t, raw, resp)
	}
}

// Только GET: остальные методы, включая HEAD и OPTIONS, — 405 с Allow: GET.
func TestUIMethodNotAllowed(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for _, raw := range []string{"/ui", "/ui/", "/ui/app.js"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
			http.MethodOptions, http.MethodHead} {
			resp, body := uiReq(t, a.stack, m, raw)
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodGet {
				t.Fatalf("%s %s: %d, Allow %q", m, raw, resp.StatusCode, resp.Header.Get("Allow"))
			}
			if m != http.MethodHead {
				wantError(t, resp, body, http.StatusMethodNotAllowed, "method_not_allowed", "invalid_request_error")
			}
			wantUIHeaders(t, m+" "+raw, resp)
		}
	}
}

// Host как у /api/admin/*: чужой — 403 и строка журнала; свои — 200. Origin у /ui не проверяется.
func TestUIHostCheck(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for _, host := range []string{"evil.example", "evil.example:8080", "192.168.1.5:8080", "100.64.0.1"} {
		resp, body := uiReq(t, a.stack, http.MethodGet, "/ui/", "Host", host)
		wantError(t, resp, body, http.StatusForbidden, "host_not_allowed", "permission_error")
		wantUIHeaders(t, host, resp)
	}
	if logs := a.logs.String(); !strings.Contains(logs, `msg="admin host rejected" host=evil.example path=/ui/`) {
		t.Fatalf("no host rejection line:\n%s", logs)
	}
	for _, host := range []string{"127.0.0.1:18080", "localhost", "[::1]:18080", "pc.tail1234.ts.net:8080"} {
		if resp, _ := uiReq(t, a.stack, http.MethodGet, "/ui/", "Host", host); resp.StatusCode != http.StatusOK {
			t.Fatalf("Host %s: %d", host, resp.StatusCode)
		}
	}
	if resp, _ := uiReq(t, a.stack, http.MethodGet, "/ui/", "Origin", "http://evil.example"); resp.StatusCode != http.StatusOK {
		t.Fatalf("Origin is checked on /ui: %d", resp.StatusCode)
	}
}

// Без WithAdmin /ui идёт по пути MVP: без ключа клиента 401, с ключом 404; заголовков панели нет.
func TestUIWithoutAdminIsClientPath(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	for _, raw := range []string{"/ui", "/ui/", "/ui/app.js"} {
		resp, body := st.call(http.MethodGet, raw, "", "")
		wantError(t, resp, body, http.StatusUnauthorized, "invalid_api_key", "authentication_error")
		resp, body = st.authed(http.MethodGet, raw, "")
		wantError(t, resp, body, http.StatusNotFound, "not_found", "invalid_request_error")
		if resp.Header.Get("Content-Security-Policy") != "" {
			t.Fatalf("%s: CSP without WithAdmin", raw)
		}
	}
}

// Код из ?otp= не попадает в журнал: строка доступа содержит только путь (§9.4).
func TestUIQueryNotLogged(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	if resp, _ := uiReq(t, a.stack, http.MethodGet, "/ui/?otp=secret-code-123"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	uiReq(t, a.stack, http.MethodGet, "/ui?otp=secret-code-123")
	waitFor(t, 2*time.Second, func() bool {
		logs := a.logs.String()
		return accessLine(logs, "/ui/") != "" && accessLine(logs, "/ui") != ""
	})
	logs := a.logs.String()
	if line := accessLine(logs, "/ui/"); !strings.Contains(line, "status=200") {
		t.Fatalf("access line %q", line)
	}
	if strings.Contains(logs, "secret-code-123") {
		t.Fatalf("the code is in the log:\n%s", logs)
	}
}
