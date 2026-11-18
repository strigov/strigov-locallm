package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tiny — минимальный GGUF v3 с general.architecture и (если не "") general.type.
func tiny(arch, gtype string) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	str := func(s string) { _ = binary.Write(&b, le, uint64(len(s))); b.WriteString(s) }
	kvs := [][2]string{{"general.architecture", arch}}
	if gtype != "" {
		kvs = append(kvs, [2]string{"general.type", gtype})
	}
	b.WriteString("GGUF")
	_ = binary.Write(&b, le, uint32(3))
	_ = binary.Write(&b, le, uint64(0))
	_ = binary.Write(&b, le, uint64(len(kvs)))
	for _, kv := range kvs {
		str(kv[0])
		_ = binary.Write(&b, le, uint32(8))
		str(kv[1])
	}
	return b.Bytes()
}

func put(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "sub", "m.gguf"), tiny("llama", "model"))
	put(t, filepath.Join(dir, "mmproj-x.GGUF"), tiny("clip", ""))
	put(t, filepath.Join(dir, "bad.gguf"), []byte("nope"))
	put(t, filepath.Join(dir, "notes.txt"), []byte("x"))
	var out, errOut bytes.Buffer
	if code := run([]string{dir}, &out, &errOut); code != 1 {
		t.Fatalf("code %d, output:\n%s", code, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], filepath.Join(dir, "bad.gguf")+"\terror: not a GGUF file") ||
		!strings.HasPrefix(lines[1], filepath.Join(dir, "mmproj-x.GGUF")+"\tkind=mmproj\tarch=clip\t") ||
		!strings.HasPrefix(lines[2], filepath.Join(dir, "sub", "m.gguf")+"\tkind=model\tarch=llama\tname=\"\"\tctx_train=0\tn_embd=0\tsplit=0\tsize=") ||
		!strings.HasSuffix(lines[2], "\tpartial=false") {
		t.Fatalf("output:\n%s", out.String())
	}
	out.Reset()
	if code := run([]string{filepath.Join(dir, "sub", "m.gguf")}, &out, &errOut); code != 0 || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("single file: code %d\n%s", code, out.String())
	}
	if code := run(nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: ggufinfo") {
		t.Fatalf("no args: code %d, stderr %q", code, errOut.String())
	}
}

func TestRunControlArchitectureIsOneLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "controls.gguf")
	put(t, p, tiny("llama\t\n\x1b]0;x\x07", "model"))
	var out, errOut bytes.Buffer
	code := run([]string{p}, &out, &errOut)
	if code != 0 || errOut.Len() != 0 || strings.Count(out.String(), "\n") != 1 ||
		!strings.Contains(out.String(), "\tarch=llama\uFFFD\uFFFD\uFFFD]0;x\uFFFD\tname=") ||
		strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

// Путь печатается только в первом столбце; текст ошибки его не повторяет.
func TestRunWalkErrorHasNoPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret-missing.gguf")
	var out, errOut bytes.Buffer
	if code := run([]string{p}, &out, &errOut); code != 1 {
		t.Fatalf("code %d", code)
	}
	_, msg, ok := strings.Cut(out.String(), "\terror: ")
	if !ok || strings.Contains(msg, p) || strings.Contains(msg, "secret-missing") {
		t.Fatalf("error contains path: %q", msg)
	}
}

// Junction с расширением .gguf не открывается даже как отдельный аргумент.
func TestRunSkipsJunction(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	put(t, filepath.Join(target, "m.gguf"), tiny("llama", "model"))
	link := filepath.Join(root, "link.gguf")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v\n%s", err, out)
	}
	for _, p := range []string{root, link} {
		var out, errOut bytes.Buffer
		if code := run([]string{p}, &out, &errOut); code != 0 || out.Len() != 0 {
			t.Fatalf("junction: code %d, output %q", code, out.String())
		}
	}
}
