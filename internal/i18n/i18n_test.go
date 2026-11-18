package i18n

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Каждый текст есть на каждом языке; число глаголов %… совпадает с русским.
func TestEveryKeyTranslated(t *testing.T) {
	for _, l := range All {
		for k := Key(0); k < keyCount; k++ {
			s := l.T(k)
			if s == "" {
				t.Errorf("%s: key %d is empty", l, k)
			}
			if got, want := strings.Count(s, "%"), strings.Count(RU.T(k), "%"); got != want {
				t.Errorf("%s: key %d has %d verbs, ru has %d: %q", l, k, got, want, s)
			}
		}
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Lang{"ru": RU, "EN": EN, " zh\n": ZH, "zh-CN": ZH, "en-US": EN, "ru_RU": RU} {
		if got, ok := Parse(in); !ok || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "de", "english", "r"} {
		if _, ok := Parse(in); ok {
			t.Errorf("Parse(%q) must fail", in)
		}
	}
}

func TestNative(t *testing.T) {
	if RU.Native() != "Русский" || EN.Native() != "English" || ZH.Native() != "中文" {
		t.Fatal("native names")
	}
}

// Пункт подменю языка понятен и тому, кто случайно выбрал незнакомый язык: в нём всегда есть «Language».
func TestLanguageMenuTitleHasEnglish(t *testing.T) {
	for _, l := range All {
		if !strings.Contains(l.T(MenuLanguage), "Language") {
			t.Errorf("%s: %q", l, l.T(MenuLanguage))
		}
	}
}

func TestCurrentDefaultsToRussian(t *testing.T) {
	if Current() != RU {
		t.Fatalf("default %q", Current())
	}
	Set(ZH)
	defer Set(RU)
	if Current() != ZH {
		t.Fatal("Set")
	}
}

func TestF(t *testing.T) {
	if got := EN.F(StatusReady, "qwen"); got != "Ready: qwen" {
		t.Fatal(got)
	}
	if got := RU.F(ConfirmForce, 2); got != "Будут прерваны незаконченные ответы: 2. Продолжить?" {
		t.Fatal(got)
	}
}

func TestSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "lang")
	if _, ok := Load(p); ok {
		t.Fatal("missing file must not load")
	}
	if err := Save(p, EN); err != nil {
		t.Fatal(err)
	}
	if l, ok := Load(p); !ok || l != EN {
		t.Fatalf("Load = %q, %v", l, ok)
	}
	if err := os.WriteFile(p, []byte("klingon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(p); ok {
		t.Fatal("unknown language must not load")
	}
}

func TestDetectIsKnown(t *testing.T) {
	if _, ok := Parse(string(Detect())); !ok {
		t.Fatalf("Detect = %q", Detect())
	}
}

func TestFromLangID(t *testing.T) {
	for id, want := range map[uint16]Lang{0x0419: RU, 0x0804: ZH, 0x0404: ZH, 0x0409: EN, 0x0407: EN, 0x0422: EN} {
		if got := fromLangID(id); got != want {
			t.Errorf("fromLangID(%#x) = %q, want %q", id, got, want)
		}
	}
}
