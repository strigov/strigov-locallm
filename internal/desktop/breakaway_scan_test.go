package desktop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// §7.1 v2: CREATE_BREAKAWAY_FROM_JOB ставит только internal/desktop/launch_windows.go — у супервизора (llama-server)
// и монитора VRAM (nvidia-smi) его нет; JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK нет нигде. Проверяются идентификаторы
// в коде (не комментарии) всех файлов internal/ и cmd/, кроме тестов.
func TestBreakawayOnlyInDesktop(t *testing.T) {
	found := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join("..", "..", top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if s, ok := n.(*ast.SelectorExpr); ok &&
					(s.Sel.Name == "CREATE_BREAKAWAY_FROM_JOB" || s.Sel.Name == "JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK") {
					found[filepath.ToSlash(p)+": "+s.Sel.Name] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got := slices.Sorted(maps.Keys(found))
	want := []string{"../../internal/desktop/launch_windows.go: CREATE_BREAKAWAY_FROM_JOB"}
	if !slices.Equal(got, want) {
		t.Fatalf("breakaway identifiers:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
