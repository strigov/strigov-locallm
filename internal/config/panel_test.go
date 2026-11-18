package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// panelFiles — корень models_dirs с файлами и пути вокруг него (§5.6 спецификации v2).
type panelFiles struct {
	root    string // <tmp>\models
	model   string // root\model.gguf
	mmproj  string // root\mmproj.gguf
	lora1   string // root\lora1.gguf
	lora2   string // root\lora2.gguf
	shard1  string // root\big-00001-of-00002.gguf (второй шард есть)
	lonely  string // root\lonely-00001-of-00002.gguf (второго шарда нет)
	missing string // root\missing.gguf (файла нет)
	percent string // root\100%.gguf
	outside string // <tmp>\outside\x.gguf
	trap    string // <tmp>\models2\x.gguf: имя каталога начинается с имени корня
	linked  string // root\link\x.gguf, где link — junction на <tmp>\outside
	c       *Config
}

func newPanelFiles(t *testing.T) panelFiles {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "models")
	f := panelFiles{
		root:    root,
		model:   filepath.Join(root, "model.gguf"),
		mmproj:  filepath.Join(root, "mmproj.gguf"),
		lora1:   filepath.Join(root, "lora1.gguf"),
		lora2:   filepath.Join(root, "lora2.gguf"),
		shard1:  filepath.Join(root, "big-00001-of-00002.gguf"),
		lonely:  filepath.Join(root, "lonely-00001-of-00002.gguf"),
		missing: filepath.Join(root, "missing.gguf"),
		percent: filepath.Join(root, "100%.gguf"),
		outside: filepath.Join(base, "outside", "x.gguf"),
		trap:    filepath.Join(base, "models2", "x.gguf"),
		linked:  filepath.Join(root, "link", "x.gguf"),
	}
	for _, p := range []string{f.model, f.mmproj, f.lora1, f.lora2, f.shard1, filepath.Join(root, "big-00002-of-00002.gguf"),
		f.lonely, f.percent, f.outside, f.trap} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	junction(t, filepath.Join(root, "link"), filepath.Dir(f.outside))
	canon, err := Canonical(root)
	if err != nil {
		t.Fatal(err)
	}
	f.c = &Config{ModelsDirs: []string{canon}}
	return f
}

// docWith — документ из одной модели "m": путь, args модели и, если profile != nil, профиль "p".
func docWith(path string, args, profile []string) *ModelsDoc {
	m := DocModel{ID: "m", Path: path, Args: args}
	if profile != nil {
		m.Profiles = []DocProfile{{Name: "p", Args: profile}}
	}
	return &ModelsDoc{Models: []DocModel{m}}
}

// PV1, PV4, PV6, PV7 для пути модели (§5.6 спецификации v2).
func TestPanelRootsAndPaths(t *testing.T) {
	f := newPanelFiles(t)
	for _, tc := range []struct {
		name, path, want string // want "" — ошибок нет
	}{
		{"inside", f.model, ""},
		{"inside, other case", strings.ToUpper(f.model), ""},
		{"inside, forward slashes", filepath.ToSlash(f.model), ""},
		{"all shards inside", f.shard1, ""},
		{"percent written as %%", strings.ReplaceAll(f.percent, "%", "%%"), ""},
		{"outside", f.outside, msgPanelOutside},
		{"name prefix of the root", f.trap, msgPanelOutside},
		{"junction to outside", f.linked, msgPanelOutside},
		{"dot-dot out of the root", f.root + `\..\outside\x.gguf`, msgPanelOutside},
		{"missing file", f.missing, msgPanelResolve},
		{"missing second shard", f.lonely, msgPanelResolve},
		{"relative", "model.gguf", msgPanelPlain},
		{"drive-relative", "C:model.gguf", msgPanelPlain},
		{`\\?\ prefix`, `\\?\` + f.model, msgPanelPlain},
		{`\\.\ prefix`, `\\.\` + f.model, msgPanelPlain},
		{"//?/ prefix", "//?/" + filepath.ToSlash(f.model), msgPanelPlain},
		{"alternate data stream", f.model + ":stream", msgPanelPlain},
		{"environment variable", `%LOCALAPPDATA%\x.gguf`, msgPanelEnv},
		{"single percent", f.percent, msgPanelEnv},
		{"control character", f.model + "\t", msgPanelControl},
		{"empty", "", msgPanelControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []FieldError
			if tc.want != "" {
				want = []FieldError{{"models[0].path", tc.want}}
			}
			if errs := CheckPanel(f.c, nil, docWith(tc.path, nil, nil)); !reflect.DeepEqual(errs, want) {
				t.Fatalf("got %+v, want %+v", errs, want)
			}
		})
	}
	// Без models_dirs из панели нельзя выбрать ни один путь.
	if errs := CheckPanel(&Config{}, nil, docWith(f.model, nil, nil)); !reflect.DeepEqual(errs, []FieldError{{"models[0].path", msgPanelOutside}}) {
		t.Fatalf("no roots: %+v", errs)
	}
}

// PV1: все шарды — в каталоге первого. Шард в другом каталоге получается только символьной ссылкой на файл, а она
// требует режима разработчика или прав администратора — без них тест пропускается.
func TestPanelShardsInOneDirectory(t *testing.T) {
	f := newPanelFiles(t)
	target := filepath.Join(f.root, "sub", "part.gguf")
	first := filepath.Join(f.root, "split-00001-of-00002.gguf")
	for _, p := range []string{target, first} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(f.root, "split-00002-of-00002.gguf")); err != nil {
		t.Skipf("file symlinks are not available: %v", err)
	}
	want := []FieldError{{"models[0].path", msgPanelShardDir}}
	if errs := CheckPanel(f.c, nil, docWith(first, nil, nil)); !reflect.DeepEqual(errs, want) {
		t.Fatalf("got %+v, want %+v", errs, want)
	}
}

// PV3, PV7 для файловых аргументов (§5.6) в args модели и профиля; --lora и --control-vector делятся по запятым.
func TestPanelFileArgs(t *testing.T) {
	f := newPanelFiles(t)
	type at struct {
		k   int
		msg string
	}
	for _, tc := range []struct {
		name string
		args []string
		want []at
	}{
		{"mmproj inside", []string{"--mmproj", f.mmproj}, nil},
		{"lora parts inside", []string{"--lora", f.lora1 + "," + f.lora2}, nil},
		{"control-vector inside", []string{"--control-vector", f.lora1}, nil},
		{"underscore spelling inside", []string{"--model_draft", f.model}, nil},
		{"percent pair", []string{"--mmproj", strings.ReplaceAll(f.percent, "%", "%%")}, nil},
		{"mmproj outside", []string{"--mmproj", f.outside}, []at{{1, "--mmproj: file is outside models_dirs"}}},
		{"lora second part outside", []string{"--lora", f.lora1 + "," + f.outside}, []at{{1, "--lora: file is outside models_dirs"}}},
		{"lora empty part", []string{"--lora", f.lora1 + ","}, []at{{1, "--lora: value " + msgPanelPlain}}},
		{"chat template via junction", []string{"--chat-template-file", f.linked}, []at{{1, "--chat-template-file: file is outside models_dirs"}}},
		{"grammar file missing", []string{"--grammar-file", f.missing}, []at{{1, "--grammar-file: cannot resolve the file path"}}},
		{"draft model, underscore, outside", []string{"--model_draft", f.trap}, []at{{1, "--model-draft: file is outside models_dirs"}}},
		{"json schema with a prefix", []string{"-jf", `\\?\` + f.model}, []at{{1, "-jf: value " + msgPanelPlain}}},
		{"mmproj with a stream", []string{"--mmproj", f.mmproj + ":s"}, []at{{1, "--mmproj: value " + msgPanelPlain}}},
		{"mmproj via %VAR%", []string{"--mmproj", `%LOCALAPPDATA%\x.gguf`}, []at{{1, msgPanelEnv}}},
		{"two file flags", []string{"--mmproj", f.outside, "-c", "8192", "--lora", f.trap},
			[]at{{1, "--mmproj: file is outside models_dirs"}, {5, "--lora: file is outside models_dirs"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var model, profile []FieldError
			for _, w := range tc.want {
				model = append(model, FieldError{fmt.Sprintf("models[0].args[%d]", w.k), w.msg})
				profile = append(profile, FieldError{fmt.Sprintf("models[0].profiles[0].args[%d]", w.k), w.msg})
			}
			if errs := CheckPanel(f.c, nil, docWith(f.model, tc.args, nil)); !reflect.DeepEqual(errs, model) {
				t.Errorf("model args: got %+v, want %+v", errs, model)
			}
			if errs := CheckPanel(f.c, nil, docWith(f.model, nil, tc.args)); !reflect.DeepEqual(errs, profile) {
				t.Errorf("profile args: got %+v, want %+v", errs, profile)
			}
		})
	}
}

// PV2 (§5.6): каждый флаг запретного списка — в написании списка и с '_' вместо '-', как флаг и как значение,
// в args модели и профиля.
func TestPanelForbiddenFlags(t *testing.T) {
	f := newPanelFiles(t)
	for _, flag := range forbiddenPanelFlags {
		msg := "flag " + flag + " cannot be set from the panel; edit models.yaml by hand"
		spellings := []string{flag}
		if name, ok := strings.CutPrefix(flag, "--"); ok && strings.Contains(name, "-") {
			spellings = append(spellings, "--"+strings.ReplaceAll(name, "-", "_"))
		}
		for _, s := range spellings {
			for _, tc := range []struct {
				doc  *ModelsDoc
				want FieldError
			}{
				{docWith(f.model, []string{s}, nil), FieldError{"models[0].args[0]", msg}},
				{docWith(f.model, []string{"--alias", s}, nil), FieldError{"models[0].args[1]", msg}},
				{docWith(f.model, nil, []string{"-c", "1024", s}), FieldError{"models[0].profiles[0].args[2]", msg}},
			} {
				if errs := CheckPanel(f.c, nil, tc.doc); !reflect.DeepEqual(errs, []FieldError{tc.want}) {
					t.Errorf("%s: got %+v, want %+v", s, errs, tc.want)
				}
			}
		}
	}
}

// Формы, которые выключают возможность, и обычные флаги проходят; аргумент сравнивается целиком.
func TestPanelAllowedFlags(t *testing.T) {
	f := newPanelFiles(t)
	args := []string{"--no-agent", "-no-ag", "--no-ui-mcp-proxy", "--no-webui-mcp-proxy", "-c", "32768", "-ngl", "99",
		"--jinja", "-ctk", "q8_0", "--alias", "qwen", "--tools-x", "-AG", "-H", "--mmproj", f.mmproj}
	if errs := CheckPanel(f.c, nil, docWith(f.model, args, args)); errs != nil {
		t.Fatalf("errs %+v", errs)
	}
}

// PV4 и PV6 (§5.6) для элементов списка: ошибка у самого элемента.
func TestPanelEnvAndControl(t *testing.T) {
	f := newPanelFiles(t)
	for _, tc := range []struct {
		value, want string // want "" — ошибок нет
	}{
		{"%HF_TOKEN%", msgPanelEnv},
		{"a%b", msgPanelEnv},
		{"%%%", msgPanelEnv},
		{"100%%", ""},
		{"%%HF_TOKEN%%", ""},
		{"", msgPanelControl},
		{"a\tb", msgPanelControl},
		{"a\x00", msgPanelControl},
		{"a\x7f", msgPanelControl},
		{"a\u0085b", ""}, // C1 — не управляющий символ по PV6
		{"привет", ""},
	} {
		var want []FieldError
		if tc.want != "" {
			want = []FieldError{{"models[0].args[1]", tc.want}}
		}
		if errs := CheckPanel(f.c, nil, docWith(f.model, []string{"--alias", tc.value}, nil)); !reflect.DeepEqual(errs, want) {
			t.Errorf("%q: got %+v, want %+v", tc.value, errs, want)
		}
	}
}

// PV8 (§5.6): путь и списки, совпавшие строкой с опорным документом, не проверяются; изменённые — проверяются полностью.
func TestPanelExemptionPV8(t *testing.T) {
	f := newPanelFiles(t)
	handArgs := []string{"--log-file", `C:\logs\llama.log`, "--alias", "%USERNAME%"}
	handProfile := []string{"--tools", "all"}
	ref := &ModelsDoc{Models: []DocModel{{ID: "hand", Path: `%LOCALLM_MODELS%\x.gguf`, Args: handArgs,
		Profiles: []DocProfile{{Name: "agent", Args: handProfile}}}}}
	doc := &ModelsDoc{Models: []DocModel{
		{ID: "renamed", Path: `%LOCALLM_MODELS%\x.gguf`, Args: slices.Clone(handArgs),
			Profiles: []DocProfile{{Name: "agent", Args: slices.Clone(handProfile)}, {Name: "copy", Args: slices.Clone(handArgs)}}},
		{ID: "moved", Path: f.outside, Args: slices.Clone(handProfile)},
		{ID: "changed", Path: strings.ToUpper(`%LOCALLM_MODELS%\x.gguf`), Args: append(slices.Clone(handProfile), "-c", "1024")},
	}}
	want := []FieldError{
		{"models[1].path", msgPanelOutside},
		{"models[2].path", msgPanelEnv},
		{"models[2].args[0]", "flag --tools cannot be set from the panel; edit models.yaml by hand"},
	}
	if errs := CheckPanel(f.c, ref, doc); !reflect.DeepEqual(errs, want) {
		t.Fatalf("got  %+v\nwant %+v", errs, want)
	}
	if errs := CheckPanel(f.c, doc, doc); errs != nil {
		t.Fatalf("a document checked against itself: %+v", errs)
	}
}

// HandOnlyMarks (§5.6): метки без исключения PV8; причины без повторов, в порядке появления.
func TestHandOnlyMarks(t *testing.T) {
	f := newPanelFiles(t)
	doc := &ModelsDoc{Models: []DocModel{
		{ID: "clean", Path: f.model, Args: []string{"--mmproj", f.mmproj},
			Profiles: []DocProfile{{Name: "ok", Args: []string{"-c", "8192"}}, {Name: "net", Args: []string{"--rpc", "host:50052"}}}},
		{ID: "far", Path: f.outside, Args: []string{"--mmproj", f.trap}},
		{ID: "raw", Path: `\\?\` + f.model, Args: []string{"--alias", "a\tb", "--api-prefix", "/x"}},
		{ID: "env", Path: `%LOCALLM_TEST_DIR%\model.gguf`, Profiles: []DocProfile{{Name: "big", Args: []string{"--mmproj", f.missing}}}},
	}}
	want := []HandOnly{
		{Profiles: []bool{false, true}, Reasons: []string{"forbidden flag --rpc"}},
		{Path: true, Args: true, Profiles: []bool{}, Reasons: []string{"outside models_dirs"}},
		{Path: true, Args: true, Profiles: []bool{},
			Reasons: []string{"not a plain absolute path", "control character or empty value", "forbidden flag --api-prefix"}},
		{Path: true, Profiles: []bool{true}, Reasons: []string{"%VAR%", "cannot resolve the path"}},
	}
	if got := HandOnlyMarks(f.c, doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := HandOnlyMarks(f.c, &ModelsDoc{}); len(got) != 0 {
		t.Fatalf("empty document: %+v", got)
	}
}

// §5.5 п. 12: ручной models.yaml с запретными флагами, %VAR% и путями вне models_dirs загружается; панель видит метки,
// а неизменённые значения проходят CheckPanel по PV8.
func TestHandWrittenFileWithForbiddenFlagsLoads(t *testing.T) {
	testEnv(t)
	const hand = `version: 1
models:
  hand:
    path: '%LOCALLM_TEST_DIR%\model.gguf'
    args: ['--log-file', '%LOCALLM_TEST_DIR%\model.bin', '--tools', 'all']
`
	c, err := loadFileMode(t, hand, "models_dirs: ['"+t.TempDir()+"']\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []HandOnly{{Path: true, Args: true, Profiles: []bool{},
		Reasons: []string{"%VAR%", "forbidden flag --log-file", "forbidden flag --tools"}}}
	if got := HandOnlyMarks(c, c.ModelsDoc); !reflect.DeepEqual(got, want) {
		t.Fatalf("marks %+v", got)
	}
	if errs := CheckPanel(c, c.ModelsDoc, c.ModelsDoc); errs != nil {
		t.Fatalf("unchanged hand-written values must pass (PV8): %+v", errs)
	}
	if errs := CheckPanel(c, &ModelsDoc{}, c.ModelsDoc); len(errs) != 4 {
		t.Fatalf("without a matching reference: want 4 errors, got %+v", errs)
	}
}

// §5.5 п. 14: тексты CheckPanel не содержат значений — ни путей, ни аргументов.
func TestPanelMessagesHaveNoValues(t *testing.T) {
	f := newPanelFiles(t)
	const secret = "SENTINEL-panel-Zq4"
	outsideSecret := filepath.Join(filepath.Dir(f.outside), secret+".jinja")
	if err := os.WriteFile(outsideSecret, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--alias", "%" + secret + "%", "--mmproj", secret + ".gguf", "--lora", `\\?\C:\` + secret + ".gguf",
		"--alias", secret + "\x01", "--chat-template-file", outsideSecret, "--hf-token", secret}
	errs := CheckPanel(f.c, nil, docWith(`C:\no-such-dir-`+secret+`\x.gguf`, args, nil))
	if len(errs) != 7 {
		t.Fatalf("want 7 errors, got %+v", errs)
	}
	for _, e := range errs {
		if strings.Contains(e.Error(), secret) {
			t.Fatalf("value leaked: %v", e)
		}
	}
}

// §5.6: 59 флагов сборки 12128, без повторов, в каноническом написании; флаги хоста — в reservedFlags, не здесь.
func TestPanelForbiddenListIsComplete(t *testing.T) {
	if len(forbiddenPanelFlags) != 59 || len(forbiddenPanel) != 59 {
		t.Fatalf("list %d, map %d; want 59", len(forbiddenPanelFlags), len(forbiddenPanel))
	}
	for _, f := range forbiddenPanelFlags {
		if !strings.HasPrefix(f, "-") || normalizeFlag(f) != f || slices.Contains(reservedFlags, f) {
			t.Errorf("bad entry %q", f)
		}
	}
	for _, allowed := range []string{"--no-agent", "-no-ag", "--no-ui-mcp-proxy", "--no-webui-mcp-proxy"} {
		if forbiddenPanel[allowed] {
			t.Errorf("%s must stay allowed", allowed)
		}
	}
}

func TestPanelHelpers(t *testing.T) {
	for p, want := range map[string]bool{
		`C:\m\a.gguf`: true, `C:/m/a.gguf`: true, `\\srv\share\a.gguf`: true,
		`a.gguf`: false, `C:a.gguf`: false, `\m\a.gguf`: false, `\\?\C:\m\a.gguf`: false, `\\.\C:\m\a.gguf`: false,
		`//?/C:/m/a.gguf`: false, `\??\C:\m\a.gguf`: false, `C:\m\a.gguf:ads`: false, `\\srv\share\a:b`: false,
	} {
		if got := plainAbsPath(p); got != want {
			t.Errorf("plainAbsPath(%q) = %v", p, got)
		}
	}
	for s, want := range map[string]bool{"%A%": true, "a%": true, "%%%": true, "%%": false, "100%%": false, "%%A%%": false, "": false} {
		if got := envRef(s); got != want {
			t.Errorf("envRef(%q) = %v", s, got)
		}
	}
	for s, want := range map[string]bool{"": true, "a\x00": true, "\x1f": true, "\x7f": true, "a b": false, "é": false, "\u0085": false} {
		if got := hasControlOrEmpty(s); got != want {
			t.Errorf("hasControlOrEmpty(%q) = %v", s, got)
		}
	}
	for _, tc := range []struct {
		root, p string
		want    bool
	}{
		{`C:\models`, `C:\models\a.gguf`, true},
		{`C:\models`, `c:\MODELS\sub\a.gguf`, true},
		{`C:\models`, `C:\models2\a.gguf`, false},
		{`C:\models`, `C:\models`, false},
		{`C:\models`, `C:\mod`, false},
		{`C:\`, `C:\a.gguf`, true},
		{`\\srv\share\m`, `\\srv\share\m\a.gguf`, true},
		{`\\srv\share\m`, `\\srv\share\mx\a.gguf`, false},
	} {
		if got := within(tc.root, tc.p); got != tc.want {
			t.Errorf("within(%q, %q) = %v", tc.root, tc.p, got)
		}
	}
	collect := func(p string) []string {
		var out []string
		for _, s := range shards(p) {
			out = append(out, s)
		}
		return out
	}
	want := []string{`C:\m\big-00001-of-00003.GGUF`, `C:\m\big-00002-of-00003.GGUF`, `C:\m\big-00003-of-00003.GGUF`}
	if got := collect(`C:\m\big-00001-of-00003.GGUF`); !reflect.DeepEqual(got, want) {
		t.Errorf("shards: %q", got)
	}
	for _, p := range []string{`C:\m\big-00002-of-00003.gguf`, `C:\m\a.gguf`, `C:\m\x-00001-of-00000.gguf`} {
		if got := collect(p); !reflect.DeepEqual(got, []string{p}) {
			t.Errorf("shards(%q) = %q", p, got)
		}
	}
}

// PV3 действует на все файловые флаги, а не только на примеры --mmproj и --lora.
func TestPanelAllFileFlags(t *testing.T) {
	f := newPanelFiles(t)
	for _, flag := range []string{"--mmproj", "-mm", "--model-draft", "-md", "--spec-draft-model", "--lora",
		"--chat-template-file", "--control-vector", "--grammar-file", "-jf", "--json-schema-file", "-lcs", "--lookup-cache-static"} {
		t.Run(flag, func(t *testing.T) {
			if errs := CheckPanel(f.c, nil, docWith(f.model, []string{flag, f.model}, []string{flag, f.model})); errs != nil {
				t.Fatalf("file inside roots: %+v", errs)
			}
			want := []FieldError{
				{"models[0].args[1]", flag + ": file is outside models_dirs"},
				{"models[0].profiles[0].args[1]", flag + ": file is outside models_dirs"},
			}
			if errs := CheckPanel(f.c, nil, docWith(f.model, []string{flag, f.outside}, []string{flag, f.outside})); !reflect.DeepEqual(errs, want) {
				t.Fatalf("file outside roots: got %+v, want %+v", errs, want)
			}
		})
	}
	want := []FieldError{{"models[0].args[1]", "--control-vector: file is outside models_dirs"}}
	if errs := CheckPanel(f.c, nil, docWith(f.model, []string{"--control-vector", f.model + "," + f.outside}, nil)); !reflect.DeepEqual(errs, want) {
		t.Fatalf("control-vector second part: %+v", errs)
	}
}

// PV1: достаточно любого корня; junction внутрь разрешён, наружу — нет (TestPanelRootsAndPaths).
func TestPanelMultipleRootsAndJunctionInside(t *testing.T) {
	f := newPanelFiles(t)
	other, err := Canonical(filepath.Dir(f.outside))
	if err != nil {
		t.Fatal(err)
	}
	f.c.ModelsDirs = append(f.c.ModelsDirs, other)
	if errs := CheckPanel(f.c, nil, docWith(f.outside, []string{"--mmproj", f.linked}, nil)); errs != nil {
		t.Fatalf("second root: %+v", errs)
	}
	junction(t, filepath.Join(f.root, "inside"), f.root)
	p := filepath.Join(f.root, "inside", "model.gguf")
	if errs := CheckPanel(f.c, nil, docWith(p, []string{"--mmproj", p}, nil)); errs != nil {
		t.Fatalf("junction inside root: %+v", errs)
	}
}

// PV8 сравнивает весь список с учётом порядка: замена даже безопасного значения отменяет исключение.
func TestPanelPV8RequiresExactList(t *testing.T) {
	f := newPanelFiles(t)
	ref := docWith(f.model, []string{"--tools", "all", "--alias", "old"}, nil)
	for _, args := range [][]string{
		{"--tools", "all", "--alias", "new"},
		{"--alias", "old", "--tools", "all"},
		{"--tools", "all"},
	} {
		errs := CheckPanel(f.c, ref, docWith(f.model, args, nil))
		if len(errs) != 1 || errs[0].Message != "flag --tools cannot be set from the panel; edit models.yaml by hand" {
			t.Fatalf("changed list must lose PV8: %+v", errs)
		}
	}
}

// PV5 остаётся общей проверкой WithModels, в том числе при точном совпадении с опорным документом панели.
func TestPanelPV8DoesNotBypassEqualsRule(t *testing.T) {
	f := newPanelFiles(t)
	const secret = "SENTINEL-equals-Qp7"
	for _, args := range [][]string{{"--alias=" + secret}, {"--" + secret + "=value"}, {"--hf_token=" + secret}} {
		for _, doc := range []*ModelsDoc{docWith(f.model, args, nil), docWith(f.model, nil, args)} {
			if errs := CheckPanel(f.c, doc, doc); errs != nil {
				t.Fatalf("matching reference: %+v", errs)
			}
			_, errs := f.c.WithModels(doc)
			if len(errs) == 0 || !strings.Contains(errs[0].Message, "the --flag=value form is not allowed") {
				t.Fatalf("WithModels must enforce PV5: %+v", errs)
			}
			for _, e := range errs {
				if strings.Contains(e.Error(), secret) {
					t.Fatalf("argument value leaked: %v", e)
				}
			}
		}
	}
}

func TestPanelNilDocuments(t *testing.T) {
	if errs := CheckPanel(&Config{}, nil, nil); errs != nil {
		t.Fatalf("nil document: %+v", errs)
	}
	if marks := HandOnlyMarks(&Config{}, nil); marks != nil {
		t.Fatalf("nil document: %+v", marks)
	}
}
