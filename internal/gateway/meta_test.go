package gateway

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// fakeHeaders — HeaderCache теста: Info по строке пути; запоминает, о каких путях спрашивали.
type fakeHeaders struct {
	mu    sync.Mutex
	info  map[string]gguf.Info
	asked map[string]bool
}

func (f *fakeHeaders) Ready(path string) (gguf.Info, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[string]bool{}
	}
	f.asked[path] = true
	in, ok := f.info[path]
	return in, ok
}

func (f *fakeHeaders) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.asked))
}

// metaOf — meta.n_ctx_train и meta.n_embd модели id в ответе GET path; у элемента нет meta — nil, nil.
func metaOf(t *testing.T, st *stack, path, id string) (ctxTrain, embd any) {
	t.Helper()
	it := itemByID(getJSON(t, st, path), id)
	if it == nil {
		t.Fatalf("%s: no model %q", path, id)
	}
	meta, ok := it["meta"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return meta["n_ctx_train"], meta["n_embd"]
}

// rawAdmin — запрос к /api/admin/* с ключом администратора без *testing.T (для горутин): статус, ETag и тело.
// ifMatch "" — без If-Match.
func rawAdmin(base, method, path, body, ifMatch string) (int, string, string, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), string(data), err
}

// §8.3: у загруженной модели meta.n_ctx_train и meta.n_embd — из кэша заголовков по пути eff(m); у незагруженной
// meta нет; кэш спрашивают только о файле загруженной модели; нет записи или кэша — 0.
func TestModelsMetaFromHeaderCache(t *testing.T) {
	pathA := `C:\models\a.gguf`
	h := &fakeHeaders{info: map[string]gguf.Info{pathA: {Kind: "model", ContextTrain: 131072, EmbeddingLen: 4096}}}
	st := newStack(t, schedtest.Config("a", "b"), schedtest.New(), nil, stackOpt(WithHeaders(h)))
	loadAndWait(t, st, "a")
	for _, path := range []string{"/models", "/v1/models"} {
		if ct, e := metaOf(t, st, path, "a"); ct != float64(131072) || e != float64(4096) {
			t.Fatalf("%s a: n_ctx_train %v, n_embd %v", path, ct, e)
		}
		if ct, e := metaOf(t, st, path, "b"); ct != nil || e != nil {
			t.Fatalf("%s b: meta %v %v", path, ct, e)
		}
	}
	if got := h.paths(); !reflect.DeepEqual(got, []string{pathA}) {
		t.Fatalf("cache asked about %v", got)
	}
	for name, opts := range map[string][]stackOpt{"no entry": {stackOpt(WithHeaders(h))}, "no cache": nil} {
		st := newStack(t, schedtest.Config("b"), schedtest.New(), nil, opts...)
		loadAndWait(t, st, "b")
		for _, path := range []string{"/models", "/v1/models"} {
			if ct, e := metaOf(t, st, path, "b"); ct != float64(0) || e != float64(0) {
				t.Fatalf("%s: %s b: n_ctx_train %v, n_embd %v", name, path, ct, e)
			}
		}
	}
}

// H18, часть GGUF (§6.8): после PUT с новым путём загруженной модели meta — от прежнего файла до перезапуска и от
// нового после. Кэш настоящий; файл нового пути заранее прочитан, поэтому прежние значения идут именно от
// Running.Path, а не от отсутствия записи.
func TestMetaFollowsRunningPath(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	oldPath, newPath := filepath.Join(a.h.ModelsDir, "a.gguf"), filepath.Join(a.h.ModelsDir, "d.gguf")
	testutil.GGUFEmbd(t, oldPath, "llama", 131072, 4096)
	testutil.GGUFEmbd(t, newPath, "llama", 32768, 2048)
	a.loadWait("a")
	wantMeta := func(stage string, ctxTrain, embd float64) {
		t.Helper()
		want := [4]any{ctxTrain, embd, ctxTrain, embd}
		var got [4]any
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			got[0], got[1] = metaOf(t, a.stack, "/models", "a")
			got[2], got[3] = metaOf(t, a.stack, "/v1/models", "a")
			if got == want {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("%s: /models n_ctx_train %v n_embd %v, /v1/models %v %v; want %v %v",
					stage, got[0], got[1], got[2], got[3], ctxTrain, embd)
			}
		}
	}
	wantMeta("before PUT", 131072, 4096)
	resp, body := a.put(a.etag(), docJSON(t, a.model("a", "d.gguf", "-c", "8192"), a.model("b", "b.gguf", "-c", "4096")))
	if v := wantJSON(t, resp, body, http.StatusOK); !reflect.DeepEqual(v["restart_required"], []any{"a"}) {
		t.Fatalf("PUT: %v", v)
	}
	if _, err := a.cache.Get(context.Background(), newPath); err != nil {
		t.Fatal(err)
	}
	if ct, e := metaOf(t, a.stack, "/models", "a"); ct != float64(131072) || e != float64(4096) {
		t.Fatalf("after PUT, before restart: n_ctx_train %v, n_embd %v", ct, e)
	}
	wantMeta("after PUT, before restart", 131072, 4096)
	gen := a.sched.Snapshot().Gen
	a.adminJSON(http.MethodPost, "/api/admin/restart", `{"model":"a"}`, http.StatusAccepted)
	a.waitRestarted("a", gen)
	wantMeta("after restart", 32768, 2048)
}

// H16 (G8 MVP): GET /models не дольше 50 мс, пока идут поиск .gguf по 300 файлам и сохранения models.yaml.
func TestH16ModelsFastDuringSaveAndScan(t *testing.T) {
	a := newAdminStack(t, adminHostOptions(), fakeBackend(schedtest.New()))
	for i := range 300 {
		testutil.GGUFEmbd(t, filepath.Join(a.h.ModelsDir, "bulk", fmt.Sprintf("m%03d.gguf", i)), "llama", 4096, 512)
	}
	a.loadWait("a")
	bodies := []string{
		docJSON(t, a.model("a", "a.gguf", "-c", "8192"), a.model("b", "b.gguf", "-c", "16384")),
		docJSON(t, a.model("a", "a.gguf", "-c", "8192"), a.model("b", "b.gguf", "-c", "4096")),
	}
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	t.Cleanup(halt)
	errs := make(chan error, 2)
	var scans, saves atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // поиск .gguf без перерыва
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			code, _, body, err := rawAdmin(a.url, http.MethodGet, "/api/admin/scan", "", "")
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("scan: status %d %s", code, body)
			}
			if err != nil {
				errs <- err
				return
			}
			scans.Add(1)
		}
	}()
	go func() { // сохранения: GET ETag, PUT с другим -c у b
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			code, etag, body, err := rawAdmin(a.url, http.MethodGet, "/api/admin/models", "", "")
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("GET /api/admin/models: status %d %s", code, body)
			}
			if err == nil {
				code, _, body, err = rawAdmin(a.url, http.MethodPut, "/api/admin/models", bodies[i%2], etag)
				if err == nil && code != http.StatusOK {
					err = fmt.Errorf("PUT /api/admin/models: status %d %s", code, body)
				}
			}
			if err != nil {
				errs <- err
				return
			}
			saves.Add(1)
		}
	}()
	waitFor(t, 60*time.Second, func() bool {
		select {
		case err := <-errs:
			t.Fatal(err)
		default:
		}
		return scans.Load() > 0 && saves.Load() > 0
	})
	getJSON(t, a.stack, "/models") // прогрев соединения
	var worst time.Duration
	for i := range 50 {
		start := time.Now()
		resp, body := a.authed(http.MethodGet, "/models", "")
		d := time.Since(start)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /models #%d: %d %s", i, resp.StatusCode, body)
		}
		if d > 50*time.Millisecond {
			t.Fatalf("GET /models #%d took %v (limit 50 ms; scans %d, saves %d)", i, d, scans.Load(), saves.Load())
		}
		worst = max(worst, d)
	}
	halt()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	t.Logf("worst GET /models: %v; scans %d, saves %d", worst, scans.Load(), saves.Load())
}
