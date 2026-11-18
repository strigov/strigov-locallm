package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/scheduler"
	"github.com/strigov/strigov-locallm/internal/scheduler/schedtest"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// wantInUse — PUT документа d отвергнут *scheduler.InUseError с именами want; файл, ETag и оба каталога прежние.
func (r *rig) wantInUse(d *config.ModelsDoc, want ...string) {
	r.t.Helper()
	before, etag := readFile(r.t, r.h.ModelsFile), r.etag()
	_, err := r.ctl.PutModels(admin, etag, d)
	var iu *scheduler.InUseError
	if !errors.As(err, &iu) || strings.Join(iu.Models, ",") != strings.Join(want, ",") {
		r.t.Fatalf("err %v, want in use %q", err, want)
	}
	if readFile(r.t, r.h.ModelsFile) != before || r.etag() != etag || catalogIDs(r.sched.Snapshot().Catalog) != "a,b" ||
		catalogIDs(r.live.Load().Models) != "a,b" {
		r.t.Fatal("file or catalog changed")
	}
	if !strings.Contains(r.log.String(), "code=model_in_use") {
		r.t.Fatal("no rejection in log")
	}
}

// H7 на уровне контроллера: без If-Match, со старым ETag, с верным — файл, копия, оба каталога, новая модель грузится.
func TestPutModelsSavesAndApplies(t *testing.T) {
	r := newRig(t, defaultHost())
	before := r.etag()
	if _, err := r.ctl.PutModels(admin, "", doc()); !errors.Is(err, ErrIfMatchRequired) {
		t.Fatalf("no If-Match: %v", err)
	}
	var ce *config.ChangedError
	if _, err := r.ctl.PutModels(admin, `"sha256:00"`, doc()); !errors.As(err, &ce) || ce.ETag != before {
		t.Fatalf("stale ETag: %v", err)
	}
	next := doc(r.model("a", "a.gguf", "-c", "8192"), r.model("b", "b.gguf", "-c", "4096"), r.model("c", "c.gguf", "-c", "2048"))
	res, err := r.ctl.PutModels(admin, before, next)
	if err != nil {
		t.Fatal(err)
	}
	data := readFile(t, r.h.ModelsFile)
	if data != string(config.EncodeModels(next)) || res.ETag != config.ETag([]byte(data), true) || r.live.Load().ModelsETag != res.ETag {
		t.Fatalf("file or ETag: %+v", res)
	}
	if catalogIDs(r.sched.Snapshot().Catalog) != "a,b,c" || catalogIDs(r.live.Load().Models) != "a,b,c" {
		t.Fatal("catalog not applied")
	}
	if baks, _ := filepath.Glob(r.h.ModelsFile + ".*.bak"); len(baks) != 1 {
		t.Fatalf("backups %q", baks)
	}
	if res.RestartRequired == nil || len(res.Warnings) != 0 || res.Warnings == nil {
		t.Fatalf("result %+v", res)
	}
	if log := r.log.String(); !strings.Contains(log, `msg="models applied" op=put actor=admin_api`) || !strings.Contains(log, "added=c") {
		t.Fatalf("log:\n%s", log)
	}
	r.loadWait("c")
}

// H10: удаление загруженной, загружающейся, ожидаемой модели и переименование загруженной — 409 model_in_use.
func TestPutModelsInUse(t *testing.T) {
	onlyA := func(r *rig) *config.ModelsDoc { return doc(r.model("a", "a.gguf", "-c", "8192")) }
	t.Run("loaded", func(t *testing.T) {
		r := newRig(t, defaultHost())
		r.loadWait("b")
		r.wantInUse(onlyA(r), "b")
	})
	t.Run("renamed", func(t *testing.T) {
		r := newRig(t, defaultHost())
		r.loadWait("b")
		r.wantInUse(doc(r.model("a", "a.gguf", "-c", "8192"), r.model("b2", "b.gguf", "-c", "4096")), "b")
	})
	t.Run("loading", func(t *testing.T) {
		r := newRig(t, defaultHost())
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		r.b.SetPlan("b", schedtest.Plan{Gate: gate})
		if err := r.ctl.Load(admin, "b"); err != nil {
			t.Fatal(err)
		}
		r.wantInUse(onlyA(r), "b")
	})
	t.Run("queued", func(t *testing.T) {
		r := newRig(t, defaultHost())
		r.loadWait("a")
		lease, err := r.sched.Acquire(context.Background(), "a", false) // a занята: b ждёт
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(lease.Release)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			if l, err := r.sched.Acquire(ctx, "b", false); err == nil {
				l.Release()
			}
		}()
		schedtest.Eventually(t, 5*time.Second, func() bool {
			s := r.sched.Snapshot()
			return s.Target == "b" || queueHas(s, "b")
		})
		r.wantInUse(onlyA(r), "b")
	})
}

// H10: удаление и переименование swap.default_model — 409 model_is_default (не 422).
func TestPutModelsDefault(t *testing.T) {
	r := newRig(t, defaultHost())
	for _, d := range []*config.ModelsDoc{
		doc(r.model("b", "b.gguf", "-c", "4096")),
		doc(r.model("a2", "a.gguf", "-c", "8192"), r.model("b", "b.gguf", "-c", "4096")),
	} {
		_, err := r.ctl.PutModels(admin, r.etag(), d)
		var de *DefaultError
		if !errors.As(err, &de) || err.Error() != `model "a" is swap.default_model in locallm.yaml; change it there first` {
			t.Fatalf("err %v", err)
		}
	}
	if catalogIDs(r.live.Load().Models) != "a,b" || !strings.Contains(r.log.String(), "code=model_is_default") {
		t.Fatal("catalog changed or no log")
	}
}

// §9.5 шаг 6: правила панели и MVP — 422 с полями и без значений; файл и каталог прежние.
func TestPutModelsInvalid(t *testing.T) {
	r := newRig(t, defaultHost())
	const secret = "hf_SECRET_value_42"
	outside := filepath.Join(r.h.Dir, "outside.gguf")
	testutil.GGUF(t, outside, "llama", "", 4096)
	before := readFile(t, r.h.ModelsFile)
	for _, tc := range []struct {
		name string
		d    *config.ModelsDoc
		want []config.FieldError
	}{
		{"forbidden flag", doc(r.model("a", "a.gguf", "--hf-token", secret)),
			[]config.FieldError{{Field: "models[0].args[0]", Message: "flag --hf-token cannot be set from the panel; edit models.yaml by hand"}}},
		{"outside models_dirs", doc(r.model("a", "a.gguf"), config.DocModel{ID: "c", Path: outside}),
			[]config.FieldError{{Field: "models[1].path", Message: "file is outside models_dirs"}}},
		{"duplicate id", doc(r.model("a", "a.gguf"), r.model("a", "b.gguf")),
			[]config.FieldError{{Field: "models[1].id", Message: `name "a" is already used by models[0].id`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.ctl.PutModels(admin, r.etag(), tc.d)
			var inv *InvalidError
			if !errors.As(err, &inv) || !reflect.DeepEqual(inv.Details, tc.want) {
				t.Fatalf("err %v", err)
			}
			if strings.Contains(fmt.Sprint(inv.Details), secret) {
				t.Fatal("value leaked")
			}
			if readFile(t, r.h.ModelsFile) != before || catalogIDs(r.live.Load().Models) != "a,b" {
				t.Fatal("file or catalog changed")
			}
		})
	}
	if log := r.log.String(); !strings.Contains(log, "code=config_invalid problems=1") || strings.Contains(log, secret) {
		t.Fatalf("log:\n%s", log)
	}
}

// F5-D6 (ledger первого плана): документ, который проходит пределы, но даёт models.yaml больше 1 МиБ, — 422.
func TestPutModelsEncodedSize(t *testing.T) {
	r := newRig(t, defaultHost())
	args := make([]string, 256)
	for i := range args {
		args[i] = strings.Repeat("'", 2100) // в YAML каждый апостроф удваивается
	}
	_, err := r.ctl.PutModels(admin, r.etag(), doc(r.model("a", "a.gguf", args...), r.model("b", "b.gguf")))
	var inv *InvalidError
	want := []config.FieldError{{Field: "models", Message: "models.yaml would be larger than 1048576 bytes"}}
	if !errors.As(err, &inv) || !reflect.DeepEqual(inv.Details, want) {
		t.Fatalf("err %v", err)
	}
}

// H17 (в): ошибка записи после принятого Reconfigure — откат каталога автомата, файл и live прежние.
func TestPutModelsWriteFailureRollsBack(t *testing.T) {
	r := newRig(t, defaultHost())
	before, etag := readFile(t, r.h.ModelsFile), r.etag()
	onlyA := doc(r.model("a", "a.gguf", "-c", "8192"))
	r.ctl.writeFile = func(string, []byte, string) (string, string, error) { return "", "", errors.New("disk full") }
	_, err := r.ctl.PutModels(admin, etag, onlyA)
	var we *WriteError
	if !errors.As(err, &we) || err.Error() != "cannot write models.yaml: disk full" {
		t.Fatalf("err %v", err)
	}
	if readFile(t, r.h.ModelsFile) != before || catalogIDs(r.sched.Snapshot().Catalog) != "a,b" || catalogIDs(r.live.Load().Models) != "a,b" {
		t.Fatal("state changed")
	}
	if !strings.Contains(r.log.String(), "rolled_back=true") {
		t.Fatal("no rollback in log")
	}
	// Файл изменили вручную между шагами 4 и 9: 412 и тот же откат.
	r.ctl.writeFile = func(string, []byte, string) (string, string, error) {
		return "", "", &config.ChangedError{Path: r.h.ModelsFile, ETag: `"sha256:11"`}
	}
	_, err = r.ctl.PutModels(admin, etag, onlyA)
	var ce *config.ChangedError
	if !errors.As(err, &ce) || catalogIDs(r.sched.Snapshot().Catalog) != "a,b" {
		t.Fatalf("err %v", err)
	}
}

// H17 (а): Load(b) между шагами 7 и 8 — PUT, удаляющий b, получает 409, b грузится.
func TestPutModelsRaceHook(t *testing.T) {
	r := newRig(t, defaultHost())
	r.ctl.beforeReconfigure = func() {
		if err := r.ctl.Load(admin, "b"); err != nil {
			t.Error(err)
		}
	}
	r.wantInUse(doc(r.model("a", "a.gguf", "-c", "8192")), "b")
	schedtest.Eventually(t, 5*time.Second, func() bool {
		s := r.sched.Snapshot()
		return s.Current == "b" && s.Phase == scheduler.PhaseReady
	})
}

// H17 (б): 200 повторов гонки PUT (удаление b) и Load(b). Исход — либо 409 и b в каталоге, либо успех PUT, Load
// получил ErrModelNotFound и b нет в Snapshot. Без gcc это проверка на дым, а не -race.
func TestPutModelsRaceRepeated(t *testing.T) {
	for i := range 200 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r := newRig(t, defaultHost())
			etag := r.etag()
			var putErr, loadErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, putErr = r.ctl.PutModels(admin, etag, doc(r.model("a", "a.gguf", "-c", "8192")))
			}()
			go func() {
				defer wg.Done()
				loadErr = r.ctl.Load(admin, "b")
			}()
			wg.Wait()
			s := r.sched.Snapshot()
			var iu *scheduler.InUseError
			switch {
			case errors.As(putErr, &iu):
				if loadErr != nil || catalogIDs(s.Catalog) != "a,b" {
					t.Fatalf("409: load %v, catalog %s", loadErr, catalogIDs(s.Catalog))
				}
			case putErr == nil:
				if !errors.Is(loadErr, scheduler.ErrModelNotFound) || catalogIDs(s.Catalog) != "a" || s.Current == "b" ||
					s.Target == "b" || (s.Intent != nil && s.Intent.Model == "b") || queueHas(s, "b") {
					t.Fatalf("200: load %v, snapshot %+v", loadErr, s)
				}
			default:
				t.Fatalf("put: %v", putErr)
			}
		})
	}
}

// H11: «Применить файл» — ручная правка применяется; испорченный файл, файл без default_model и файл, удаляющий
// загруженную модель, отвергаются, каталог прежний. Режим legacy — ErrNotMigrated.
func TestReloadModels(t *testing.T) {
	r := newRig(t, defaultHost())
	r.h.WriteModels(t, twoModels+"  c:\n    path: '{models}\\c.gguf'\n")
	res, err := r.ctl.ReloadModels(admin)
	if err != nil {
		t.Fatal(err)
	}
	if catalogIDs(r.sched.Snapshot().Catalog) != "a,b,c" || res.ETag != r.etag() || r.live.Load().ModelsETag != r.etag() {
		t.Fatalf("not applied: %+v", res)
	}
	var inv *InvalidError
	if err := os.WriteFile(r.h.ModelsFile, []byte("version: 1\nmodels: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ctl.ReloadModels(admin); !errors.As(err, &inv) || inv.Details[0].Field != "models_file" {
		t.Fatalf("broken file: %v", err)
	}
	r.h.WriteModels(t, "  b:\n    path: '{models}\\b.gguf'\n")
	if _, err := r.ctl.ReloadModels(admin); !errors.As(err, &inv) || len(inv.Details) != 1 || inv.Details[0].Field != "swap.default_model" {
		t.Fatalf("no default model: %v", err)
	}
	r.loadWait("b")
	r.h.WriteModels(t, "  a:\n    path: '{models}\\a.gguf'\n")
	var iu *scheduler.InUseError
	if _, err := r.ctl.ReloadModels(admin); !errors.As(err, &iu) {
		t.Fatalf("in use: %v", err)
	}
	if catalogIDs(r.sched.Snapshot().Catalog) != "a,b,c" || catalogIDs(r.live.Load().Models) != "a,b,c" {
		t.Fatal("catalog changed")
	}
	legacy := newRig(t, testutil.HostOptions{Legacy: true, DefaultModel: "a", Models: twoModels, Stubs: []string{"a.gguf", "b.gguf"}})
	if _, err := legacy.ctl.ReloadModels(admin); !errors.Is(err, ErrNotMigrated) {
		t.Fatalf("legacy reload: %v", err)
	}
	if _, err := legacy.ctl.PutModels(admin, `"absent"`, doc()); !errors.Is(err, ErrNotMigrated) {
		t.Fatalf("legacy put: %v", err)
	}
}

// H12 на уровне контроллера: перенос legacy → file, повтор — ErrAlreadyMigrated, сбой — *WriteError.
func TestMigrateThroughController(t *testing.T) {
	legacy := testutil.HostOptions{Legacy: true, DefaultModel: "a", Models: twoModels, Stubs: []string{"a.gguf", "b.gguf"}}
	r := newRig(t, legacy)
	res, err := r.ctl.Migrate(admin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := r.live.Load()
	if cfg.ModelsMode != config.ModeFile || res.ModelsFile != r.h.ModelsFile || cfg.ModelsETag != r.etag() ||
		res.ModelsETag != cfg.ModelsETag || catalogIDs(cfg.Models) != "a,b" {
		t.Fatalf("result %+v, config mode %s", res, cfg.ModelsMode)
	}
	if _, err := r.ctl.Migrate(admin); !errors.Is(err, config.ErrAlreadyMigrated) {
		t.Fatalf("repeat: %v", err)
	}
	r2 := newRig(t, legacy)
	r2.ctl.migrate = func(*config.Config) (*config.Config, config.MigrateResult, error) {
		return nil, config.MigrateResult{}, errors.New("boom")
	}
	var we *WriteError
	if _, err := r2.ctl.Migrate(admin); !errors.As(err, &we) || r2.live.Load().ModelsMode != config.ModeLegacy {
		t.Fatalf("failure: %v", err)
	}
}

// §5.8 п. 4 и §5.5 п. 14: файл модели исчез после старта, config.Migrate перечитывает конфиг в стиле файла, и его
// ошибка содержит путь после раскрытия %VAR%. Ответ — ровно "migration check failed"; значение переменной (секрет)
// не попадает ни в ответ, ни в журнал; файлы не тронуты.
func TestMigrateHidesPaths(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "s3cr3t-7f3a")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALLM_TEST_SECRET", secret)
	testutil.GGUF(t, filepath.Join(secret, "a.gguf"), "llama", "", 131072)
	r := newRig(t, testutil.HostOptions{Legacy: true, DefaultModel: "a",
		Models: "  a:\n    path: '%LOCALLM_TEST_SECRET%\\a.gguf'\n"})
	if err := os.Remove(filepath.Join(secret, "a.gguf")); err != nil {
		t.Fatal(err)
	}
	_, err := r.ctl.Migrate(admin)
	var we *WriteError
	if !errors.As(err, &we) || err.Error() != "migration check failed" || r.live.Load().ModelsMode != config.ModeLegacy {
		t.Fatalf("migrate: %v", err)
	}
	if _, serr := os.Stat(r.h.ModelsFile); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("models.yaml written: %v", serr)
	}
	log := r.log.String()
	if !strings.Contains(log, `msg="models migration failed"`) {
		t.Fatalf("no log line:\n%s", log)
	}
	for _, s := range []string{err.Error(), log} {
		if strings.Contains(strings.ToLower(s), "s3cr3t") || strings.Contains(s, "a.gguf") {
			t.Fatalf("path leaked: %s", s)
		}
	}
}

// §8.4: проектор и адаптер вместо модели, --mmproj не проектор, контекст больше обучающего, нечитаемый заголовок.
func TestWarnings(t *testing.T) {
	r := newRig(t, defaultHost())
	dir := r.h.ModelsDir
	testutil.GGUF(t, filepath.Join(dir, "proj.gguf"), "clip", "", 0)
	testutil.GGUF(t, filepath.Join(dir, "lora.gguf"), "llama", "adapter", 0)
	testutil.StubFile(t, filepath.Join(dir, "broken.gguf"))
	d := doc(
		r.model("a", "a.gguf", "-c", "262144", "--mmproj", filepath.Join(dir, "lora.gguf")),
		r.model("b", "proj.gguf"),
		r.model("c", "broken.gguf"),
		r.model("d", "lora.gguf"),
	)
	d.Models[0].Profiles = []config.DocProfile{
		{Name: "p", Args: []string{"--mmproj", filepath.Join(dir, "proj.gguf")}},
		{Name: "q", Args: []string{"--mmproj", filepath.Join(dir, "broken.gguf")}},
	}
	res, err := r.ctl.PutModels(admin, r.etag(), d)
	if err != nil {
		t.Fatal(err)
	}
	const header = "cannot read the GGUF header: "
	want := []config.FieldError{
		{Field: "models[0].args", Message: "context length is larger than the model's training context (131072)"},
		{Field: "models[0].args[3]", Message: "--mmproj: file is not a projector (mmproj)"},
		{Field: "models[0].profiles[1].args[1]", Message: "--mmproj: " + header},
		{Field: "models[1].path", Message: "file is a projector (mmproj), not a model"},
		{Field: "models[2].path", Message: header},
		{Field: "models[3].path", Message: "file is an adapter, not a model"},
	}
	if len(res.Warnings) != len(want) {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	for i, w := range want {
		got := res.Warnings[i]
		if got.Field != w.Field || (got.Message != w.Message && !(strings.HasSuffix(w.Message, header) && strings.HasPrefix(got.Message, w.Message))) {
			t.Errorf("warning %d: %+v, want %+v", i, got, w)
		}
	}
}

// Предупреждения ждут заголовки не дольше warnTimeout, даже если Cache.Get завис до чтения заголовка (Canonical или
// os.Stat на недоступном пути, ctx там не проверяется): PUT отвечает в срок, modelsMu свободен, следующая смена
// каталога проходит.
func TestWarningsBoundedWait(t *testing.T) {
	r := newRig(t, defaultHost())
	r.ctl.warnTimeout = 100 * time.Millisecond
	release := make(chan struct{})
	old := headerGet
	headerGet = func(*gguf.Cache, context.Context, string) (gguf.Info, error) {
		<-release // ctx не смотрит: так висит системный вызов до чтения заголовка
		return gguf.Info{}, errors.New("released")
	}
	t.Cleanup(func() { close(release); headerGet = old })
	start := time.Now()
	res, err := r.ctl.PutModels(admin, r.etag(), doc(r.model("a", "a.gguf", "-c", "8192"), r.model("b", "b.gguf")))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second || len(res.Warnings) != 0 {
		t.Fatalf("PUT took %v, warnings %+v", d, res.Warnings)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.ctl.ReloadModels(admin)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("modelsMu is still held after the warnings timeout")
	}
}

// §6.8: буферы вывода удалённых имён забываются после сохранения.
func TestPutForgetsRemovedLogs(t *testing.T) {
	r := newRig(t, defaultHost())
	r.b.AppendLog("b", "old line")
	if _, err := r.ctl.PutModels(admin, r.etag(), doc(r.model("a", "a.gguf", "-c", "8192"))); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.b.Logs("b", 10); ok {
		t.Fatal("logs of removed b kept")
	}
}
