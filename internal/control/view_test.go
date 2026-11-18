package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strigov/strigov-locallm/internal/config"
	"github.com/strigov/strigov-locallm/internal/gguf"
	"github.com/strigov/strigov-locallm/internal/testutil"
)

// §9.5: applied_etag, disk_etag, out_of_sync; ETag на диске пересчитывается только при смене размера или времени.
func TestStateETags(t *testing.T) {
	r := newRig(t, defaultHost())
	v := r.ctl.State()
	if v.AppliedETag != r.etag() || v.DiskETag != v.AppliedETag || v.OutOfSync || !v.PauseSaved || v.Snap == nil || v.Cfg != r.live.Load() {
		t.Fatalf("initial %+v", v)
	}
	r.h.WriteModels(t, twoModels+"  c:\n    path: '{models}\\c.gguf'\n")
	if v = r.ctl.State(); !v.OutOfSync || v.DiskETag != r.etag() {
		t.Fatalf("after a manual edit %+v", v)
	}
	st, err := os.Stat(r.h.ModelsFile)
	if err != nil {
		t.Fatal(err)
	}
	same := strings.Replace(readFile(t, r.h.ModelsFile), "'8192'", "'9999'", 1) // та же длина
	if err := os.WriteFile(r.h.ModelsFile, []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(r.h.ModelsFile, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := r.ctl.State().DiskETag; got != v.DiskETag {
		t.Fatal("ETag recomputed although size and time are the same")
	}
	later := st.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(r.h.ModelsFile, later, later); err != nil {
		t.Fatal(err)
	}
	if got := r.ctl.State().DiskETag; got != config.ETag([]byte(same), true) {
		t.Fatalf("ETag not recomputed: %s", got)
	}
	if err := os.Remove(r.h.ModelsFile); err != nil {
		t.Fatal(err)
	}
	if got := r.ctl.State().DiskETag; got != `"absent"` {
		t.Fatalf("missing file: %s", got)
	}
	l := newRig(t, testutil.HostOptions{Legacy: true, DefaultModel: "a", Models: twoModels, Stubs: []string{"a.gguf", "b.gguf"}})
	if v := l.ctl.State(); v.AppliedETag != "" || v.DiskETag != "" || v.OutOfSync {
		t.Fatalf("legacy %+v", v)
	}
}

// §9.5: GET /api/admin/models — документ файла на диске, hand_only и причины, info из кэша, warnings; массивы не nil
// (F4-D3 ledger первого плана).
func TestModelsView(t *testing.T) {
	hand := "  a:\n    path: '{models}\\a.gguf'\n    args: ['-c', '8192', '--log-file', 'C:\\logs\\x.log']\n" +
		"    profiles:\n      long: ['-c', '262144']\n  b:\n    path: '{models}\\b.gguf'\n"
	r := newRig(t, testutil.HostOptions{DefaultModel: "a", Models: hand, Stubs: []string{"a.gguf", "b.gguf"}})
	v := r.ctl.Models(context.Background())
	if v.Mode != config.ModeFile || v.ModelsFile != r.h.ModelsFile || v.ETag != r.etag() || v.OutOfSync || len(v.DiskErrors) != 0 ||
		v.DiskErrors == nil || v.DefaultModel != "a" || v.DefaultArgs == nil || len(v.ModelsDirs) != 1 || len(v.Models) != 2 {
		t.Fatalf("view %+v", v)
	}
	a, b := v.Models[0], v.Models[1]
	if a.PathHandOnly || !a.ArgsHandOnly || !reflect.DeepEqual(a.HandOnlyReasons, []string{"forbidden flag --log-file"}) ||
		len(a.Profiles) != 1 || a.Profiles[0].HandOnly || a.Profiles[0].Name != "long" {
		t.Fatalf("a %+v", a)
	}
	wantWarn := []config.FieldError{{Field: "models[0].profiles[0].args", Message: "context length is larger than the model's training context (131072)"}}
	if !reflect.DeepEqual(a.Warnings, wantWarn) {
		t.Fatalf("warnings %+v", a.Warnings)
	}
	if a.Info == nil || a.Info.Kind != "model" || a.Info.Architecture != "llama" || a.Info.ContextTrain != 131072 {
		t.Fatalf("info %+v", a.Info)
	}
	if b.HandOnlyReasons == nil || len(b.HandOnlyReasons) != 0 || b.Args == nil || b.Profiles == nil || b.Warnings == nil {
		t.Fatalf("b has null arrays: %+v", b)
	}
	r.h.WriteModels(t, "  a:\n    path: '{models}\\a.gguf'\n")
	if v = r.ctl.Models(context.Background()); len(v.Models) != 1 || !v.OutOfSync || v.ETag != r.etag() {
		t.Fatalf("manual edit %+v", v)
	}
}

// Файл на диске не проходит проверку — применённый документ и ошибки файла; legacy — без ETag.
func TestModelsViewBrokenDisk(t *testing.T) {
	r := newRig(t, defaultHost())
	if err := os.WriteFile(r.h.ModelsFile, []byte("version: 1\nmodels: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := r.ctl.Models(context.Background())
	if len(v.DiskErrors) != 1 || v.DiskErrors[0].Field != "models_file" || len(v.Models) != 2 || v.ETag != r.etag() || !v.OutOfSync {
		t.Fatalf("view %+v", v)
	}
	l := newRig(t, testutil.HostOptions{Legacy: true, DefaultModel: "a", Models: twoModels, Stubs: []string{"a.gguf", "b.gguf"}})
	if lv := l.ctl.Models(context.Background()); lv.Mode != config.ModeLegacy || lv.ETag != "" || lv.OutOfSync || len(lv.Models) != 2 {
		t.Fatalf("legacy %+v", lv)
	}
}

// §9.5: поиск — used_by по базовым именам (путь модели и --mmproj), mmproj не nil.
func TestScanUsedBy(t *testing.T) {
	r := newRig(t, testutil.HostOptions{DefaultModel: "a",
		Models: "  a:\n    path: '{models}\\a.gguf'\n    args: ['--mmproj', '{models}\\mmproj-a.gguf']\n" +
			"    profiles:\n      long: ['-c', '8192']\n  b:\n    path: '{models}\\A.GGUF'\n",
		Stubs: []string{"a.gguf", "mmproj-a.gguf", "free.gguf"}})
	r.ctl.scanner = gguf.NewScanner(gguf.NewCache(0), gguf.Limits{}, nil)
	v := r.ctl.Scan(context.Background())
	if len(v.Roots) != 1 || v.Truncated || len(v.Errors) != 0 || len(v.Files) != 3 {
		t.Fatalf("scan %+v", v)
	}
	used := map[string][]string{}
	for _, f := range v.Files {
		if f.MMProj == nil {
			t.Fatalf("%s: mmproj is nil", f.Path)
		}
		used[filepath.Base(f.Path)] = f.UsedBy
	}
	want := map[string][]string{"a.gguf": {"a", "b"}, "mmproj-a.gguf": {"a"}, "free.gguf": {}}
	if !reflect.DeepEqual(used, want) {
		t.Fatalf("used_by %v", used)
	}
}

// FR1-N1 (ledger планировщика): застрявший обход не держит ответ дольше scanWait; повторный вызов — тоже.
func TestScanBoundedWait(t *testing.T) {
	r := newRig(t, defaultHost())
	release := make(chan struct{})
	var calls atomic.Int32
	epoch := time.Now()
	sc := gguf.NewScanner(gguf.NewCache(0), gguf.Limits{}, func() time.Time {
		if calls.Add(1) == 1 {
			<-release // обход «завис» в системном вызове
			return epoch
		}
		return epoch.Add(time.Hour) // после освобождения срок истёк: диск больше не трогается
	})
	r.ctl.scanner = sc
	t.Cleanup(func() {
		close(release)
		_, _ = sc.Scan(context.Background(), nil) // дождаться конца обхода до удаления каталога
	})
	old := scanWait
	scanWait = 50 * time.Millisecond
	t.Cleanup(func() { scanWait = old })
	start := time.Now()
	v := r.ctl.Scan(context.Background())
	if !v.Truncated || len(v.Errors) != 1 || v.Errors[0].Message != "scan did not finish in time" || len(v.Files) != 0 {
		t.Fatalf("scan %+v", v)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
	if v := r.ctl.Scan(context.Background()); !v.Truncated {
		t.Fatal("second call is not bounded")
	}
}

// Критерий 3: обход закончился, но Canonical пути каталога завис (недоступный сетевой путь модели, добавленной
// вручную) — ответ всё равно не позже scanWait, файлы с пустыми used_by; повторный вызов тоже в срок и не начинает
// второй расчёт used_by.
func TestScanUsedByBoundedWait(t *testing.T) {
	r := newRig(t, defaultHost())
	r.ctl.scanner = gguf.NewScanner(gguf.NewCache(0), gguf.Limits{}, nil)
	release := make(chan struct{})
	var calls atomic.Int32
	oldCanon, oldWait := canonical, scanWait
	canonical = func(string) (string, error) {
		calls.Add(1)
		<-release // Canonical без ctx завис в системном вызове
		return "", errors.New("released")
	}
	scanWait = time.Second
	t.Cleanup(func() {
		close(release)
		r.ctl.usedSem <- struct{}{} // дождаться конца брошенного расчёта
		canonical, scanWait = oldCanon, oldWait
	})
	for i := range 2 {
		start := time.Now()
		v := r.ctl.Scan(context.Background())
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("call %d waited %v", i, d)
		}
		if !v.Truncated || len(v.Files) != 3 || len(v.Errors) == 0 || v.Errors[len(v.Errors)-1].Message != "scan did not finish in time" {
			t.Fatalf("call %d: %+v", i, v)
		}
		for _, f := range v.Files {
			if f.UsedBy == nil || len(f.UsedBy) != 0 {
				t.Fatalf("call %d: %s used_by %v", i, f.Path, f.UsedBy)
			}
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d canonicalisations started, want 1 (one hung worker)", n)
	}
}

// PV4 (§12.2 v2): default_args в GET /api/admin/models — как в locallm.yaml, значения %VAR% наружу не выходят.
func TestModelsViewDefaultArgsNotExpanded(t *testing.T) {
	t.Setenv("LOCALLM_TEST_SECRET", "s3cret-value")
	r := newRig(t, defaultHost())
	data, err := os.ReadFile(r.h.Config)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(data), "default_args: []", "default_args: ['--hf-token', '%LOCALLM_TEST_SECRET%']", 1)
	if err := os.WriteFile(r.h.Config, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(r.h.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Llama.DefaultArgs, []string{"--hf-token", "s3cret-value"}) {
		t.Fatalf("launch args must stay expanded: %q", cfg.Llama.DefaultArgs)
	}
	r.live.Store(cfg)
	if got := r.ctl.Models(context.Background()).DefaultArgs; !reflect.DeepEqual(got, []string{"--hf-token", "%LOCALLM_TEST_SECRET%"}) {
		t.Fatalf("view default_args %q", got)
	}
}
