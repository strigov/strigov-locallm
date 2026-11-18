package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/control"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// docJSON — тело PUT /api/admin/models.
func docJSON(t *testing.T, models ...config.DocModel) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// model — модель документа с файлом в каталоге моделей стенда.
func (a *adminStack) model(id, file string, args ...string) config.DocModel {
	return config.DocModel{ID: id, Path: filepath.Join(a.h.ModelsDir, file), Args: args}
}

// etag — заголовок ETag ответа GET /api/admin/models.
func (a *adminStack) etag() string {
	a.t.Helper()
	_, resp := a.adminJSON(http.MethodGet, "/api/admin/models", "", http.StatusOK)
	return resp.Header.Get("ETag")
}

// put — PUT /api/admin/models с ключом администратора; ifMatch "" — без If-Match.
func (a *adminStack) put(ifMatch, body string) (*http.Response, []byte) {
	a.t.Helper()
	var hdr []string
	if ifMatch != "" {
		hdr = []string{"If-Match", ifMatch}
	}
	return a.admin(http.MethodPut, "/api/admin/models", body, testAdminKey, hdr...)
}

// waitRestarted — модель id снова в Ready в поколении новее gen.
func (a *adminStack) waitRestarted(id string, gen uint64) {
	a.t.Helper()
	schedtest.Eventually(a.t, 30*time.Second, func() bool {
		sn := a.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == id && sn.Gen > gen
	})
}

// chunks — число частей потокового ответа модели id (строки "data: " без [DONE]).
func (a *adminStack) chunks(id string) int {
	a.t.Helper()
	resp, body := a.req(http.MethodPost, "/v1/chat/completions", `{"model":"`+id+`","stream":true}`,
		"Content-Type", "application/json", "Authorization", "Bearer "+testKey)
	if resp.StatusCode != http.StatusOK {
		a.t.Fatalf("stream %s: %d %s", id, resp.StatusCode, body)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			n++
		}
	}
	return n
}

// stateModel — элемент models ответа /api/admin/state.
func stateModel(t *testing.T, st map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range st["models"].([]any) {
		if mm := m.(map[string]any); mm["id"] == id {
			return mm
		}
	}
	t.Fatalf("state has no model %q: %v", id, st["models"])
	return nil
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// H7: без If-Match — 428; старый ETag — 412 с полем etag; верный — файл записан, копия .bak создана, новое имя сразу
// в /v1/models, /api/swap на него работает; state сверяет ETag.
func TestAdminPutModels(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	v, resp := a.adminJSON(http.MethodGet, "/api/admin/models", "", http.StatusOK)
	etag := resp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"sha256:`) || v["mode"] != "file" || v["models_file"] != a.h.ModelsFile || v["out_of_sync"] != false ||
		len(v["models"].([]any)) != 2 || !reflect.DeepEqual(v["disk_errors"], []any{}) {
		t.Fatalf("GET models: %s %v", etag, v)
	}
	doc := docJSON(t, a.model("a", "a.gguf", "-c", "8192"), a.model("b", "b.gguf", "-c", "4096"), a.model("c", "c.gguf"))
	resp, body := a.put("", doc)
	wantError(t, resp, body, http.StatusPreconditionRequired, "if_match_required", "invalid_request_error")
	resp, body = a.put(`"sha256:0000"`, doc)
	if e := wantError(t, resp, body, http.StatusPreconditionFailed, "config_changed_on_disk", "conflict_error"); e["etag"] != etag ||
		e["message"] != "models.yaml changed on disk; reload it and repeat" {
		t.Fatalf("stale If-Match: %v", e)
	}
	before := readText(t, a.h.ModelsFile)
	resp, body = a.put(etag, doc)
	saved := wantJSON(t, resp, body, http.StatusOK)
	next := resp.Header.Get("ETag")
	if next == etag || saved["etag"] != next || !reflect.DeepEqual(saved["restart_required"], []any{}) || !reflect.DeepEqual(saved["warnings"], []any{}) {
		t.Fatalf("PUT: %s %v", next, saved)
	}
	if !strings.Contains(readText(t, a.h.ModelsFile), "c.gguf") {
		t.Fatalf("models.yaml:\n%s", readText(t, a.h.ModelsFile))
	}
	baks, err := filepath.Glob(a.h.ModelsFile + ".*.bak")
	if err != nil || len(baks) != 1 || readText(t, baks[0]) != before {
		t.Fatalf("backups %v %v", baks, err)
	}
	if got := modelIDs(t, getJSON(t, a.stack, "/v1/models")); got != "a,b,c" {
		t.Fatalf("/v1/models after PUT: %q", got)
	}
	resp, body = a.authed(http.MethodPost, "/api/swap", `{"model":"c"}`)
	if v := wantJSON(t, resp, body, http.StatusOK); v["model"] != "c" {
		t.Fatalf("swap to the new model: %v", v)
	}
	st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK)
	if conf := st["config"].(map[string]any); conf["applied_etag"] != next || conf["disk_etag"] != next || conf["out_of_sync"] != false || conf["mode"] != "file" {
		t.Fatalf("state config %v", conf)
	}
	if !strings.Contains(a.logs.String(), `msg="models applied" op=put actor=admin_api`) {
		t.Fatalf("log:\n%s", a.logs.String())
	}
}

// H8 и H9: правила панели через API — 422 с полем и текстом без значения; метки hand_only и причины (массив, а не
// null); исключение PV8 для неизменённого ручного списка; правка одного аргумента ручного списка — 422; путь вне
// корня, через junction, альтернативный поток, \\?\ и файловый аргумент вне корня — 422.
func TestAdminPutRejectsPanelValues(t *testing.T) {
	o := adminHostOptions()
	o.Models = "  a:\n    path: '{models}\\a.gguf'\n    args: ['-c', '8192', '--log-file', 'C:\\logs\\x.log']\n  b:\n    path: '{models}\\b.gguf'\n"
	a := newAdminStack(t, o, fakeBackend(schedtest.New()))
	v, resp := a.adminJSON(http.MethodGet, "/api/admin/models", "", http.StatusOK)
	etag := resp.Header.Get("ETag")
	ms := v["models"].([]any)
	ma, mb := ms[0].(map[string]any), ms[1].(map[string]any)
	if ma["args_hand_only"] != true || ma["path_hand_only"] != false || !reflect.DeepEqual(ma["hand_only_reasons"], []any{"forbidden flag --log-file"}) {
		t.Fatalf("hand-written model %v", ma)
	}
	if !reflect.DeepEqual(mb["hand_only_reasons"], []any{}) || !reflect.DeepEqual(mb["profiles"], []any{}) || !reflect.DeepEqual(mb["warnings"], []any{}) {
		t.Fatalf("plain model %v", mb)
	}

	keep := a.model("a", "a.gguf", "-c", "8192", "--log-file", `C:\logs\x.log`)
	b2 := a.model("b", "b.gguf", "-c", "2048")
	resp, body := a.put(etag, docJSON(t, keep, b2))
	wantJSON(t, resp, body, http.StatusOK) // PV8: ручной список не изменён
	etag = resp.Header.Get("ETag")

	edited := a.model("a", "a.gguf", "-c", "4096", "--log-file", `C:\logs\x.log`)
	outside := t.TempDir()
	testutil.GGUF(t, filepath.Join(outside, "x.gguf"), "llama", "", 4096)
	link := filepath.Join(a.h.ModelsDir, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v\n%s", err, out)
	}
	const plain = `must be a plain absolute path (no \\?\ or \\.\ prefix, no ':' after the drive letter)`
	for _, c := range []struct {
		name         string
		models       []config.DocModel
		field, msg   string
		absentInBody string
	}{
		{"edited hand list", []config.DocModel{edited, b2}, "models[0].args[2]", "flag --log-file cannot be set from the panel; edit models.yaml by hand", "x.log"},
		{"new forbidden flag", []config.DocModel{keep, b2, a.model("c", "c.gguf", "--hf-token", "SECRET123")}, "models[2].args[0]", "flag --hf-token cannot be set from the panel; edit models.yaml by hand", "SECRET123"},
		{"outside the roots", []config.DocModel{keep, b2, {ID: "c", Path: filepath.Join(outside, "x.gguf")}}, "models[2].path", "file is outside models_dirs", ""},
		{"junction", []config.DocModel{keep, b2, {ID: "c", Path: filepath.Join(link, "x.gguf")}}, "models[2].path", "file is outside models_dirs", ""},
		{"alternate stream", []config.DocModel{keep, b2, {ID: "c", Path: filepath.Join(a.h.ModelsDir, "c.gguf") + ":ads"}}, "models[2].path", plain, ""},
		{"verbatim prefix", []config.DocModel{keep, b2, {ID: "c", Path: `\\?\` + filepath.Join(a.h.ModelsDir, "c.gguf")}}, "models[2].path", plain, ""},
		{"file argument outside", []config.DocModel{keep, b2, a.model("c", "c.gguf", "--mmproj", filepath.Join(outside, "x.gguf"))}, "models[2].args[1]", "--mmproj: file is outside models_dirs", ""},
	} {
		resp, body := a.put(etag, docJSON(t, c.models...))
		e := wantError(t, resp, body, http.StatusUnprocessableEntity, "config_invalid", "invalid_request_error")
		want := []any{map[string]any{"field": c.field, "message": c.msg}}
		if !reflect.DeepEqual(e["details"], want) || e["message"] != "models document is invalid: 1 problems" {
			t.Fatalf("%s: %v", c.name, e)
		}
		if c.absentInBody != "" && strings.Contains(string(body), c.absentInBody) {
			t.Fatalf("%s: the answer contains the value: %s", c.name, body)
		}
	}
	if got := a.etag(); got != etag {
		t.Fatalf("a rejected PUT changed models.yaml: %s → %s", etag, got)
	}
}

// H10: удаление загруженной модели — 409 model_in_use с полем models, файл, ETag и каталог прежние; удаление и
// переименование swap.default_model — 409 model_is_default.
func TestAdminPutInUseAndDefault(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	a.loadWait("b")
	etag := a.etag()
	before := readText(t, a.h.ModelsFile)
	resp, body := a.put(etag, docJSON(t, a.model("a", "a.gguf", "-c", "8192")))
	e := wantError(t, resp, body, http.StatusConflict, "model_in_use", "conflict_error")
	if !reflect.DeepEqual(e["models"], []any{"b"}) || e["message"] != `model "b" is in use (loaded, loading or requested); unload it first` {
		t.Fatalf("in use: %v", e)
	}
	if readText(t, a.h.ModelsFile) != before || a.etag() != etag || modelIDs(t, getJSON(t, a.stack, "/v1/models")) != "a,b" {
		t.Fatal("a rejected PUT changed the file or the catalog")
	}
	for name, doc := range map[string]string{
		"rename": docJSON(t, a.model("a2", "a.gguf", "-c", "8192"), a.model("b", "b.gguf", "-c", "4096")),
		"delete": docJSON(t, a.model("b", "b.gguf", "-c", "4096")),
	} {
		resp, body := a.put(etag, doc)
		if e := wantError(t, resp, body, http.StatusConflict, "model_is_default", "conflict_error"); e["message"] != `model "a" is swap.default_model in locallm.yaml; change it there first` {
			t.Fatalf("%s default: %v", name, e)
		}
	}
	if !strings.Contains(a.logs.String(), `msg="models save rejected" op=put actor=admin_api session="" code=model_in_use`) {
		t.Fatalf("log:\n%s", a.logs.String())
	}
}

// H11: «Применить файл» — ручная правка применяется и out_of_sync снимается; испорченный файл и файл без
// swap.default_model — 422, каталог прежний; файл, удаляющий загруженную модель, — 409 model_in_use.
func TestAdminReload(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	a.h.WriteModels(t, abModels+"  c:\n    path: '{models}\\c.gguf'\n")
	if st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); st["config"].(map[string]any)["out_of_sync"] != true {
		t.Fatalf("manual edit: %v", st["config"])
	}
	v, resp := a.adminJSON(http.MethodPost, "/api/admin/models/reload", `{}`, http.StatusOK)
	if v["etag"] != resp.Header.Get("ETag") || modelIDs(t, getJSON(t, a.stack, "/v1/models")) != "a,b,c" {
		t.Fatalf("reload: %v", v)
	}
	if st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); st["config"].(map[string]any)["out_of_sync"] != false {
		t.Fatalf("after reload: %v", st["config"])
	}

	if err := os.WriteFile(a.h.ModelsFile, []byte("version: 1\nmodels: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, body := a.admin(http.MethodPost, "/api/admin/models/reload", `{}`, testAdminKey)
	e := wantError(t, resp, body, http.StatusUnprocessableEntity, "config_invalid", "invalid_request_error")
	d := e["details"].([]any)
	if len(d) == 0 {
		t.Fatalf("broken file: %v", e)
	}
	for _, x := range d {
		if x.(map[string]any)["field"] != "models_file" {
			t.Fatalf("broken file: %v", e)
		}
	}

	a.h.WriteModels(t, "  b:\n    path: '{models}\\b.gguf'\n")
	resp, body = a.admin(http.MethodPost, "/api/admin/models/reload", `{}`, testAdminKey)
	e = wantError(t, resp, body, http.StatusUnprocessableEntity, "config_invalid", "invalid_request_error")
	want := []any{map[string]any{"field": "swap.default_model", "message": `"a" is not a key of models or a <model>-<profile> name`}}
	if !reflect.DeepEqual(e["details"], want) {
		t.Fatalf("no default model: %v", e)
	}

	a.loadWait("c")
	a.h.WriteModels(t, abModels)
	resp, body = a.admin(http.MethodPost, "/api/admin/models/reload", `{}`, testAdminKey)
	if e := wantError(t, resp, body, http.StatusConflict, "model_in_use", "conflict_error"); !reflect.DeepEqual(e["models"], []any{"c"}) {
		t.Fatalf("loaded model removed: %v", e)
	}
	if got := modelIDs(t, getJSON(t, a.stack, "/v1/models")); got != "a,b,c" {
		t.Fatalf("catalog after rejected reloads: %q", got)
	}
}

// H12: перенос через API — legacy → file; до переноса PUT и «Применить файл» — 409 models_not_migrated, GET без
// ETag; повтор переноса — 409 already_migrated.
func TestAdminMigrate(t *testing.T) {
	o := adminHostOptions()
	o.Legacy = true
	a := newAdminStack(t, o, fakeBackend(schedtest.New()))
	v, resp := a.adminJSON(http.MethodGet, "/api/admin/models", "", http.StatusOK)
	if resp.Header.Get("ETag") != "" || v["mode"] != "legacy" || v["out_of_sync"] != false || len(v["models"].([]any)) != 2 {
		t.Fatalf("legacy GET: %q %v", resp.Header.Get("ETag"), v)
	}
	notMigrated := "models are defined in locallm.yaml; migrate them to models.yaml first"
	resp, body := a.put(`"absent"`, docJSON(t, a.model("a", "a.gguf")))
	if e := wantError(t, resp, body, http.StatusConflict, "models_not_migrated", "conflict_error"); e["message"] != notMigrated {
		t.Fatalf("legacy PUT: %v", e)
	}
	resp, body = a.admin(http.MethodPost, "/api/admin/models/reload", `{}`, testAdminKey)
	wantError(t, resp, body, http.StatusConflict, "models_not_migrated", "conflict_error")

	m, _ := a.adminJSON(http.MethodPost, "/api/admin/models/migrate", `{}`, http.StatusOK)
	if m["models_file"] != a.h.ModelsFile || m["backup"] == "" {
		t.Fatalf("migrate: %v", m)
	}
	if _, err := os.Stat(a.h.ModelsFile); err != nil {
		t.Fatal(err)
	}
	v, resp = a.adminJSON(http.MethodGet, "/api/admin/models", "", http.StatusOK)
	if resp.Header.Get("ETag") == "" || v["mode"] != "file" {
		t.Fatalf("after migrate: %q %v", resp.Header.Get("ETag"), v)
	}
	resp, body = a.admin(http.MethodPost, "/api/admin/models/migrate", `{}`, testAdminKey)
	if e := wantError(t, resp, body, http.StatusConflict, "already_migrated", "conflict_error"); e["message"] != "models are already in models.yaml" {
		t.Fatalf("second migrate: %v", e)
	}
	if got := modelIDs(t, getJSON(t, a.stack, "/v1/models")); got != "a,b" {
		t.Fatalf("catalog after migrate: %q", got)
	}
}

// H15: перезапуск применяет новые флаги — --chunks 3 → --chunks 7: до перезапуска restart_required и прежний поток,
// после — поток из 7 частей. Настоящий супервизор и fakeupstream.exe.
func TestAdminRestartAppliesNewFlags(t *testing.T) {
	o := testutil.HostOptions{DefaultModel: "a", Stubs: []string{"a.gguf", "b.gguf"},
		Models:    "  a:\n    path: '{models}\\a.gguf'\n    args: ['--chunks', '3']\n  b:\n    path: '{models}\\b.gguf'\n",
		ServerExe: testutil.BuildFakeUpstream(t), LlamaPort: testutil.FreePort(t)}
	a := newAdminStack(t, o, realBackend(t))
	a.loadWait("a")
	if n := a.chunks("a"); n != 3 {
		t.Fatalf("before PUT: %d chunks", n)
	}
	resp, body := a.put(a.etag(), docJSON(t, a.model("a", "a.gguf", "--chunks", "7"), a.model("b", "b.gguf")))
	if v := wantJSON(t, resp, body, http.StatusOK); !reflect.DeepEqual(v["restart_required"], []any{"a"}) {
		t.Fatalf("PUT: %v", v)
	}
	if st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); st["restart_required"] != true {
		t.Fatalf("state before restart: %v", st["restart_required"])
	}
	if n := a.chunks("a"); n != 3 {
		t.Fatalf("after PUT, before restart: %d chunks", n)
	}
	gen := a.sched.Snapshot().Gen
	a.adminJSON(http.MethodPost, "/api/admin/restart", `{"model":"a"}`, http.StatusAccepted)
	a.waitRestarted("a", gen)
	if n := a.chunks("a"); n != 7 {
		t.Fatalf("after restart: %d chunks", n)
	}
	if st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK); st["restart_required"] != false {
		t.Fatalf("state after restart: %v", st["restart_required"])
	}
}

// H18 (без части GGUF): A загружена с -c 8192 без --mmproj; после PUT с -c 65536 и --mmproj клиенты до перезапуска
// видят 8192 и ["text"], /api/admin/state — next с новыми значениями и restart_required; незагруженная B сразу
// показывает новый -c; после перезапуска — 65536 и ["text","image"] везде, next: null.
func TestAdminParamsBeforeAndAfterRestart(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	a.loadWait("a")
	check := func(stage string, ctxA float64, mods []any, next any, restart bool, ctxB float64) {
		t.Helper()
		v1, m := getJSON(t, a.stack, "/v1/models"), getJSON(t, a.stack, "/models")
		for name, it := range map[string]map[string]any{"/v1/models": itemByID(v1, "a"), "/models": itemByID(m, "a")} {
			meta, _ := it["meta"].(map[string]any)
			arch, _ := it["architecture"].(map[string]any)
			if meta["n_ctx"] != ctxA || !reflect.DeepEqual(arch["input_modalities"], mods) {
				t.Fatalf("%s: %s a: meta %v, architecture %v", stage, name, meta, arch)
			}
		}
		if got := itemByID(v1, "a")["context_length"]; got != ctxA {
			t.Fatalf("%s: /v1/models a context_length %v", stage, got)
		}
		if got := itemByID(v1, "b")["context_length"]; got != ctxB {
			t.Fatalf("%s: /v1/models b context_length %v", stage, got)
		}
		st, _ := a.adminJSON(http.MethodGet, "/api/admin/state", "", http.StatusOK)
		sa, sb := stateModel(t, st, "a"), stateModel(t, st, "b")
		if sa["context_length"] != ctxA || !reflect.DeepEqual(sa["next"], next) || st["restart_required"] != restart ||
			sb["context_length"] != ctxB || sb["next"] != nil || sa["status"] != "loaded" || sa["default"] != true {
			t.Fatalf("%s: state a %v, b %v, restart_required %v", stage, sa, sb, st["restart_required"])
		}
	}
	check("before PUT", 8192, []any{"text"}, nil, false, 4096)
	mmproj := filepath.Join(a.h.ModelsDir, "mmproj-a.gguf")
	resp, body := a.put(a.etag(), docJSON(t, a.model("a", "a.gguf", "-c", "65536", "--mmproj", mmproj), a.model("b", "b.gguf", "-c", "16384")))
	wantJSON(t, resp, body, http.StatusOK)
	check("after PUT", 8192, []any{"text"}, map[string]any{"context_length": float64(65536), "input_modalities": []any{"text", "image"}}, true, 16384)
	gen := a.sched.Snapshot().Gen
	a.adminJSON(http.MethodPost, "/api/admin/restart", `{"model":"a"}`, http.StatusAccepted)
	a.waitRestarted("a", gen)
	check("after restart", 65536, []any{"text", "image"}, nil, false, 16384)
}

// §9.5: GET /api/admin/scan — корни, файлы с видом, контекстом, проекторами и used_by; error всегда ""; errors — [].
func TestAdminScan(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	v, _ := a.adminJSON(http.MethodGet, "/api/admin/scan", "", http.StatusOK)
	roots := a.live.Load().ModelsDirs
	if !reflect.DeepEqual(v["roots"], []any{roots[0]}) || v["truncated"] != false || !reflect.DeepEqual(v["errors"], []any{}) {
		t.Fatalf("scan: %v", v)
	}
	if _, ok := v["elapsed_ms"].(float64); !ok {
		t.Fatalf("elapsed_ms %v", v["elapsed_ms"])
	}
	files := map[string]map[string]any{}
	for _, f := range v["files"].([]any) {
		fm := f.(map[string]any)
		files[filepath.Base(fm["path"].(string))] = fm
	}
	fa := files["a.gguf"]
	mm := filepath.Join(roots[0], "mmproj-a.gguf")
	if len(files) != 4 || fa["kind"] != "model" || fa["architecture"] != "llama" || fa["context_train"] != float64(131072) ||
		fa["shards_ok"] != true || fa["error"] != "" || !reflect.DeepEqual(fa["used_by"], []any{"a"}) || !reflect.DeepEqual(fa["mmproj"], []any{mm}) {
		t.Fatalf("a.gguf: %v (files %d)", fa, len(files))
	}
	if !reflect.DeepEqual(files["c.gguf"]["used_by"], []any{}) || files["mmproj-a.gguf"]["kind"] != "mmproj" ||
		!reflect.DeepEqual(files["mmproj-a.gguf"]["mmproj"], []any{}) {
		t.Fatalf("c.gguf %v, mmproj-a.gguf %v", files["c.gguf"], files["mmproj-a.gguf"])
	}
}

// §9.6 v2: каждая ошибка контроллера — свой статус, code, type, текст и доп. поля; прочие — как у команд MVP.
func TestAdminErrorMapping(t *testing.T) {
	s := &server{live: config.NewLive(schedtest.Config("a", "b"))}
	details := []config.FieldError{{Field: "models[0].path", Message: "file does not exist"}}
	for _, c := range []struct {
		err            error
		status         int
		code, typ, msg string
		extra          map[string]any
	}{
		{&scheduler.InUseError{Models: []string{"b"}}, 409, "model_in_use", "conflict_error", `model "b" is in use (loaded, loading or requested); unload it first`, map[string]any{"models": []string{"b"}}},
		{&control.DefaultError{Model: "a"}, 409, "model_is_default", "conflict_error", `model "a" is swap.default_model in locallm.yaml; change it there first`, nil},
		{control.ErrNotMigrated, 409, "models_not_migrated", "conflict_error", "models are defined in locallm.yaml; migrate them to models.yaml first", nil},
		{config.ErrAlreadyMigrated, 409, "already_migrated", "conflict_error", "models are already in models.yaml", nil},
		{&config.ChangedError{Path: `C:\x\models.yaml`, ETag: `"absent"`}, 412, "config_changed_on_disk", "conflict_error", "models.yaml changed on disk; reload it and repeat", map[string]any{"etag": `"absent"`}},
		{&control.InvalidError{Details: details}, 422, "config_invalid", "invalid_request_error", "models document is invalid: 1 problems", map[string]any{"details": details}},
		{control.ErrIfMatchRequired, 428, "if_match_required", "invalid_request_error", "If-Match header with the ETag from GET /api/admin/models is required", nil},
		{&control.WriteError{Err: errors.New("disk full")}, 500, "config_write_failed", "server_error", "cannot write models.yaml: disk full", nil},
		{scheduler.ErrPaused, 503, "host_paused", "server_error", pausedMessage, nil},
		{scheduler.ErrModelNotFound, 404, "model_not_found", "invalid_request_error", `model "m" is not configured; available: a, b`, nil},
		{scheduler.ErrHalted, 500, "internal_error", "server_error", scheduler.ErrHalted.Error(), nil},
	} {
		e := s.adminError("m", c.err)
		if e.Status != c.status || e.Code != c.code || e.Type != c.typ || e.Message != c.msg || !reflect.DeepEqual(e.Extra, c.extra) {
			t.Fatalf("%T %v: %+v", c.err, c.err, e)
		}
	}
}
