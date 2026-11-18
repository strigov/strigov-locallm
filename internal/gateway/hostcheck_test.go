package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

// §9.1 v2: допустимы 127.0.0.1, ::1, localhost, хост server.listen, admin_hosts и любое имя на .ts.net; порт,
// регистр, скобки IPv6 и точка в конце имени не мешают. Хост 0.0.0.0 или :: из server.listen («все адреса») не
// добавляется.
func TestAllowedHosts(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Listen: "192.168.1.10:8080", AdminHosts: []string{"mypc", "100.64.0.1", "fd7a:115c:a1e0::1"}}}
	s := &server{cfg: cfg, hosts: allowedHosts(cfg)}
	for host, want := range map[string]bool{
		"192.168.1.10:8080": true, "mypc": true, "MYPC:8080": true, "mypc.:8080": true, "100.64.0.1:8080": true,
		"[fd7a:115c:a1e0::1]:8080": true, "[FD7A:115C:A1E0:0:0:0:0:1]": true, "127.0.0.1": true, "[::1]:1": true,
		"localhost": true, "x.tail1234.ts.net": true,
		"mypc.evil.example": false, "100.64.0.2": false, "[fd7a:115c:a1e0::2]": false, "": false, "ts.net": false,
		"192.168.1.11": false, "localhost.evil": false,
	} {
		if got := s.hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
	for _, listen := range []string{"0.0.0.0:8080", "[::]:8080"} {
		h := allowedHosts(&config.Config{Server: config.Server{Listen: listen}})
		if len(h) != 3 || h["0.0.0.0"] || h["::"] {
			t.Fatalf("listen %s: %v", listen, h)
		}
	}
}

// H2: Host и Origin запроса к /api/admin/*: 403 host_not_allowed раньше 401 и раньше входа, текст с хостом не длиннее
// 100 символов, строка журнала; клиентские маршруты Host не проверяют.
func TestAdminHostAndOrigin(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for _, c := range []struct{ host, origin string }{
		{"127.0.0.1:18080", ""}, {"localhost", ""}, {"LOCALHOST:9", ""}, {"localhost.:8080", ""}, {"[::1]:8080", ""},
		{"mypc.tail1234.ts.net", ""}, {"MyPC.Tail1234.TS.NET:8080", ""},
		{"localhost:8080", "http://localhost:8080"}, {"localhost:8080", "https://mypc.tail1234.ts.net"},
		{"localhost", "http://[::1]:8080"},
	} {
		hdr := []string{"Host", c.host}
		if c.origin != "" {
			hdr = append(hdr, "Origin", c.origin)
		}
		if resp, body := a.admin(http.MethodGet, "/api/admin/state", "", testAdminKey, hdr...); resp.StatusCode != http.StatusOK {
			t.Fatalf("Host %q, Origin %q: %d %s", c.host, c.origin, resp.StatusCode, body)
		}
	}
	long := "a" + strings.Repeat("b", 150) + ".example"
	for _, c := range []struct{ host, origin, shown string }{
		{"evil.example", "", "evil.example"},
		{"localhost.evil.example:8080", "", "localhost.evil.example:8080"},
		{"127.0.0.2", "", "127.0.0.2"},
		{"mypc", "", "mypc"},
		{"100.64.0.1:8080", "", "100.64.0.1:8080"},
		{"ts.net", "", "ts.net"},
		{"[::2]:8080", "", "[::2]:8080"},
		{long, "", long[:100]},
		{"localhost", "null", "null"},
		{"localhost", "http://evil.example", "http://evil.example"},
		{"localhost", "file://localhost", "file://localhost"},
		{"localhost", "localhost:8080", "localhost:8080"},
	} {
		hdr := []string{"Host", c.host}
		if c.origin != "" {
			hdr = append(hdr, "Origin", c.origin)
		}
		resp, body := a.admin(http.MethodGet, "/api/admin/state", "", testAdminKey, hdr...)
		e := wantError(t, resp, body, http.StatusForbidden, "host_not_allowed", "permission_error")
		if want := `host "` + c.shown + `" is not allowed for the admin interface; add it to server.admin_hosts`; e["message"] != want {
			t.Fatalf("Host %q, Origin %q: %v", c.host, c.origin, e)
		}
		wantSecurity(t, "403", resp)
	}
	resp, body := a.admin(http.MethodGet, "/api/admin/state", "", "", "Host", "evil.example")
	wantError(t, resp, body, http.StatusForbidden, "host_not_allowed", "permission_error")
	resp, body = a.admin(http.MethodPost, "/api/admin/session", `{"admin_key":"`+testAdminKey+`"}`, "", "Host", "evil.example")
	wantError(t, resp, body, http.StatusForbidden, "host_not_allowed", "permission_error")
	resp, body = a.req(http.MethodGet, "/v1/models", "", "Host", "evil.example", "Authorization", "Bearer "+testKey)
	wantJSON(t, resp, body, http.StatusOK)
	if log := a.logs.String(); !strings.Contains(log, `msg="admin host rejected" host=evil.example path=/api/admin/state`) || strings.Contains(log, long) {
		t.Fatalf("log:\n%s", log)
	}
}
