package ui

import (
	"io/fs"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const maxTotal = 100 << 10 // §10.1: всего не больше 100 КиБ

// forbidden — запретные выражения §10.1 и адреса со схемой (без CDN и внешних ресурсов): ищутся во всех файлах,
// включая комментарии.
var forbidden = []*regexp.Regexp{
	regexp.MustCompile(`innerHTML`),
	regexp.MustCompile(`outerHTML`),
	regexp.MustCompile(`insertAdjacentHTML`),
	regexp.MustCompile(`document\.write`),
	regexp.MustCompile(`\beval\s*\(`),
	regexp.MustCompile(`\bnew\s+Function\b`),
	regexp.MustCompile("\\bset(Timeout|Interval)\\s*\\(\\s*['\"`]"),
	regexp.MustCompile(`(?i)javascript:`),
	regexp.MustCompile(`https?://`),
}

// readAll — все файлы Files() по имени; подкаталог — ошибка теста.
func readAll(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." {
				t.Errorf("subdirectory %s: static must be flat", p)
			}
			return nil
		}
		data, err := fs.ReadFile(Files(), p)
		if err != nil {
			return err
		}
		files[p] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Каталог плоский; только .html, .js и .css; шесть файлов панели на месте.
func TestFilesLayout(t *testing.T) {
	files := readAll(t)
	for name := range files {
		switch path.Ext(name) {
		case ".html", ".js", ".css":
		default:
			t.Errorf("%s: only .html, .js and .css are allowed", name)
		}
	}
	for _, name := range []string{"index.html", "style.css", "app.js", "api.js", "logic.js", "poller.js"} {
		if _, ok := files[name]; !ok {
			t.Errorf("%s is missing", name)
		}
	}
}

func TestTotalSize(t *testing.T) {
	total := 0
	for _, text := range readAll(t) {
		total += len(text)
	}
	if total > maxTotal {
		t.Fatalf("panel files: %d bytes, limit %d", total, maxTotal)
	}
	t.Logf("panel files: %d bytes", total)
}

func TestNoForbiddenWords(t *testing.T) {
	for name, text := range readAll(t) {
		for _, re := range forbidden {
			if m := re.FindString(text); m != "" {
				t.Errorf("%s: forbidden %q", name, m)
			}
		}
	}
}

// index.html: ровно один <script> — модуль app.js, стили — файлом; ни <style>, ни атрибутов on…= и style= (CSP §9.1).
func TestIndexHasNoInlineCode(t *testing.T) {
	html := readAll(t)["index.html"]
	if n := len(regexp.MustCompile(`(?i)<script\b`).FindAllString(html, -1)); n != 1 ||
		!strings.Contains(html, `<script type="module" src="app.js"></script>`) {
		t.Fatalf("index.html: %d <script> tags; want exactly <script type=\"module\" src=\"app.js\"></script>", n)
	}
	if !strings.Contains(html, `<link rel="stylesheet" href="style.css">`) {
		t.Fatal("index.html: no style.css link")
	}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?i)<style`), regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		regexp.MustCompile(`(?i)\sstyle\s*=`)} {
		if m := re.FindString(html); m != "" {
			t.Errorf("index.html: inline code %q", m)
		}
	}
}

// Модули импортируют только ./<имя>.js этого же каталога; без import 'x' и import().
func TestImportsAreLocal(t *testing.T) {
	fromRe := regexp.MustCompile(`\bfrom\s*['"]([^'"]*)['"]`)
	bareRe := regexp.MustCompile(`\bimport\s*['"]`)
	dynRe := regexp.MustCompile(`\bimport\s*\(`)
	localRe := regexp.MustCompile(`^\./[a-z]+\.js$`)
	files := readAll(t)
	imports := 0
	for name, text := range files {
		if path.Ext(name) != ".js" {
			continue
		}
		if bareRe.MatchString(text) || dynRe.MatchString(text) {
			t.Errorf("%s: side-effect or dynamic import", name)
		}
		for _, m := range fromRe.FindAllStringSubmatch(text, -1) {
			imports++
			if !localRe.MatchString(m[1]) {
				t.Errorf("%s: import from %q", name, m[1])
			} else if _, ok := files[strings.TrimPrefix(m[1], "./")]; !ok {
				t.Errorf("%s: %q is not in static", name, m[1])
			}
		}
	}
	if imports == 0 {
		t.Fatal("no imports found: app.js must import api.js, logic.js and poller.js")
	}
}

// TestJSLogic — тесты node --test панели (Ф2). Нет node в PATH или нет jstest/*.test.mjs — пропуск.
func TestJSLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not in PATH")
	}
	files, err := filepath.Glob(filepath.Join("jstest", "*.test.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no jstest/*.test.mjs")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
	t.Logf("node --test:\n%s", out)
}
