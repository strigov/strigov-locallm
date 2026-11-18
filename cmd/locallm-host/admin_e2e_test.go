package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// adminKey — ключ администратора, который хост создал в <dir>\admin-key.txt.
func (e *e2eHost) adminKey(t *testing.T) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(e.dir, "admin-key.txt"))), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// admin — запрос к /api/admin/* хоста с ключом администратора; POST и PUT — с Content-Type: application/json;
// hdr — пары имя, значение.
func (e *e2eHost) admin(t *testing.T, method, path, body string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminKey(t))
	if method == http.MethodPost || method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e2eClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// adminObject — ответ /api/admin/* со статусом want как объект.
func adminObject(t *testing.T, resp *http.Response, body []byte, want int) map[string]any {
	t.Helper()
	var v map[string]any
	if resp.StatusCode != want || json.Unmarshal(body, &v) != nil {
		t.Fatalf("status %d, body %s; want %d", resp.StatusCode, body, want)
	}
	return v
}

// Сквозной тест (§15): ключ администратора создан хостом; GET и PUT /api/admin/models с If-Match добавляют модель c
// (--chunks 5) в models.yaml (с копией .bak), она сразу видна в /v1/models, и на неё работает пересадка /api/swap.
func TestE2EAdminPutNewModelAndSwap(t *testing.T) {
	dir := t.TempDir()
	models := filepath.Join(dir, "models")
	e := startServingWith(t, testConf{Dir: dir, DefaultModel: "a", ModelsMode: "file",
		Extra: "models_dirs: ['" + models + "']\n"})
	e.waitReady(t, "a")
	testutil.StubFile(t, filepath.Join(models, "c.gguf"))
	resp, body := e.admin(t, http.MethodGet, "/api/admin/models", "")
	etag := resp.Header.Get("ETag")
	if v := adminObject(t, resp, body, http.StatusOK); etag == "" || v["mode"] != "file" {
		t.Fatalf("GET /api/admin/models: %q %v", etag, v)
	}
	doc, err := json.Marshal(map[string]any{"models": []map[string]any{
		{"id": "a", "path": filepath.Join(models, "a.gguf"), "args": []string{}},
		{"id": "b", "path": filepath.Join(models, "b.gguf"), "args": []string{}},
		{"id": "c", "path": filepath.Join(models, "c.gguf"), "args": []string{"--chunks", "5"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp, body = e.admin(t, http.MethodPut, "/api/admin/models", string(doc))
	adminObject(t, resp, body, http.StatusPreconditionRequired)
	resp, body = e.admin(t, http.MethodPut, "/api/admin/models", string(doc), "If-Match", etag)
	if v := adminObject(t, resp, body, http.StatusOK); v["etag"] == etag || resp.Header.Get("ETag") != v["etag"] {
		t.Fatalf("PUT: %v", v)
	}
	if ids := e.modelIDs(t); !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Fatalf("/v1/models after PUT: %v", ids)
	}
	resp, body, err = e.call(http.MethodPost, "/api/swap", `{"model":"c"}`, e.key)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("swap to c: %v %v %s", err, resp, body)
	}
	if n := e.completionTokens(t, "c"); n != 5 {
		t.Fatalf("model c answered with %d tokens, want 5 (--chunks 5)", n)
	}
	if text := readFile(t, filepath.Join(dir, "models.yaml")); !strings.Contains(text, "c.gguf") || !strings.Contains(text, "--chunks") {
		t.Fatalf("models.yaml:\n%s", text)
	}
	if baks, err := filepath.Glob(filepath.Join(dir, "models.yaml.*.bak")); err != nil || len(baks) != 1 {
		t.Fatalf("backups %v %v", baks, err)
	}
	out := e.p.out.String()
	if !strings.Contains(out, `msg="models applied" op=put actor=admin_api`) || strings.Contains(out, e.adminKey(t)) {
		t.Fatalf("host log must have the save line and never the admin key:\n%s", out)
	}
}

// Сквозной тест (§15, H13): пауза через API записана в state.json и переживает перезапуск: после TerminateProcess
// новый хост с тем же каталогом стартует на паузе, default_model не грузит (строка журнала), клиентам отвечает
// 503 host_paused; снятие паузы записывается, и модель снова грузится.
func TestE2EPauseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	e := startServingWith(t, testConf{Dir: dir, DefaultModel: "a"})
	e.waitReady(t, "a")
	resp, body := e.admin(t, http.MethodPost, "/api/admin/pause", `{}`)
	if v := adminObject(t, resp, body, http.StatusOK); v["paused"] != true || v["persisted"] != true {
		t.Fatalf("pause: %v", v)
	}
	if st, ok, err := config.ReadState(state); err != nil || !ok || !st.Paused || st.By != "admin_api" {
		t.Fatalf("state.json after pause: %+v %v %v", st, ok, err)
	}
	if err := e.p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	e.p.waitExit(t, 20*time.Second)

	e2 := startServingWith(t, testConf{Dir: dir, DefaultModel: "a"})
	if out := e2.p.out.String(); !strings.Contains(out, `msg="default model skipped: host is paused" model=a`) ||
		!strings.Contains(out, "paused=true") {
		t.Fatalf("second start:\n%s", out)
	}
	if st, model, _ := e2.status(t); st != "empty" || model != "" {
		t.Fatalf("second start: state %q, model %q", st, model)
	}
	resp, body, err := e2.call(http.MethodPost, "/v1/chat/completions", `{"model":"a","messages":[{"role":"user","content":"hi"}]}`, e2.key)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "host_paused") {
		t.Fatalf("chat on pause: %v %v %s", err, resp, body)
	}
	resp, body = e2.admin(t, http.MethodPost, "/api/admin/resume", `{}`)
	if v := adminObject(t, resp, body, http.StatusOK); v["paused"] != false || v["persisted"] != true {
		t.Fatalf("resume: %v", v)
	}
	if st, ok, err := config.ReadState(state); err != nil || !ok || st.Paused {
		t.Fatalf("state.json after resume: %+v %v %v", st, ok, err)
	}
	if n := e2.completionTokens(t, "a"); n != 20 {
		t.Fatalf("after resume: %d tokens", n)
	}
}
