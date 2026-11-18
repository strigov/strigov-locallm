package gguf

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// ggufFile — построитель синтетического GGUF: пары пишутся в body, заголовок с числом пар — в bytes().
type ggufFile struct {
	ver  uint32
	n    uint64
	body bytes.Buffer
}

func newFile(ver uint32) *ggufFile { return &ggufFile{ver: ver} }

func (g *ggufFile) w(v any)       { _ = binary.Write(&g.body, binary.LittleEndian, v) }
func (g *ggufFile) u32(v uint32)  { g.w(v) }
func (g *ggufFile) u64(v uint64)  { g.w(v) }
func (g *ggufFile) s(v string)    { g.u64(uint64(len(v))); g.body.WriteString(v) }
func (g *ggufFile) raw(b ...byte) { g.body.Write(b) }

// kv — пара: ключ, тип, значение пишет val.
func (g *ggufFile) kv(key string, t uint32, val func()) *ggufFile {
	g.n++
	g.s(key)
	g.u32(t)
	val()
	return g
}

func (g *ggufFile) str(key, v string) *ggufFile { return g.kv(key, tString, func() { g.s(v) }) }

func (g *ggufFile) u32kv(key string, v uint32) *ggufFile {
	return g.kv(key, tUint32, func() { g.u32(v) })
}

func (g *ggufFile) bytes() []byte {
	var b bytes.Buffer
	b.WriteString("GGUF")
	le := binary.LittleEndian
	_ = binary.Write(&b, le, g.ver)
	_ = binary.Write(&b, le, uint64(0)) // tensor_count
	_ = binary.Write(&b, le, g.n)
	b.Write(g.body.Bytes())
	return b.Bytes()
}

func (g *ggufFile) write(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, g.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readOK(t *testing.T, p string) Info {
	t.Helper()
	in, err := ReadInfo(context.Background(), p)
	if err != nil {
		t.Fatalf("ReadInfo: %v", err)
	}
	return in
}

// allTypes добавляет по паре каждого типа 0–12, массив строк, массив чисел и массив глубины 3.
func allTypes(g *ggufFile) *ggufFile {
	g.kv("t.u8", tUint8, func() { g.raw(7) })
	g.kv("t.i8", tInt8, func() { g.raw(0xff) })
	g.kv("t.u16", tUint16, func() { g.w(uint16(7)) })
	g.kv("t.i16", tInt16, func() { g.w(int16(-7)) })
	g.kv("t.u32", tUint32, func() { g.u32(7) })
	g.kv("t.i32", tInt32, func() { g.w(int32(-7)) })
	g.kv("t.f32", tFloat32, func() { g.w(float32(1.5)) })
	g.kv("t.bool", tBool, func() { g.raw(1) })
	g.kv("t.str", tString, func() { g.s("hello") })
	g.kv("t.u64", tUint64, func() { g.u64(7) })
	g.kv("t.i64", tInt64, func() { g.w(int64(-7)) })
	g.kv("t.f64", tFloat64, func() { g.w(2.5) })
	g.kv("t.strs", tArray, func() { g.u32(tString); g.u64(3); g.s("a"); g.s(""); g.s("ccc") })
	g.kv("t.ints", tArray, func() { g.u32(tInt32); g.u64(4); g.w([4]int32{1, 2, 3, 4}) })
	g.kv("t.nested", tArray, func() { // массив массивов массивов: вложенность 3
		g.u32(tArray)
		g.u64(1)
		g.u32(tArray)
		g.u64(2)
		g.u32(tUint8)
		g.u64(2)
		g.raw(1, 2)
		g.u32(tUint8)
		g.u64(0)
	})
	return g
}

func TestReadInfoAllTypes(t *testing.T) {
	g := allTypes(newFile(3))
	g.str("general.architecture", "llama").str("general.type", "model").str("general.name", "Tiny Llama")
	g.u32kv("llama.context_length", 4096)
	g.kv("llama.embedding_length", tUint64, func() { g.u64(2048) })
	p := g.write(t, t.TempDir(), "tiny.gguf")
	in := readOK(t, p)
	st, _ := os.Stat(p)
	want := Info{Kind: "model", Architecture: "llama", Name: "Tiny Llama", ContextTrain: 4096, EmbeddingLen: 2048, Size: st.Size()}
	if in != want {
		t.Fatalf("info %+v, want %+v", in, want)
	}
}

func TestReadInfoVersions(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []uint32{2, 3} {
		p := newFile(v).str("general.architecture", "llama").write(t, dir, "v.gguf")
		if in := readOK(t, p); in.Architecture != "llama" || in.Partial {
			t.Fatalf("version %d: %+v", v, in)
		}
	}
	for _, v := range []uint32{0, 1, 4} {
		p := newFile(v).str("general.architecture", "llama").write(t, dir, "v.gguf")
		if _, err := ReadInfo(context.Background(), p); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("version %d: %v", v, err)
		}
	}
	p := filepath.Join(dir, "magic.gguf")
	if err := os.WriteFile(p, append([]byte("GGUG"), make([]byte, 20)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInfo(context.Background(), p); !errors.Is(err, ErrNotGGUF) {
		t.Fatalf("magic: %v", err)
	}
	if _, err := ReadInfo(context.Background(), filepath.Join(dir, "missing.gguf")); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing file: %v (error must not contain the path)", err)
	}
}

// Правило вида §8.1: general.type → clip → имя с mmproj → архитектура → unknown.
func TestReadInfoKindRules(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, file, gtype, arch, want string }{
		{"type model", "a.gguf", "model", "llama", "model"},
		{"type mmproj", "a.gguf", "mmproj", "clip", "mmproj"},
		{"type adapter", "a.gguf", "adapter", "llama", "adapter"},
		{"old projector by clip", "a.gguf", "", "clip", "mmproj"},
		{"projector by name", "MMProj-a.gguf", "", "llama", "mmproj"},
		{"architecture only", "a.gguf", "", "qwen35", "model"},
		{"unknown type value", "a.gguf", "vocab", "llama", "model"},
		{"nothing", "a.gguf", "", "", "unknown"},
	}
	for _, c := range cases {
		g := newFile(3)
		if c.gtype != "" {
			g.str("general.type", c.gtype)
		}
		if c.arch != "" {
			g.str("general.architecture", c.arch)
		}
		if in := readOK(t, g.write(t, dir, c.file)); in.Kind != c.want {
			t.Fatalf("%s: kind %q, want %q", c.name, in.Kind, c.want)
		}
	}
}

// Значения берутся с префиксом архитектуры: у clip нет clip.context_length — 0.
func TestReadInfoArchitecturePrefix(t *testing.T) {
	dir := t.TempDir()
	g := newFile(3).u32kv("qwen.context_length", 9).u32kv("llama.context_length", 4096).u32kv("clip.vision.embedding_length", 1152)
	g.str("general.architecture", "llama")
	if in := readOK(t, g.write(t, dir, "m.gguf")); in.ContextTrain != 4096 || in.EmbeddingLen != 0 {
		t.Fatalf("llama: %+v", in)
	}
	g = newFile(3).str("general.architecture", "clip").u32kv("clip.vision.embedding_length", 1152).u32kv("clip.vision.context_length", 5)
	if in := readOK(t, g.write(t, dir, "p.gguf")); in.ContextTrain != 0 || in.EmbeddingLen != 0 || in.Kind != "mmproj" {
		t.Fatalf("clip: %+v", in)
	}
}

// «Принимается любой целый тип» (§8.1); не целые значения не учитываются.
func TestReadInfoIntegerTypes(t *testing.T) {
	dir := t.TempDir()
	write := map[uint32]func(g *ggufFile){
		tUint8:  func(g *ggufFile) { g.raw(200) },
		tInt8:   func(g *ggufFile) { g.raw(100) },
		tUint16: func(g *ggufFile) { g.w(uint16(200)) },
		tInt16:  func(g *ggufFile) { g.w(int16(200)) },
		tUint32: func(g *ggufFile) { g.u32(200) },
		tInt32:  func(g *ggufFile) { g.w(int32(200)) },
		tUint64: func(g *ggufFile) { g.u64(200) },
		tInt64:  func(g *ggufFile) { g.w(int64(200)) },
	}
	for typ, val := range write {
		g := newFile(3).str("general.architecture", "x")
		g.kv("x.context_length", typ, func() { val(g) })
		want := int64(200)
		if typ == tInt8 {
			want = 100
		}
		if in := readOK(t, g.write(t, dir, "i.gguf")); in.ContextTrain != want {
			t.Fatalf("type %d: %d", typ, in.ContextTrain)
		}
	}
	g := newFile(3).str("general.architecture", "x")
	g.kv("x.context_length", tFloat32, func() { g.w(float32(4096)) })
	g.kv("x.embedding_length", tUint64, func() { g.u64(1 << 63) }) // больше MaxInt64
	if in := readOK(t, g.write(t, dir, "f.gguf")); in.ContextTrain != 0 || in.EmbeddingLen != 0 {
		t.Fatalf("non-integer: %+v", in)
	}
}

// Лимиты §8.1: найденное до лимита возвращается с Partial == true и без ошибки.
func TestReadInfoLimitsArePartial(t *testing.T) {
	dir := t.TempDir()
	start := func() *ggufFile { return newFile(3).str("general.architecture", "llama") }
	cases := map[string]func(g *ggufFile){
		"key over 64 KiB": func(g *ggufFile) { g.kv(strings.Repeat("k", maxKey+1), tUint8, func() { g.raw(1) }) },
		"string over 1 MiB": func(g *ggufFile) {
			g.kv("big", tString, func() { g.s(strings.Repeat("x", maxString+1)) })
		},
		"array nesting 4": func(g *ggufFile) {
			g.kv("deep", tArray, func() {
				for range 3 {
					g.u32(tArray)
					g.u64(1)
				}
				g.u32(tUint8)
				g.u64(0)
			})
		},
		"count times size over budget": func(g *ggufFile) {
			g.kv("huge", tArray, func() { g.u32(tUint64); g.u64(1 << 62) })
		},
		"metadata over 16 MiB": func(g *ggufFile) { // 20 строк по 1 МиБ: число проходит, байты кончаются
			g.kv("tokens", tArray, func() {
				g.u32(tString)
				g.u64(20)
				for range 20 {
					g.s(strings.Repeat("t", maxString))
				}
			})
		},
	}
	for name, add := range cases {
		g := start()
		add(g)
		g.str("general.name", "after the limit")
		in := readOK(t, g.write(t, dir, "l.gguf"))
		if !in.Partial || in.Architecture != "llama" || in.Name != "" {
			t.Fatalf("%s: %+v", name, in)
		}
	}
	old := readTimeout
	readTimeout = 0
	t.Cleanup(func() { readTimeout = old })
	if in := readOK(t, start().write(t, dir, "time.gguf")); !in.Partial {
		t.Fatalf("time limit: %+v", in)
	}
	// Срок — на всё чтение: проверяется и после последней пары (здесь пар нет, tick не вызывается).
	if in := readOK(t, newFile(3).write(t, dir, "empty.gguf")); !in.Partial {
		t.Fatalf("time limit at the end of metadata: %+v", in)
	}
}

func TestReadInfoBadType(t *testing.T) {
	dir := t.TempDir()
	g := newFile(3).str("general.architecture", "llama")
	g.kv("bad", 13, func() { g.u32(0) })
	if _, err := ReadInfo(context.Background(), g.write(t, dir, "b.gguf")); !errors.Is(err, ErrBadType) {
		t.Fatalf("value type 13: %v", err)
	}
	g = newFile(3)
	g.kv("bad", tArray, func() { g.u32(99); g.u64(1) })
	if _, err := ReadInfo(context.Background(), g.write(t, dir, "a.gguf")); !errors.Is(err, ErrBadType) {
		t.Fatalf("element type 99: %v", err)
	}
}

// Обрезанный файл — ErrTruncated при любой длине (нужные ключи стоят в конце, остановки раньше нет).
func TestReadInfoTruncated(t *testing.T) {
	full := allTypes(newFile(3)).str("general.architecture", "llama").bytes()
	p := filepath.Join(t.TempDir(), "cut.gguf")
	for n := 0; n < len(full); n++ {
		if err := os.WriteFile(p, full[:n], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadInfo(context.Background(), p); !errors.Is(err, ErrTruncated) {
			t.Fatalf("cut at %d of %d: %v", n, len(full), err)
		}
	}
}

// Остановка: все нужные ключи найдены — дальше файл не читается (неизвестный тип после них не мешает).
func TestReadInfoStopsWhenAllFound(t *testing.T) {
	g := newFile(3).str("general.architecture", "llama").str("general.type", "model").str("general.name", "n")
	g.kv("split.count", tUint16, func() { g.w(uint16(0)) })
	g.u32kv("llama.context_length", 1).u32kv("llama.embedding_length", 2)
	g.kv("bad", 99, func() {})
	in := readOK(t, g.write(t, t.TempDir(), "s.gguf"))
	if in.Partial || in.ContextTrain != 1 || in.EmbeddingLen != 2 {
		t.Fatalf("%+v", in)
	}
}

// Размер составной модели — сумма шардов по stat (§8.1); перебор до первого отсутствующего.
func TestReadInfoSplitSize(t *testing.T) {
	dir := t.TempDir()
	first := func(name string, count uint16) string {
		g := newFile(3).str("general.architecture", "qwen")
		g.kv("split.count", tUint16, func() { g.w(count) })
		return g.write(t, dir, name)
	}
	p := first("m-00001-of-00003.gguf", 3)
	for i, size := range map[string]int{"m-00002-of-00003.gguf": 100, "m-00003-of-00003.gguf": 200} {
		if err := os.WriteFile(filepath.Join(dir, i), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := os.Stat(p)
	if in := readOK(t, p); in.SplitCount != 3 || in.Size != st.Size()+300 {
		t.Fatalf("all shards: %+v (first %d)", in, st.Size())
	}
	never := func() bool { return false }
	if total, ok := shardSum(p, 3, never); !ok || total != st.Size()+300 {
		t.Fatalf("shardSum %d %v", total, ok)
	}
	if total, ok := shardSum(p, 3, func() bool { return true }); total != 0 || ok { // срок истёк — ни одного stat
		t.Fatalf("expired shardSum %d %v", total, ok)
	}
	checks := 0 // срок истекает после первого stat: перебор останавливается на второй проверке
	midWalk := func() bool { checks++; return checks > 1 }
	if total, ok := shardSum(p, 3, midWalk); total != st.Size() || ok || checks != 2 {
		t.Fatalf("shardSum expiring mid-walk %d %v after %d checks", total, ok, checks)
	}
	if err := os.Remove(filepath.Join(dir, "m-00003-of-00003.gguf")); err != nil {
		t.Fatal(err)
	}
	if in := readOK(t, p); in.Size != st.Size()+100 {
		t.Fatalf("missing shard: %+v", in)
	}
	if _, ok := shardSum(p, 3, never); ok {
		t.Fatal("shards_ok with a missing shard")
	}
	for _, name := range []string{"single.gguf", "x-00001-of-00004.gguf"} { // не шаблон или другое число шардов
		q := first(name, 3)
		qs, _ := os.Stat(q)
		if in := readOK(t, q); in.Size != qs.Size() {
			t.Fatalf("%s: size %d, want %d", name, in.Size, qs.Size())
		}
	}
}

// Правило режима файла шарда (F1R3-D1): годится любой файл, кроме каталога, канала и устройства; облачные
// заглушки с ModeIrregular (OneDrive) — тоже. Настоящую заглушку в тесте не создать: проверка по битам режима.
func TestShardModeOK(t *testing.T) {
	yes := []fs.FileMode{0, 0o644, fs.ModeIrregular, fs.ModeIrregular | 0o644, fs.ModeSymlink}
	no := []fs.FileMode{
		fs.ModeDir, fs.ModeDir | 0o755, fs.ModeNamedPipe, fs.ModeDevice, fs.ModeCharDevice, fs.ModeSocket,
	}
	for _, m := range yes {
		if !shardModeOK(m) {
			t.Errorf("mode %v must be accepted", m)
		}
	}
	for _, m := range no {
		if shardModeOK(m) {
			t.Errorf("mode %v must be rejected", m)
		}
	}
}

// Файл открыт другим процессом на запись — чтение работает; наш дескриптор не мешает удалению.
func TestOpenSharedAllowsWritersAndDelete(t *testing.T) {
	p := newFile(3).str("general.architecture", "llama").write(t, t.TempDir(), "m.gguf")
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := openShared(p)
	if err != nil {
		w.Close()
		t.Fatalf("open while another handle writes: %v", err)
	}
	defer f.Close()
	if in := readOK(t, p); in.Architecture != "llama" {
		t.Fatalf("%+v", in)
	}
	w.Close() // os.OpenFile не даёт FILE_SHARE_DELETE; наш дескриптор — даёт
	if err := os.Remove(p); err != nil {
		t.Fatalf("delete while openShared handle is open: %v", err)
	}
}

// Поля ограничены байтами и остаются верным UTF-8 даже после замены неверных байтов.
func TestReadInfoClipsFields(t *testing.T) {
	for _, s := range []string{strings.Repeat("я", 200), strings.Repeat("a", 255) + "я", strings.Repeat("\xff", 300)} {
		g := newFile(3).str("general.architecture", s).str("general.name", s)
		in := readOK(t, g.write(t, t.TempDir(), "m.gguf"))
		for _, field := range []string{in.Architecture, in.Name} {
			if len(field) > 256 || !utf8.ValidString(field) || field == "" {
				t.Fatalf("invalid field: %q", field)
			}
		}
		if in.Partial {
			t.Fatal("clipping fields must not mark metadata partial")
		}
	}
}

// Обрезанное поле — своя копия: срез не держит в кэше всю строку (до maxString, после замены управляющих
// символов на U+FFFD — втрое больше).
func TestClipDoesNotRetainSource(t *testing.T) {
	const n = 20
	kept := make([]string, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range n {
		src := strings.Repeat("a", 1<<20)
		if i%2 == 1 {
			src = strings.Repeat("\x01", 1<<20)
		}
		kept = append(kept, clip(src))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 1<<20 {
		t.Fatalf("%d clipped fields retain %d bytes", n, grown)
	}
	runtime.KeepAlive(kept)
}

func TestReadInfoReplacesControlRunes(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"terminal controls", "llama\t\n\x1b]0;x\x07", "llama\uFFFD\uFFFD\uFFFD]0;x\uFFFD"},
		{"C0 DEL C1", "a\x00\x7f\u0085\u009bb", "a\uFFFD\uFFFD\uFFFD\uFFFDb"},
		{"replacement fits", strings.Repeat("a", 253) + "\tz", strings.Repeat("a", 253) + "\uFFFD"},
		{"replacement crosses limit", strings.Repeat("a", 254) + "\n", strings.Repeat("a", 254)},
		{"replacement at last byte", strings.Repeat("a", 255) + "\x1b", strings.Repeat("a", 255)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFile(3).str("general.architecture", tc.value).str("general.name", tc.value)
			in := readOK(t, g.write(t, t.TempDir(), "controls.gguf"))
			for _, field := range []string{in.Architecture, in.Name} {
				if field != tc.want || strings.ContainsFunc(field, unicode.IsControl) || len(field) > maxField || !utf8.ValidString(field) {
					t.Errorf("field %q, want %q without controls and within byte limit", field, tc.want)
				}
			}
			if in.Partial {
				t.Error("replacing controls must not mark metadata partial")
			}
		})
	}
}

func TestReadInfoSuffixKeyLimit(t *testing.T) {
	for _, suffix := range []string{sufCtx, sufEmb} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			g := newFile(3).str("general.architecture", "llama")
			for i := range 64 {
				g.u32kv(fmt.Sprintf("other%d%s", i, suffix), 1)
			}
			if in := readOK(t, g.write(t, dir, "full.gguf")); in.Partial {
				t.Fatal("64 keys must fit")
			}
			g.u32kv("other0"+suffix, 2)
			if in := readOK(t, g.write(t, dir, "duplicate.gguf")); in.Partial {
				t.Fatal("an existing key must still fit in a full table")
			}
			g.u32kv("llama"+suffix, 4096).str("general.name", "after limit")
			in := readOK(t, g.write(t, dir, "overflow.gguf"))
			if !in.Partial || in.Architecture != "llama" || in.ContextTrain != 0 || in.EmbeddingLen != 0 || in.Name != "" {
				t.Fatalf("65th distinct key: %+v", in)
			}
		})
	}
}

func TestReadInfoNonPositiveLengthsAreAbsent(t *testing.T) {
	cases := []struct {
		name string
		typ  uint32
		val  any
	}{
		{"int8", tInt8, int8(-5)},
		{"int16", tInt16, int16(-5)},
		{"int32", tInt32, int32(-5)},
		{"int64", tInt64, int64(-5)},
		{"zero", tInt64, int64(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newFile(3).str("general.architecture", "llama")
			for _, suffix := range []string{sufCtx, sufEmb} {
				g.kv("llama"+suffix, tc.typ, func() { g.w(tc.val) })
			}
			in := readOK(t, g.write(t, t.TempDir(), "nonpositive.gguf"))
			if in.ContextTrain != 0 || in.EmbeddingLen != 0 || in.Partial {
				t.Fatalf("nonpositive lengths must be absent: %+v", in)
			}
		})
	}
}

func TestReadInfoLengthBoundaries(t *testing.T) {
	dir := t.TempDir()
	g := newFile(3).str("general.architecture", "llama")
	g.str(strings.Repeat("k", 64<<10), strings.Repeat("v", 1<<20))
	g.str("general.name", "after boundary")
	if in := readOK(t, g.write(t, dir, "boundary.gguf")); in.Partial || in.Name != "after boundary" {
		t.Fatalf("lengths at limits: %+v", in)
	}
	for _, part := range []string{"key", "string", "array count"} {
		g = newFile(3).str("general.architecture", "llama")
		switch part {
		case "key":
			g.n++
			g.u64(^uint64(0)) // длина не переводится в int и не используется для выделения памяти
		case "string":
			g.kv("huge", tString, func() { g.u64(^uint64(0)) })
		case "array count":
			g.kv("huge", tArray, func() { g.u32(tUint8); g.u64(^uint64(0)) })
		}
		if in := readOK(t, g.write(t, dir, "huge.gguf")); !in.Partial || in.Architecture != "llama" {
			t.Fatalf("%s: %+v", part, in)
		}
	}
}

// Даже максимальное число пар ограничено общим бюджетом, а не числом из заголовка.
func TestReadInfoPairCountBudget(t *testing.T) {
	g := newFile(3).str("general.architecture", "llama")
	for range (16<<20)/13 + 1 { // пустой ключ, тип uint8, один байт
		g.kv("", tUint8, func() { g.raw(0) })
	}
	g.n = ^uint64(0)
	if in := readOK(t, g.write(t, t.TempDir(), "pairs.gguf")); !in.Partial || in.Architecture != "llama" {
		t.Fatalf("pair budget: %+v", in)
	}
}

func TestReadInfoCancelledAndErrorsHaveNoPath(t *testing.T) {
	dir := t.TempDir()
	p := newFile(3).str("general.architecture", "llama").write(t, dir, "secret.gguf")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if in, err := ReadInfo(ctx, p); !errors.Is(err, context.Canceled) || in != (Info{}) {
		t.Fatalf("cancelled: %+v, %v", in, err)
	}
	for _, p := range []string{filepath.Join(dir, "secret-missing.gguf"), dir, filepath.Join(dir, "secret\x00.gguf")} {
		_, err := ReadInfo(context.Background(), p)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), dir) {
			t.Fatalf("error must not contain path: %v", err)
		}
	}
}
