package gateway

import (
	"net/http"
	"testing"

	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

func TestG1AuthMatrix(t *testing.T) {
	b := schedtest.New()
	st := newStack(t, schedtest.Config("a"), b, b)
	routes := []struct {
		method, path, body string
		want               int // с верным ключом
	}{
		{http.MethodGet, "/v1/models", "", 200},
		{http.MethodPost, "/v1/chat/completions", `{"model":"a"}`, 200},
		{http.MethodPost, "/v1/completions", `{"model":"a"}`, 200},
		{http.MethodPost, "/v1/embeddings", `{"model":"a"}`, 404},  // ответ fakeupstream проходит как есть
		{http.MethodGet, "/props?model=a&autoload=false", "", 404}, // a загружена; /props у fakeupstream нет, его ответ проходит как есть
		{http.MethodGet, "/models", "", 200},
		{http.MethodPost, "/models/load", `{"model":"a"}`, 200},
		{http.MethodPost, "/models/unload", `{"model":"a"}`, 200},
		{http.MethodGet, "/api/status", "", 200},
		{http.MethodPost, "/api/swap", `{"model":"a"}`, 200},
		{http.MethodPost, "/api/unload", "", 202},
		{http.MethodGet, "/api/logs", "", 200},
		{http.MethodGet, "/nope", "", 404},
		{http.MethodPost, "/health", "", 405},
	}
	for _, r := range routes {
		for _, key := range []string{"", "wrong-key-0123456789abcdef0123456789abcdef"} {
			resp, body := st.call(r.method, r.path, r.body, key)
			wantError(t, resp, body, 401, "invalid_api_key", "authentication_error")
		}
	}
	if s := b.Starts(); len(s) != 0 {
		t.Fatalf("unauthorized requests must not start models: %v", s)
	}
	for _, r := range routes {
		if resp, body := st.authed(r.method, r.path, r.body); resp.StatusCode != r.want {
			t.Fatalf("%s %s with key: %d, want %d: %s", r.method, r.path, resp.StatusCode, r.want, body)
		}
	}
	if resp, _ := st.call(http.MethodGet, "/health", "", ""); resp.StatusCode != 200 {
		t.Fatalf("GET /health without key: %d", resp.StatusCode)
	}
}
