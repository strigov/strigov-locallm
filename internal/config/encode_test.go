package config

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// exampleModelsYAML — пример §5.3 спецификации v2: ровно его пишет панель для этого каталога.
const exampleModelsYAML = `# models.yaml — модели locallm-host. Файл перезаписывает веб-панель, комментарии при этом не сохраняются.
# После ручной правки: панель → «Применить файл» или перезапуск хоста.
version: 1
models:
  qwen-coder:
    path: 'C:\models\unsloth--Qwen3.8-27B-GGUF\Qwen3.8-27B-UD-IQ3_XXS.gguf'
    args: ['-c', '32768']
    profiles:
      long: ['-c', '65536', '-ctk', 'q8_0', '-ctv', 'q8_0']
  gemma:
    path: 'C:\models\google-- gemma-4-26B-q4-0-it-GGUF\gemma-4-26B_q4_0-it.gguf'
    args: ['-c', '16384', '--mmproj', 'C:\models\google-- gemma-4-26B-q4-0-it-GGUF\mmproj-gemma-4-26B-A4B-it-Q8_0.gguf']
  tiny:
    path: 'C:\models\LiquidAI--LFM2.5-2.6B-GGUF\LFM2.5-2.6B-QAD-Q4_0.gguf'
`

// §5.7 спецификации v2: детерминированный формат панели.
func TestEncodeModelsFormat(t *testing.T) {
	doc := &ModelsDoc{Models: []DocModel{
		{ID: "qwen-coder", Path: `C:\models\unsloth--Qwen3.8-27B-GGUF\Qwen3.8-27B-UD-IQ3_XXS.gguf`, Args: []string{"-c", "32768"},
			Profiles: []DocProfile{{Name: "long", Args: []string{"-c", "65536", "-ctk", "q8_0", "-ctv", "q8_0"}}}},
		{ID: "gemma", Path: `C:\models\google-- gemma-4-26B-q4-0-it-GGUF\gemma-4-26B_q4_0-it.gguf`,
			Args: []string{"-c", "16384", "--mmproj", `C:\models\google-- gemma-4-26B-q4-0-it-GGUF\mmproj-gemma-4-26B-A4B-it-Q8_0.gguf`}},
		{ID: "tiny", Path: `C:\models\LiquidAI--LFM2.5-2.6B-GGUF\LFM2.5-2.6B-QAD-Q4_0.gguf`},
	}}
	if got := string(EncodeModels(doc)); got != exampleModelsYAML {
		t.Fatalf("got:\n%s\nwant:\n%s", got, exampleModelsYAML)
	}
	if parsed, errs := ParseModels([]byte(exampleModelsYAML)); errs != nil || !reflect.DeepEqual(parsed, doc) {
		t.Fatalf("parsed %+v errs %v", parsed, errs)
	}
	empty := modelsHeader + "version: 1\nmodels: {}\n"
	for _, d := range []*ModelsDoc{nil, {}} {
		if got := string(EncodeModels(d)); got != empty {
			t.Fatalf("empty catalog:\n%s", got)
		}
	}
	got := string(EncodeModels(&ModelsDoc{Models: []DocModel{{ID: "a", Path: `C:\a.gguf`, Args: []string{}, Profiles: []DocProfile{{Name: "p"}}}}}))
	if want := modelsHeader + "version: 1\nmodels:\n  a:\n    path: 'C:\\a.gguf'\n    profiles:\n      p: []\n"; got != want {
		t.Fatalf("empty lists:\n%s", got)
	}
}

// §5.7: имена — без кавычек, только если yaml.v3 прочитает их строкой; строки — в одинарных кавычках, с управляющими
// символами, NEL, U+2028/U+2029, BOM и U+FFFE/U+FFFF — в двойных с экранированием.
func TestEncodeModelsQuoting(t *testing.T) {
	for in, want := range map[string]string{
		"qwen-coder": "qwen-coder", "Q_1.5b": "Q_1.5b", "a.b": "a.b",
		"true": "'true'", "1.5": "'1.5'", "null": "'null'", "1": "'1'", "0x1F": "'0x1F'", "1e3": "'1e3'",
		"~": "'~'", "<<": "'<<'", "-x": "'-x'", "a b": "'a b'", "it's": "'it''s'", "": "''",
	} {
		if got := encodeName(in); got != want {
			t.Errorf("encodeName(%q) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		`C:\m\a b.gguf`: `'C:\m\a b.gguf'`,
		"it's":          `'it''s'`,
		"":              `''`,
		"#not comment":  `'#not comment'`,
		"é ü 中":         `'é ü 中'`,
		"a\tb":          `"a\x09b"`,
		"line\nbreak":   `"line\x0Abreak"`,
		"q\"\\\x7f":     `"q\"\\\x7F"`,
		"nel\u0085":     `"nel\u0085"`,
		"ls\u2028":      `"ls\u2028"`,
		"bom\ufeff":     `"bom\uFEFF"`,
		"x\uffff":       `"x\uFFFF"`,
	} {
		if got := encodeString(in); got != want {
			t.Errorf("encodeString(%q) = %s, want %s", in, got, want)
		}
	}
	doc := &ModelsDoc{}
	for _, name := range []string{"true", "1.5", "null", "~", "<<", "it's", ""} {
		doc.Models = append(doc.Models, DocModel{ID: name, Path: "a\tb\u2028c", Args: []string{"it's", "q\"\\\x7f", "nel\u0085"},
			Profiles: []DocProfile{{Name: name, Args: []string{"bom\ufeff"}}}})
	}
	if got, errs := ParseModels(EncodeModels(doc)); errs != nil || !reflect.DeepEqual(got, doc) {
		t.Fatalf("round trip: errs %v\ngot  %#v\nwant %#v", errs, got, doc)
	}
}

// randomString — до 11 символов из набора, который ломает YAML.
func randomString(r *rand.Rand) string {
	pool := []rune("aZ09 -_.:#%'\"\\[]{},&*!|>@`\t\n\r\x01\x7f\u0085\u00a0\u2028\u2029\ufeff\ufffe\uffffé中😀")
	var b strings.Builder
	for range r.IntN(12) {
		b.WriteRune(pool[r.IntN(len(pool))])
	}
	return b.String()
}

// randomDoc — случайный документ: до 4 моделей, до 2 профилей, до 3 аргументов; пустые списки — nil, как у ParseModels.
func randomDoc(r *rand.Rand) *ModelsDoc {
	names := []string{"true", "1.5", "null", "~", "0x1F", "1e3", "-x", ".inf", "<<", "a b", "it's", "qwen-coder", "Q_1.5b", "", "yes"}
	pick := func(used map[string]bool) string {
		for {
			s := randomString(r)
			if r.IntN(2) == 0 {
				s = names[r.IntN(len(names))]
			}
			if !used[s] {
				used[s] = true
				return s
			}
		}
	}
	list := func() []string {
		n := r.IntN(4)
		if n == 0 {
			return nil
		}
		out := make([]string, n)
		for i := range out {
			out[i] = randomString(r)
		}
		return out
	}
	doc := &ModelsDoc{}
	models := map[string]bool{}
	for range r.IntN(5) {
		m := DocModel{ID: pick(models), Path: randomString(r), Args: list()}
		profiles := map[string]bool{}
		for range r.IntN(3) {
			m.Profiles = append(m.Profiles, DocProfile{Name: pick(profiles), Args: list()})
		}
		doc.Models = append(doc.Models, m)
	}
	return doc
}

// §5.7: EncodeModels → ParseModels → тот же документ.
func TestEncodeModelsRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 1000 {
		doc := randomDoc(r)
		data := EncodeModels(doc)
		if got, errs := ParseModels(data); errs != nil || !reflect.DeepEqual(got, doc) {
			t.Fatalf("iteration %d: errs %v\ndoc %#v\ngot %#v\ntext:\n%s", i, errs, doc, got, data)
		}
	}
}
