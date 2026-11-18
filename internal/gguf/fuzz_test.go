package gguf

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// countingReader считает байты, отданные декодеру.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// FuzzReadInfo — декодер никогда не паникует и не читает больше бюджета (плюс буфер bufio) (§8.1, §14.2).
func FuzzReadInfo(f *testing.F) {
	g := allTypes(newFile(3)).str("general.architecture", "llama").u32kv("llama.context_length", 4096)
	f.Add(g.bytes())
	f.Add(newFile(2).str("general.type", "mmproj").bytes())
	f.Add([]byte("GGUF"))
	f.Add([]byte{})
	deep := newFile(3)
	deep.kv("deep", tArray, func() {
		for range 5 {
			deep.u32(tArray)
			deep.u64(1)
		}
	})
	f.Add(deep.bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		cr := &countingReader{r: bytes.NewReader(data)}
		in, err := decode(context.Background(), cr, "fuzz.gguf", time.Now().Add(readTimeout))
		if cr.n > maxMeta+bufSize {
			t.Fatalf("read %d bytes", cr.n)
		}
		for _, s := range []string{in.Architecture, in.Name} {
			if len(s) > maxField || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
				t.Fatalf("field not clipped: %d bytes, valid %t, controls %t", len(s), utf8.ValidString(s), strings.ContainsFunc(s, unicode.IsControl))
			}
		}
		if err != nil && in != (Info{}) {
			t.Fatalf("error %v with info %+v", err, in)
		}
	})
}
