//go:build windows

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// junction — точка соединения link → target во временном каталоге теста (mklink /J; прав администратора не нужно).
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v\n%s", link, target, err, out)
	}
}

// PV1 (§5.6 спецификации v2): окончательный путь — без \\?\, в регистре диска, после junction и "..".
func TestCanonical(t *testing.T) {
	dir := t.TempDir()
	root, err := Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(root, `\\?\`) || !filepath.IsAbs(root) {
		t.Fatalf("root %q must be a plain absolute path", root)
	}
	file := filepath.Join(dir, "Sub", "Model.GGUF")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(dir, "link"), filepath.Join(dir, "Sub"))
	want := root + `\Sub\Model.GGUF`
	for _, p := range []string{
		file,
		strings.ToLower(file),
		filepath.ToSlash(file),
		dir + `\Sub\.\x\..\Model.GGUF`,
		filepath.Join(dir, "link", "model.gguf"),
	} {
		if got, err := Canonical(p); err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", p, got, err, want)
		}
	}
	if got, err := Canonical(filepath.Join(dir, "sub")); err != nil || got != root+`\Sub` {
		t.Errorf("directory: %q, %v", got, err)
	}
}

// Путь может прийти из значения аргумента: в тексте ошибки его нет (§5.5 п. 14).
func TestCanonicalErrorHasNoPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "SENTINEL-missing-Qx1", "model.gguf")
	if _, err := Canonical(p); err == nil || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("err=%v", err)
	}
	if _, err := Canonical("C:\\a\x00b"); err == nil || strings.Contains(err.Error(), `C:\a`) {
		t.Fatalf("NUL: err=%v", err)
	}
}

func TestStripFinalPathPrefix(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\models\x.gguf`:     `C:\models\x.gguf`,
		`\\?\UNC\srv\share\x.gguf`: `\\srv\share\x.gguf`,
		`C:\models\x.gguf`:         `C:\models\x.gguf`,
	} {
		if got := stripFinalPathPrefix(in); got != want {
			t.Errorf("stripFinalPathPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
