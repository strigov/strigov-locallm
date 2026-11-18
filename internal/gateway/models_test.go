package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
)

func getJSON(t *testing.T, st *stack, path string) map[string]any {
	t.Helper()
	resp, body := st.authed(http.MethodGet, path, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s: %d %q %s", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return v
}

func readGolden(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "routermode", name))
	if err != nil {
		t.Fatalf("golden %s (P0-REF, plan 1 Ф5): %v", name, err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return v
}

// provisionalGoldens — эталоны, помеченные provisional в testdata/routermode/README.md (план 1 Ф5 Step 11):
// строка таблицы "| <файл> | <что снято> | true — <причина> |"; файл → текст ячейки. Непомеченный эталон в карту не попадает.
func provisionalGoldens(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "routermode", "README.md"))
	if err != nil {
		t.Fatalf("testdata/routermode/README.md (P0-REF, plan 1 Ф5): %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		cells := strings.Split(strings.TrimSpace(line), "|") // "| a | b | c |" → ["", a, b, c, ""]
		if len(cells) != 5 {
			continue
		}
		file, flag := strings.Trim(strings.TrimSpace(cells[1]), "`"), strings.TrimSpace(cells[3])
		if strings.HasPrefix(flag, "true") {
			out[file] = flag
		}
	}
	return out
}

func items(body map[string]any) []map[string]any {
	arr, _ := body["data"].([]any)
	var out []map[string]any
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func itemByID(body map[string]any, id string) map[string]any {
	for _, it := range items(body) {
		if it["id"] == id {
			return it
		}
	}
	return nil
}

func statusOf(item map[string]any) map[string]any {
	m, _ := item["status"].(map[string]any)
	return m
}

// goldenItem — элемент эталона для статуса; nil — в эталоне нет элемента этого статуса.
func goldenItem(body map[string]any, status string) map[string]any {
	for _, it := range items(body) {
		st := statusOf(it)
		switch status {
		case "failed": // модель «неудачи» P0-REF — big-oom или broken; признак — что-то кроме value: unloaded
			if (it["id"] == "big-oom" || it["id"] == "broken") &&
				(st["failed"] == true || st["exit_code"] != nil || (st["value"] != nil && st["value"] != "unloaded")) {
				return it
			}
		case "unloaded":
			if st["value"] == "unloaded" && (st["failed"] == nil || st["failed"] == false) {
				return it
			}
		default:
			if st["value"] == status {
				return it
			}
		}
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	}
	return fmt.Sprintf("%T", v)
}

// zeroOf — нулевое значение JSON-типа эталона (§9.4: поле без аналога у нас).
func zeroOf(v any) any {
	switch t := v.(type) {
	case string:
		return ""
	case float64:
		return 0
	case bool:
		return false
	case []any:
		return []any{}
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = zeroOf(x)
		}
		return m
	}
	return nil
}

// shapeDiff сравнивает ключи и JSON-типы ours с эталоном и накапливает поправку; несовместимые типы — в errs.
func shapeDiff(ours, golden map[string]any, prefix string, allowExtra map[string]bool, p *fieldPatch, errs *[]string) {
	for k, gv := range golden {
		ov, ok := ours[k]
		if !ok {
			if p.add == nil {
				p.add = map[string]any{}
			}
			p.add[prefix+k] = zeroOf(gv)
			continue
		}
		gk, ok2 := jsonKind(gv), jsonKind(ov)
		switch {
		case gk == "null" || ok2 == "null":
		case gk != ok2:
			*errs = append(*errs, fmt.Sprintf("%s%s: ours %s, router mode %s", prefix, k, ok2, gk))
		case gk == "object":
			shapeDiff(ov.(map[string]any), gv.(map[string]any), prefix+k+".", allowExtra, p, errs)
		}
	}
	for k := range ours {
		if _, ok := golden[k]; !ok && !allowExtra[prefix+k] {
			p.drop = append(p.drop, prefix+k)
		}
	}
	slices.Sort(p.drop)
}

func without(m map[string]any, key string) map[string]any {
	c := maps.Clone(m)
	delete(c, key)
	return c
}

// shapeStack — модели a, b, c: c failed (Fail), b loaded; затем a loading (Gate). Снимки до и после.
func shapeStack(t *testing.T) (st *stack, s1, s2 map[bool]map[string]any) {
	t.Helper()
	b := schedtest.New()
	b.SetPlan("c", schedtest.Plan{Fail: true})
	st = newStack(t, schedtest.Config("a", "b", "c"), b, b)
	if err := st.sched.Load("c"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseEmpty && len(sn.Failed) == 1
	})
	if err := st.sched.Load("b"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == "b"
	})
	s1 = map[bool]map[string]any{false: getJSON(t, st, "/models"), true: getJSON(t, st, "/v1/models")}
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	b.SetPlan("a", schedtest.Plan{Gate: gate})
	if err := st.sched.Load("a"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseLoading && sn.Current == "a"
	})
	s2 = map[bool]map[string]any{false: getJSON(t, st, "/models"), true: getJSON(t, st, "/v1/models")}
	return st, s1, s2
}

func TestG7ModelsMatchRouterMode(t *testing.T) {
	prov := provisionalGoldens(t)
	_, s1, s2 := shapeStack(t)
	cases := []struct {
		status, file, id string
		snap             map[bool]map[string]any
	}{
		{"loaded", "models_loaded.json", "b", s1},
		{"unloaded", "models_unloaded.json", "a", s1},
		{"failed", "models_failed.json", "c", s1},
		{"loading", "models_loading.json", "a", s2},
	}
	var literal strings.Builder
	for _, v1 := range []bool{false, true} {
		name, prefix := "modelsShape", ""
		allow := map[string]bool{}
		if v1 {
			name, prefix = "v1ModelsShape", "v1_"
			allow["context_length"] = true // расширение §9.3
		}
		patches := map[string]fieldPatch{}
		for _, c := range cases {
			t.Run(prefix+c.status, func(t *testing.T) {
				g := goldenItem(readGolden(t, prefix+c.file), c.status)
				if g == nil {
					if why, ok := prov[prefix+c.file]; ok {
						t.Skipf("%s%s has no %s item; README marks it provisional (%s); re-run after P0-REF", prefix, c.file, c.status, why)
					}
					t.Fatalf("%s%s has no %s item and is not marked provisional in testdata/routermode/README.md: router mode shape is incompatible with G7", prefix, c.file, c.status)
				}
				o := itemByID(c.snap[v1], c.id)
				if o == nil {
					t.Fatalf("our body has no item %q", c.id)
				}
				var p fieldPatch
				var errs []string
				shapeDiff(o, g, "", allow, &p, &errs)
				if c.status == "failed" {
					if gv := statusOf(g)["value"]; gv != failedValue {
						errs = append(errs, fmt.Sprintf("status.value of a failed model: ours %q, router mode %v; set var failedValue = %q", failedValue, gv, fmt.Sprint(gv)))
					}
				}
				for _, e := range errs {
					t.Errorf("%s: %s (fix modelsBody in models.go)", name, e)
				}
				if len(p.add) > 0 || len(p.drop) > 0 {
					patches[c.status] = p
				}
			})
		}
		var top fieldPatch
		var errs []string
		shapeDiff(without(s1[v1], "data"), without(readGolden(t, prefix+"models_loaded.json"), "data"), "", nil, &top, &errs)
		for _, e := range errs {
			t.Errorf("%s top level: %s", name, e)
		}
		if len(top.add) > 0 || len(top.drop) > 0 {
			patches["_top"] = top
		}
		if len(patches) > 0 {
			fmt.Fprintf(&literal, "var %s = map[string]fieldPatch{\n", name)
			for _, k := range slices.Sorted(maps.Keys(patches)) {
				fmt.Fprintf(&literal, "\t%q: {add: %#v, drop: %#v},\n", k, patches[k].add, patches[k].drop)
			}
			literal.WriteString("}\n")
		}
	}
	if literal.Len() > 0 {
		t.Fatalf("/models shape differs from router mode; replace the declarations in internal/gateway/shape.go with:\n%s", literal.String())
	}
}

func TestModelsBodyValues(t *testing.T) {
	// значения наших полей — без поправок формы router mode (их проверяет G7)
	saved, savedV1 := modelsShape, v1ModelsShape
	modelsShape, v1ModelsShape = map[string]fieldPatch{}, map[string]fieldPatch{}
	t.Cleanup(func() { modelsShape, v1ModelsShape = saved, savedV1 })
	st, s1, _ := shapeStack(t)
	m := s1[false]
	var ids []string
	for _, it := range items(m) {
		ids = append(ids, fmt.Sprint(it["id"]))
	}
	if !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Fatalf("ids %v, want config order a, b, c", ids)
	}
	bi := itemByID(m, "b")
	if bi["path"] != `C:\models\b.gguf` || statusOf(bi)["value"] != "loaded" {
		t.Fatalf("loaded item %v", bi)
	}
	if s := statusOf(itemByID(m, "c")); s["value"] != failedValue || s["failed"] != true || s["exit_code"] != float64(1) {
		t.Fatalf("failed item status %v", s)
	}
	if s := statusOf(itemByID(m, "a")); s["value"] != "unloaded" {
		t.Fatalf("unloaded item status %v", s)
	}
	v := s1[true]
	if v["object"] != "list" {
		t.Fatalf("/v1/models %v", v)
	}
	for id, want := range map[string]string{"a": "unloaded", "b": "loaded", "c": failedValue} {
		it := itemByID(v, id)
		if it["object"] != "model" || it["owned_by"] != "locallm" || it["created"] != float64(st.srv.started.Unix()) ||
			it["context_length"] != float64(4096) || statusOf(it)["value"] != want {
			t.Fatalf("/v1/models item %s: %v", id, it)
		}
	}
}

// F2-D6: поле есть в эталонах, поэтому остаётся, но argv клиенту не уходит ни в одном статусе, с поправками формы.
func TestModelsHideArgv(t *testing.T) {
	_, s1, s2 := shapeStack(t)
	for _, snap := range []map[bool]map[string]any{s1, s2} {
		for _, v1 := range []bool{false, true} {
			for _, it := range items(snap[v1]) {
				if raw, ok := statusOf(it)["args"].([]any); !ok || len(raw) != 0 {
					t.Fatalf("v1=%v, item %v: status.args must be an empty array", v1, it)
				}
			}
		}
	}
}

func TestModelsContextLengthMatchesV1(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "llama-server.exe")
	model := filepath.Join(dir, "model.gguf")
	for _, path := range []string{exe, model} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "locallm.yaml")
	yaml := fmt.Sprintf("llama:\n  server_exe: %q\n  default_args: []\nmodels:\n  a:\n    path: %q\n    args: ['-c', '16384']\n  b:\n    path: %q\n", exe, model, model)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st := newStack(t, cfg, schedtest.New(), nil)
	loadAndWait(t, st, "a")
	models := getJSON(t, st, "/models")
	meta, ok := itemByID(models, "a")["meta"].(map[string]any)
	if !ok {
		t.Fatal("loaded model has no meta object")
	}
	v1 := getJSON(t, st, "/v1/models")
	if want := itemByID(v1, "a")["context_length"]; want != float64(16384) || meta["n_ctx"] != want {
		t.Fatalf("meta.n_ctx %v, context_length %v; want 16384", meta["n_ctx"], want)
	}
	v1Meta, ok := itemByID(v1, "a")["meta"].(map[string]any)
	if !ok || v1Meta["n_ctx"] != float64(16384) {
		t.Fatalf("/v1/models meta %v; want n_ctx 16384", itemByID(v1, "a")["meta"])
	}
	if _, exists := itemByID(v1, "b")["meta"]; exists {
		t.Fatal("/v1/models: unloaded model acquired a meta object")
	}
	if _, exists := itemByID(models, "b")["meta"]; exists {
		t.Fatal("unloaded model acquired a meta object")
	}
}

func TestModelsInputModalities(t *testing.T) {
	const projector = `C:\private-projector\projection-secret.gguf`
	cases := []struct {
		name           string
		defaults, args []string
		image          bool
	}{
		{name: "text"},
		{name: "mmproj", args: []string{"--mmproj", projector}, image: true},
		{name: "mm", args: []string{"-mm", projector}, image: true},
		{name: "mmproj equals", args: []string{"--mmproj=" + projector}, image: true},
		{name: "no mmproj", args: []string{"--no-mmproj"}},
		{name: "defaults mmproj", defaults: []string{"--mmproj", projector}, image: true},
		{name: "defaults mm", defaults: []string{"-mm", projector}, image: true},
		{name: "defaults mmproj equals", defaults: []string{"--mmproj=" + projector}, image: true},
		{name: "default projector with model args", defaults: []string{"--mmproj", projector}, args: []string{"--no-webui"}, image: true},
		{name: "model projector with default args", defaults: []string{"--no-webui"}, args: []string{"--mmproj", projector}, image: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := schedtest.Config("a", "b", "c", "d")
			cfg.Llama.DefaultArgs = slices.Clone(c.defaults)
			for i := range cfg.Models {
				cfg.Models[i].Args = append([]string{"-c", "16384"}, c.args...)
				cfg.Models[i].ContextLength = 16384
			}
			b := schedtest.New()
			b.SetPlan("c", schedtest.Plan{Fail: true})
			gate := make(chan struct{})
			defer close(gate)
			b.SetPlan("d", schedtest.Plan{Gate: gate})
			st := newStack(t, cfg, b, b)
			if err := st.sched.Load("c"); err != nil {
				t.Fatal(err)
			}
			schedtest.Eventually(t, 5*time.Second, func() bool {
				sn := st.sched.Snapshot()
				return sn.Phase == scheduler.PhaseEmpty && len(sn.Failed) == 1
			})
			loadAndWait(t, st, "a")
			check := func() {
				t.Helper()
				want := []any{"text"}
				if c.image {
					want = append(want, "image")
				}
				for _, path := range []string{"/models", "/v1/models"} {
					resp, body := st.authed(http.MethodGet, path, "")
					if resp.StatusCode != 200 {
						t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
					}
					var catalog map[string]any
					if err := json.Unmarshal(body, &catalog); err != nil {
						t.Fatal(err)
					}
					for _, item := range items(catalog) {
						arch, _ := item["architecture"].(map[string]any)
						got, ok := arch["input_modalities"].([]any)
						if !ok || !slices.Equal(got, want) {
							t.Fatalf("GET %s: model %v modalities %v, want %v", path, item["id"], got, want)
						}
						if meta, ok := item["meta"].(map[string]any); ok && meta["n_ctx"] != float64(16384) {
							t.Fatalf("GET %s: model %v meta.n_ctx %v, want 16384", path, item["id"], meta["n_ctx"])
						}
						if args, ok := statusOf(item)["args"].([]any); !ok || len(args) != 0 {
							t.Fatalf("GET %s: model %v status.args must be an empty array", path, item["id"])
						}
					}
					for _, private := range []string{cfg.Llama.ServerExe, projector, "projection-secret.gguf", "--mmproj", "-mm", "--no-mmproj", "--no-webui"} {
						encoded, _ := json.Marshal(private)
						if strings.Contains(string(body), strings.Trim(string(encoded), `"`)) {
							t.Fatalf("GET %s leaked an argument or path: %q", path, private)
						}
					}
					for _, m := range cfg.Models {
						encoded, _ := json.Marshal(m.Path)
						if strings.Contains(string(body), strings.Trim(string(encoded), `"`)) {
							t.Fatalf("GET %s leaked a model path", path)
						}
					}
				}
			}
			check() // loaded, unloaded, failed
			if err := st.sched.Load("d"); err != nil {
				t.Fatal(err)
			}
			schedtest.Eventually(t, 5*time.Second, func() bool {
				sn := st.sched.Snapshot()
				return sn.Phase == scheduler.PhaseLoading && sn.Current == "d"
			})
			check() // loading
		})
	}
}

func TestG8ModelsRespondInstantlyDuringLoad(t *testing.T) {
	st, _ := newRealStack(t, map[string][]string{"a": {"--loading", "5s"}}, nil)
	st.authed(http.MethodGet, "/models", "") // прогрев: первый запрос не замеряется
	start := time.Now()
	resp, body := st.authed(http.MethodPost, "/models/load", `{"model":"a"}`)
	if d := time.Since(start); resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"success":true}` || d > 100*time.Millisecond {
		t.Fatalf("POST /models/load: %d %s in %v (limit 100ms)", resp.StatusCode, body, d)
	}
	for i := range 50 {
		t0 := time.Now()
		resp, body := st.authed(http.MethodGet, "/models", "")
		if d := time.Since(t0); resp.StatusCode != 200 || d > 50*time.Millisecond {
			t.Fatalf("GET /models #%d: %d in %v (limit 50ms): %s", i, resp.StatusCode, d, body)
		}
	}
	if sn := st.sched.Snapshot(); sn.Phase != scheduler.PhaseLoading || sn.Current != "a" {
		t.Fatalf("the load must still be running: %+v", sn)
	}
}

func TestG14LoadDuringLoadingShowsBothLoading(t *testing.T) {
	st, _ := newRealStack(t, map[string][]string{"a": {"--loading", "5s"}}, nil)
	for _, m := range []string{"a", "b"} {
		resp, body := st.authed(http.MethodPost, "/models/load", fmt.Sprintf(`{"model":%q}`, m))
		if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"success":true}` {
			t.Fatalf("load %s: %d %s", m, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/models", "/v1/models"} {
		v := getJSON(t, st, path)
		for _, id := range []string{"a", "b"} {
			if got := statusOf(itemByID(v, id))["value"]; got != "loading" {
				t.Fatalf("%s: %s is %v, want loading", path, id, got)
			}
		}
	}
}

// propsHandler — upstream с GET /props; seen получает "<метод> <путь и query> auth=<Authorization>".
func propsHandler(seen chan<- string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- fmt.Sprintf("%s %s auth=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chat_template":"tmpl"}`)
	})
}

func loadAndWait(t *testing.T, st *stack, model string) {
	t.Helper()
	if err := st.sched.Load(model); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == model
	})
}

func TestPropsForwardsToLoadedModel(t *testing.T) {
	seen := make(chan string, 4)
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: propsHandler(seen)})
	st := newStack(t, schedtest.Config("a", "b"), b, b)
	loadAndWait(t, st, "a")
	for _, path := range []string{"/props?model=a&autoload=false", "/props"} {
		resp, body := st.authed(http.MethodGet, path, "")
		if resp.StatusCode != 200 || string(body) != `{"chat_template":"tmpl"}` {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
		if got, want := <-seen, fmt.Sprintf("GET %s auth=%q", path, ""); got != want {
			t.Fatalf("upstream saw %q, want %q", got, want)
		}
	}
	resp, body := st.authed(http.MethodPost, "/props", "")
	wantError(t, resp, body, 405, "method_not_allowed", "invalid_request_error")
	schedtest.Eventually(t, 2*time.Second, func() bool { return st.sched.Snapshot().Inflight == 0 }) // Lease возвращены
}

func TestPropsNeverLoads(t *testing.T) {
	b := schedtest.New()
	st := newStack(t, schedtest.Config("a", "b"), b, b)
	notLoaded := func(path, model string) {
		t.Helper()
		before := st.sched.Snapshot()
		resp, body := st.authed(http.MethodGet, path, "")
		e := wantError(t, resp, body, 502, "upstream_error", "server_error")
		if want := fmt.Sprintf("llama-server for %q is unreachable: model is not loaded", model); e["message"] != want {
			t.Fatalf("message %v, want %q", e["message"], want)
		}
		sn := st.sched.Snapshot()
		if sn.Phase != before.Phase || sn.Gen != before.Gen || sn.Current != before.Current || sn.Target != before.Target ||
			len(sn.Queue) != len(before.Queue) {
			t.Fatalf("GET %s changed the scheduler: %+v -> %+v", path, before, sn)
		}
	}
	notLoaded("/props?model=a&autoload=false", "a") // Empty
	notLoaded("/props?model=a&autoload=true", "a")  // autoload клиента ничего не меняет
	notLoaded("/props", "")
	resp, body := st.authed(http.MethodGet, "/props?model=zz&autoload=false", "")
	wantError(t, resp, body, 404, "model_not_found", "invalid_request_error")
	if s := b.Starts(); len(s) != 0 {
		t.Fatalf("starts %v", s)
	}

	gate := make(chan struct{})
	b.SetPlan("a", schedtest.Plan{Gate: gate})
	if err := st.sched.Load("a"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 5*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseLoading })
	notLoaded("/props?model=a&autoload=false", "a") // Loading
	close(gate)
	schedtest.Eventually(t, 5*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseReady })
	notLoaded("/props?model=b&autoload=false", "b") // Ready с другой моделью

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := st.sched.Acquire(ctx, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.sched.Load("b"); err != nil {
		t.Fatal(err)
	}
	schedtest.Eventually(t, 2*time.Second, func() bool { return st.sched.Snapshot().Phase == scheduler.PhaseDraining })
	notLoaded("/props?model=a&autoload=false", "a") // Draining: новых Lease старому поколению нет
	l.Release()
	schedtest.Eventually(t, 5*time.Second, func() bool {
		sn := st.sched.Snapshot()
		return sn.Phase == scheduler.PhaseReady && sn.Current == "b"
	})
	if s := b.Starts(); !slices.Equal(s, []string{"a", "b"}) {
		t.Fatalf("starts %v, want only the explicit loads", s)
	}
}

// Как G13: Lease выдан, поколение вышло из обслуживания до проксирования — 502, в upstream запрос не уходит.
func TestPropsStaleLease(t *testing.T) {
	seen := make(chan string, 4)
	granted, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook := func(*http.Request) { once.Do(func() { close(granted); <-release }) }
	b := schedtest.New()
	b.SetPlan("a", schedtest.Plan{Handler: propsHandler(seen)})
	st := newStack(t, schedtest.Config("a"), b, b, withAfterGrant(hook))
	loadAndWait(t, st, "a")
	done := make(chan sent, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, st.url+"/props?model=a&autoload=false", nil)
		if err != nil {
			done <- sent{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- sent{err: err}
			return
		}
		done <- sent{code: resp.StatusCode, body: readAll(resp)}
	}()
	select {
	case <-granted:
	case <-time.After(5 * time.Second):
		t.Fatal("GET /props was not granted")
	}
	if n := st.sched.Snapshot().Inflight; n != 1 {
		t.Fatalf("inflight %d: GET /props must hold a Lease", n)
	}
	b.Exit(1, "boom")
	schedtest.Eventually(t, 5*time.Second, func() bool { _, failed := st.sched.Snapshot().Failed["a"]; return failed })
	close(release)
	r := await(t, done, 5*time.Second)
	if e := decodeError(t, r.body); r.code != 502 || e["code"] != "upstream_error" || e["message"] != `llama-server for "a" exited` {
		t.Fatalf("%d: %v", r.code, e)
	}
	if len(seen) != 0 {
		t.Fatalf("stale request reached upstream: %s", <-seen)
	}
}
