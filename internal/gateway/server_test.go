package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

func TestHealthNeedsNoKey(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	resp, body := st.call(http.MethodGet, "/health", "", "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Fatalf("health: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	resp, body = st.call(http.MethodPost, "/health", "", "")
	wantError(t, resp, body, 401, "invalid_api_key", "authentication_error")
	resp, body = st.authed(http.MethodPost, "/health", "")
	e := wantError(t, resp, body, 405, "method_not_allowed", "invalid_request_error")
	if resp.Header.Get("Allow") != "GET" || e["message"] != "method POST not allowed for /health" {
		t.Fatalf("405: Allow %q, %v", resp.Header.Get("Allow"), e)
	}
}

func TestAuthRules(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	const wrong = "wrong-key-0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		path string
		set  func(*http.Request)
		want int
	}{
		{"no key", "/v1/chat/completions", func(*http.Request) {}, 401},
		{"wrong key", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+wrong) }, 401},
		{"x-api-key", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("x-api-key", testKey) }, 401},
		{"query", "/v1/chat/completions?api_key=" + testKey, func(*http.Request) {}, 401},
		{"basic", "/v1/chat/completions", func(r *http.Request) { r.SetBasicAuth("owner", testKey) }, 401},
		{"bare key", "/v1/chat/completions", func(r *http.Request) { r.Header.Set("Authorization", testKey) }, 401},
		{"unknown path without key", "/nope", func(*http.Request) {}, 401},
		{"lowercase scheme", "/nope", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+testKey) }, 404},
		{"unknown path with key", "/nope", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testKey) }, 404},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodPost, st.url+c.path, strings.NewReader(`{"model":"a"}`))
		if err != nil {
			t.Fatal(err)
		}
		c.set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(resp)
		if resp.StatusCode != c.want {
			t.Fatalf("%s: status %d, want %d: %s", c.name, resp.StatusCode, c.want, body)
		}
		if c.want == 401 {
			e := wantError(t, resp, body, 401, "invalid_api_key", "authentication_error")
			if resp.Header.Get("WWW-Authenticate") != "Bearer" || e["message"] != "missing or invalid API key" {
				t.Fatalf("%s: %v %v", c.name, resp.Header, e)
			}
		}
	}
	logs := st.logs.String()
	if strings.Contains(logs, testKey) || strings.Contains(logs, wrong) {
		t.Fatalf("API key leaked into the log:\n%s", logs)
	}
	if strings.Count(logs, `msg="invalid api key"`) != 7 {
		t.Fatalf("want 7 invalid-key warnings:\n%s", logs)
	}
}

func accessLine(logs, path string) string {
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "msg=access") && strings.Contains(l, "path="+path+" ") {
			return l
		}
	}
	return ""
}

func TestAccessLogFields(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	req, _ := http.NewRequest(http.MethodPost, st.url+"/v1/chat/completions?secret=q1",
		strings.NewReader(`{"model":"a","messages":[{"role":"user","content":"BODY-MARKER"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Tailscale-User-Login", "owner@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Locallm-Wait-Ms") == "" {
		t.Fatalf("proxy: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	waitFor(t, 2*time.Second, func() bool { return accessLine(st.logs.String(), "/v1/chat/completions") != "" })
	line := accessLine(st.logs.String(), "/v1/chat/completions")
	for _, want := range []string{"level=INFO", "method=POST", "status=200", "model=a", "wait_ms=", "duration_ms=",
		"bytes_out=", "remote=127.0.0.1:", "ts_user=owner@example.com"} {
		if !strings.Contains(line, want) {
			t.Fatalf("access line misses %q: %s", want, line)
		}
	}
	logs := st.logs.String()
	for _, bad := range []string{"secret", "q1", "BODY-MARKER", testKey} {
		if strings.Contains(logs, bad) {
			t.Fatalf("log contains %q:\n%s", bad, logs)
		}
	}
}

func TestRouteErrors(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	resp, body := st.authed(http.MethodPut, "/v1/chat/completions", `{"model":"a"}`)
	e := wantError(t, resp, body, 405, "method_not_allowed", "invalid_request_error")
	if resp.Header.Get("Allow") != "POST" || e["message"] != "method PUT not allowed for /v1/chat/completions" {
		t.Fatalf("405: Allow %q, %v", resp.Header.Get("Allow"), e)
	}
	resp, body = st.authed(http.MethodGet, "/v1/nothing", "")
	if e := wantError(t, resp, body, 404, "not_found", "invalid_request_error"); e["message"] != "no route for GET /v1/nothing" {
		t.Fatalf("404: %v", e)
	}
	resp, body = st.authed(http.MethodDelete, "/api/zzz", "")
	if e := wantError(t, resp, body, 404, "not_found", "invalid_request_error"); e["message"] != "no route for DELETE /api/zzz" {
		t.Fatalf("404: %v", e)
	}
}

func TestNewHTTPServerSettings(t *testing.T) {
	hs := NewHTTPServer(http.NotFoundHandler(), nil)
	if hs.ReadHeaderTimeout != 10*time.Second || hs.IdleTimeout != 120*time.Second || hs.ReadTimeout != 0 ||
		hs.WriteTimeout != 0 || hs.ErrorLog == nil || hs.Handler == nil || !hs.DisableGeneralOptionsHandler {
		t.Fatalf("server %+v", hs)
	}
}

// rawGatewayCall сохраняет request target, включая dot-сегменты и OPTIONS *.
func rawGatewayCall(t *testing.T, endpoint, method, target, body, key string) (*http.Response, []byte) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	auth := ""
	if key != "" {
		auth = "Authorization: Bearer " + key + "\r\n"
	}
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%s\r\n%s",
		method, target, u.Host, len(body), auth, body); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func TestRouteRejectsDotSegments(t *testing.T) {
	type forwarded struct{ target, body string }
	seen := make(chan forwarded, 16)
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- forwarded{r.RequestURI, string(body)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})})
	st := newStack(t, schedtest.Config("a"), b, b)
	const requestBody = "{ \"model\": \"a\", \"extra\": [1, 2] }\n"
	for _, target := range []string{
		"/v1/../props",
		"/v1/./chat/completions",
		"/v1/%2e%2e/props",
		"/v1/..%2fprops",
		"/v1/%2E%2e%2Fprops",
		"/v1%2f..%2Fprops",
		"/v1/%252e%252e/props",
	} {
		for _, key := range []string{"", testKey} {
			name := "no key"
			if key != "" {
				name = "valid key"
			}
			t.Run(target+"/"+name, func(t *testing.T) {
				resp, body := rawGatewayCall(t, st.url, http.MethodPost, target, requestBody, key)
				select {
				case got := <-seen:
					t.Fatalf("unsafe request reached upstream: %q", got.target)
				default:
				}
				if key == "" {
					wantError(t, resp, body, 401, "invalid_api_key", "authentication_error")
					return
				}
				e := wantError(t, resp, body, 404, "not_found", "invalid_request_error")
				parsed, err := url.ParseRequestURI(target)
				if err != nil {
					t.Fatal(err)
				}
				if want := "no route for POST " + parsed.Path; e["message"] != want {
					t.Fatalf("message %v, want %q", e["message"], want)
				}
			})
		}
	}
	if starts := b.Starts(); len(starts) != 0 {
		t.Fatalf("rejected requests started models: %v", starts)
	}
	for _, target := range []string{"/v1/chat/completions", "/v1/custom%2Froute?part=.%2f..&keep=%2e"} {
		resp, body := rawGatewayCall(t, st.url, http.MethodPost, target, requestBody, testKey)
		if resp.StatusCode != 200 || string(body) != requestBody {
			t.Fatalf("clean POST %s: %d %s", target, resp.StatusCode, body)
		}
		if got := <-seen; got.target != target || got.body != requestBody {
			t.Fatalf("clean request changed: %+v", got)
		}
	}
}

func TestGeneralOptionsRequiresAuth(t *testing.T) {
	st := newStack(t, schedtest.Config("a"), schedtest.New(), nil)
	hs := httptest.NewUnstartedServer(st.srv)
	hs.Config = NewHTTPServer(st.srv, nil)
	hs.Start()
	t.Cleanup(hs.Close)
	resp, body := rawGatewayCall(t, hs.URL, http.MethodOptions, "*", "", "")
	wantError(t, resp, body, 401, "invalid_api_key", "authentication_error")
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("OPTIONS *: missing Bearer challenge: %v", resp.Header)
	}
	waitFor(t, 2*time.Second, func() bool { return accessLine(st.logs.String(), "*") != "" })
	if line := accessLine(st.logs.String(), "*"); !strings.Contains(line, "method=OPTIONS") || !strings.Contains(line, "status=401") {
		t.Fatalf("OPTIONS * access line: %s", line)
	}
	resp, body = rawGatewayCall(t, hs.URL, http.MethodOptions, "*", "", testKey)
	wantError(t, resp, body, 404, "not_found", "invalid_request_error")
}
