package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Сквозной тест H3 (§9.1 п. 2): настоящий exe отдаёт панель без ключа и перенаправляет /ui на /ui/.
func TestE2EPanelServedWithoutKey(t *testing.T) {
	e := startServing(t)
	resp, body, err := e.call(http.MethodGet, "/ui/", "", "")
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(string(body), `<script type="module" src="app.js"></script>`) {
		t.Fatalf("/ui/: %v %v %s", err, resp, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'; script-src 'self'") {
		t.Fatalf("/ui/: CSP %q", csp)
	}
	resp, body, err = e.call(http.MethodGet, "/ui/app.js", "", "")
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("/ui/app.js: %v %v %s", err, resp, body)
	}
	noFollow := &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noFollow.Get(e.base + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusPermanentRedirect || r.Header.Get("Location") != "/ui/" {
		t.Fatalf("/ui: %d, Location %q", r.StatusCode, r.Header.Get("Location"))
	}
}
