// Package gguf — чтение метаданных GGUF, кэш и поиск файлов моделей (§8 спецификации v2).
package gguf

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Info — метаданные файла GGUF (§4.4 спецификации v2).
type Info struct {
	Kind         string // "model", "mmproj", "adapter", "unknown"
	Architecture string // до 256 байт, верный UTF-8; управляющие символы заменены на U+FFFD
	Name         string // до 256 байт, верный UTF-8; управляющие символы заменены на U+FFFD
	ContextTrain int64  // <arch>.context_length; значения <= 0 — нет (0)
	EmbeddingLen int64  // <arch>.embedding_length; значения <= 0 — нет (0)
	SplitCount   int
	Size         int64 // байты; у составной модели — сумма шардов
	Partial      bool  // чтение остановлено лимитом (§8.1)
}

var (
	ErrNotGGUF     = errors.New("not a GGUF file")
	ErrUnsupported = errors.New("unsupported GGUF version")
	ErrTruncated   = errors.New("truncated GGUF metadata")
	ErrBadType     = errors.New("unknown GGUF value type")
)

// Лимиты §8.1.
const (
	maxMeta       = 16 << 20 // байт метаданных, считая заголовок
	maxKey        = 64 << 10
	maxString     = 1 << 20
	maxDepth      = 3   // вложенность массивов
	maxField      = 256 // сохраняемая длина architecture и name
	maxSuffixKeys = 64  // разных ключей в каждой таблице суффиксов; новый сверх лимита — Partial
	bufSize       = 64 << 10
)

// readTimeout — лимит времени одного чтения (§8.1); тесты подменяют.
var readTimeout = 2 * time.Second

// errLimit — сработал лимит: вернуть найденное с Partial (§8.1).
var errLimit = errors.New("gguf limit reached")

// Типы значений GGUF (§8.1).
const (
	tUint8 uint32 = iota
	tInt8
	tUint16
	tInt16
	tUint32
	tInt32
	tFloat32
	tBool
	tString
	tArray
	tUint64
	tInt64
	tFloat64
)

// shardRe — шаблон шардов MVP §5.3 п. 4 (тот же, что config.shardRe).
var shardRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)

// ReadInfo читает только метаданные (§8.1). Файл открывается с общим доступом на чтение, запись и удаление.
// До чтения отклоняются недисковые объекты и каталоги.
// Сработал лимит — найденное с Partial == true и без ошибки. Ошибка не содержит пути.
func ReadInfo(ctx context.Context, path string) (Info, error) {
	f, err := openShared(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	if err := checkDiskFile(f); err != nil {
		return Info{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return Info{}, fmt.Errorf("stat: %w", unwrapPath(err))
	}
	if st.IsDir() {
		return Info{}, errors.New("not a regular disk file")
	}
	deadline := time.Now().Add(readTimeout) // один срок §8.1 на метаданные и обход шардов
	in, err := decode(ctx, f, filepath.Base(path), deadline)
	if err != nil {
		return Info{}, err
	}
	in.Size = st.Size()
	if in.SplitCount > 1 {
		expired := func() bool { return !time.Now().Before(deadline) }
		if total, _ := shardSum(path, in.SplitCount, expired); total > 0 {
			in.Size = total
		}
	}
	if !time.Now().Before(deadline) { // срок истёк после последней пары или при обходе шардов
		in.Partial = true
	}
	return in, nil
}

// unwrapPath убирает путь из *os.PathError: ошибки пакета пути не содержат.
func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// shardSum — сумма размеров шардов составной модели по stat (§8.1) и все ли они на месте (§8.2 shards_ok).
// Имя не по шаблону или число шардов в имени не равно count — (0, false). Перебор останавливается на первом
// отсутствующем шарде. Срок: expired вызывается перед каждым stat (ReadInfo — срок 2 с по time.Now, поиск — свой
// срок 30 с по подменяемым часам); истёк — перебор останавливается с ok == false, дальше диск не трогается.
func shardSum(path string, count int, expired func() bool) (int64, bool) {
	dir, base := filepath.Split(path)
	m := shardRe.FindStringSubmatchIndex(base)
	if m == nil {
		return 0, false
	}
	if n, err := strconv.Atoi(base[m[4]:m[5]]); err != nil || n != count {
		return 0, false
	}
	var total int64
	for k := 1; k <= count; k++ {
		if expired() {
			return total, false
		}
		name := base[:m[2]] + fmt.Sprintf("%05d", k) + base[m[3]:]
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !shardModeOK(st.Mode()) {
			return total, false
		}
		total += st.Size()
	}
	return total, true
}

// shardModeOK — режим файла шарда (F1R3-D1): годится любой файл, кроме каталога, канала и устройства. Облачные
// заглушки (ModeIrregular) проходят: их размер известен без скачивания. Не путать с обходом поиска (§8.2) — тот
// пропускает точки повторной обработки по атрибуту файла до всякого stat.
func shardModeOK(m fs.FileMode) bool {
	return m&(fs.ModeDir|fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice|fs.ModeSocket) == 0
}

// decoder читает метаданные с учётом лимитов §8.1; n — прочитано байт, считая заголовок.
type decoder struct {
	ctx      context.Context
	r        *bufio.Reader
	n        int64
	deadline time.Time
	buf      [8]byte
}

// found — нужные ключи (§8.1).
type found struct {
	arch, gtype, name         string
	hasArch, hasType, hasName bool
	split                     int
	hasSplit                  bool
	ctxLen, embLen            map[string]int64 // префикс ключа → значение
}

const (
	sufCtx = ".context_length"
	sufEmb = ".embedding_length"
)

// decode разбирает метаданные из r; base — имя файла для правила вида, deadline — срок §8.1.
func decode(ctx context.Context, r io.Reader, base string, deadline time.Time) (Info, error) {
	d := &decoder{ctx: ctx, r: bufio.NewReaderSize(r, bufSize), deadline: deadline}
	f := &found{ctxLen: map[string]int64{}, embLen: map[string]int64{}}
	err := d.meta(f)
	if err != nil && !errors.Is(err, errLimit) {
		return Info{}, err
	}
	in := f.info(base)
	in.Partial = err != nil
	return in, nil
}

func (d *decoder) meta(f *found) error {
	var magic [4]byte
	if err := d.read(magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != "GGUF" {
		return ErrNotGGUF
	}
	ver, err := d.u32()
	if err != nil {
		return err
	}
	if ver != 2 && ver != 3 {
		return fmt.Errorf("%w %d", ErrUnsupported, ver)
	}
	if _, err := d.u64(); err != nil { // tensor_count
		return err
	}
	kv, err := d.u64()
	if err != nil {
		return err
	}
	var key []byte
	for i := uint64(0); i < kv && !f.done(); i++ {
		if err := d.tick(); err != nil {
			return err
		}
		kl, err := d.u64()
		if err != nil {
			return err
		}
		if kl > maxKey {
			return errLimit
		}
		if uint64(cap(key)) < kl {
			key = make([]byte, kl)
		}
		key = key[:kl]
		if err := d.read(key); err != nil {
			return err
		}
		t, err := d.u32()
		if err != nil {
			return err
		}
		if err := f.pair(d, string(key), t); err != nil {
			return err
		}
	}
	return nil
}

// pair читает значение пары и запоминает нужное.
func (f *found) pair(d *decoder, key string, t uint32) error {
	switch key {
	case "general.architecture", "general.type", "general.name":
		if t != tString {
			_, _, err := d.value(t, 0)
			return err
		}
		s, err := d.str(true)
		if err != nil {
			return err
		}
		s = clip(s)
		switch key {
		case "general.architecture":
			f.arch, f.hasArch = s, true
		case "general.type":
			f.gtype, f.hasType = s, true
		default:
			f.name, f.hasName = s, true
		}
		return nil
	}
	v, isInt, err := d.value(t, 0)
	if err != nil || !isInt {
		return err
	}
	switch {
	case key == "split.count":
		if v >= 0 && v <= 99999 {
			f.split, f.hasSplit = int(v), true
		}
	case strings.HasSuffix(key, sufCtx):
		return put(f.ctxLen, strings.TrimSuffix(key, sufCtx), v)
	case strings.HasSuffix(key, sufEmb):
		return put(f.embLen, strings.TrimSuffix(key, sufEmb), v)
	}
	return nil
}

// put сохраняет длину (неположительная — 0); новый ключ сверх лимита останавливает чтение с Partial.
func put(m map[string]int64, k string, v int64) error {
	if _, ok := m[k]; !ok && len(m) >= maxSuffixKeys {
		return errLimit
	}
	if v <= 0 {
		v = 0
	}
	m[k] = v
	return nil
}

// done — все нужные ключи найдены (§8.1: остановка).
func (f *found) done() bool {
	if !f.hasArch || !f.hasType || !f.hasName || !f.hasSplit {
		return false
	}
	_, c := f.ctxLen[f.arch]
	_, e := f.embLen[f.arch]
	return c && e
}

// info — итог: значения с префиксом архитектуры и вид файла.
func (f *found) info(base string) Info {
	in := Info{Kind: kindOf(f.gtype, f.arch, base), Architecture: f.arch, Name: f.name, SplitCount: f.split}
	if f.arch != "" {
		in.ContextTrain, in.EmbeddingLen = f.ctxLen[f.arch], f.embLen[f.arch]
	}
	return in
}

// kindOf — правило вида §8.1.
func kindOf(gtype, arch, base string) string {
	switch gtype {
	case "model", "mmproj", "adapter":
		return gtype
	}
	switch {
	case arch == "clip":
		return "mmproj"
	case strings.Contains(strings.ToLower(base), "mmproj"):
		return "mmproj"
	case arch != "":
		return "model"
	}
	return "unknown"
}

// clip — верный UTF-8 без управляющих символов, не длиннее maxField байт: неверные байты и управляющие
// символы заменяются на U+FFFD до обрезки по границе символа (замены могут удлинить строку).
func clip(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return utf8.RuneError
		}
		return r
	}, s)
	if len(s) <= maxField {
		return s
	}
	i := maxField
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return strings.Clone(s[:i]) // копия: срез держал бы в кэше всю строку
}

// minSize — минимальный размер значения типа t (у строки и массива — их заголовки); 0 — неизвестный тип.
func minSize(t uint32) int64 {
	switch t {
	case tUint8, tInt8, tBool:
		return 1
	case tUint16, tInt16:
		return 2
	case tUint32, tInt32, tFloat32:
		return 4
	case tUint64, tInt64, tFloat64, tString:
		return 8
	case tArray:
		return 12
	}
	return 0
}

// value читает значение типа t на глубине depth; целое возвращается с isInt == true, строки пропускаются.
func (d *decoder) value(t uint32, depth int) (int64, bool, error) {
	switch t {
	case tString:
		_, err := d.str(false)
		return 0, false, err
	case tArray:
		return 0, false, d.array(depth)
	}
	size := minSize(t)
	if size == 0 {
		return 0, false, fmt.Errorf("%w %d", ErrBadType, t)
	}
	b := d.buf[:size]
	if err := d.read(b); err != nil {
		return 0, false, err
	}
	le := binary.LittleEndian
	switch t {
	case tUint8:
		return int64(b[0]), true, nil
	case tInt8:
		return int64(int8(b[0])), true, nil
	case tUint16:
		return int64(le.Uint16(b)), true, nil
	case tInt16:
		return int64(int16(le.Uint16(b))), true, nil
	case tUint32:
		return int64(le.Uint32(b)), true, nil
	case tInt32:
		return int64(int32(le.Uint32(b))), true, nil
	case tUint64:
		if u := le.Uint64(b); u <= math.MaxInt64 {
			return int64(u), true, nil
		}
	case tInt64:
		return int64(le.Uint64(b)), true, nil
	}
	return 0, false, nil // float32, float64, bool, uint64 больше MaxInt64
}

// array — тип элемента, число, элементы. Вложенность больше maxDepth и число элементов, не помещающееся в
// остаток бюджета, — лимит.
func (d *decoder) array(depth int) error {
	if depth >= maxDepth {
		return errLimit
	}
	et, err := d.u32()
	if err != nil {
		return err
	}
	cnt, err := d.u64()
	if err != nil {
		return err
	}
	size := minSize(et)
	if size == 0 {
		return fmt.Errorf("%w %d", ErrBadType, et)
	}
	if cnt > uint64(maxMeta-d.n)/uint64(size) {
		return errLimit
	}
	if et != tString && et != tArray {
		return d.skip(int64(cnt) * size)
	}
	for i := uint64(0); i < cnt; i++ {
		if i%1024 == 0 {
			if err := d.tick(); err != nil {
				return err
			}
		}
		if _, _, err := d.value(et, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// str — строка (длина uint64 и байты); keep == false — пропуск без сохранения.
func (d *decoder) str(keep bool) (string, error) {
	n, err := d.u64()
	if err != nil {
		return "", err
	}
	if n > maxString {
		return "", errLimit
	}
	if !keep {
		return "", d.skip(int64(n))
	}
	b := make([]byte, n)
	if err := d.read(b); err != nil {
		return "", err
	}
	return string(b), nil
}

// take учитывает n байт в бюджете maxMeta; не помещается — лимит.
func (d *decoder) take(n int64) error {
	if n < 0 || n > maxMeta-d.n {
		return errLimit
	}
	d.n += n
	return nil
}

func (d *decoder) read(p []byte) error {
	if err := d.take(int64(len(p))); err != nil {
		return err
	}
	_, err := io.ReadFull(d.r, p)
	return ioErr(err)
}

func (d *decoder) skip(n int64) error {
	if err := d.take(n); err != nil {
		return err
	}
	_, err := d.r.Discard(int(n))
	return ioErr(err)
}

func (d *decoder) u32() (uint32, error) {
	if err := d.read(d.buf[:4]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d.buf[:4]), nil
}

func (d *decoder) u64() (uint64, error) {
	if err := d.read(d.buf[:8]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(d.buf[:8]), nil
}

// tick — лимит времени (§8.1) и отмена ctx вызывающего.
func (d *decoder) tick() error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(d.deadline) {
		return errLimit
	}
	return nil
}

// ioErr — конец файла посреди метаданных — ErrTruncated; прочие ошибки чтения — без пути.
func ioErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ErrTruncated
	}
	return fmt.Errorf("read: %w", unwrapPath(err))
}
