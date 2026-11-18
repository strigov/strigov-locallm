// Command ggufinfo печатает метаданные GGUF (§8.1 спецификации v2) каждого файла .gguf в указанных каталогах
// (с подкаталогами) и файлах — для проверки QV4 на файлах владельца. Файлы только читаются.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/strigov/strigov-locallm/internal/gguf"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run — строка на файл: путь и поля Info через табуляцию либо путь и ошибка. Код 2 — нет аргументов, 1 — была
// ошибка, иначе 0. Ссылки и junction не обходятся (filepath.WalkDir).
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: ggufinfo <folder or file.gguf>...")
		return 2
	}
	code := 0
	for _, root := range args {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				var pe *os.PathError
				if errors.As(err, &pe) {
					err = pe.Err
				}
				fmt.Fprintf(out, "%s\terror: %v\n", p, err)
				code = 1
				return nil
			}
			if d.IsDir() || d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || !strings.EqualFold(filepath.Ext(p), ".gguf") {
				return nil
			}
			in, err := gguf.ReadInfo(context.Background(), p)
			if err != nil {
				fmt.Fprintf(out, "%s\terror: %v\n", p, err)
				code = 1
				return nil
			}
			fmt.Fprintf(out, "%s\tkind=%s\tarch=%s\tname=%q\tctx_train=%d\tn_embd=%d\tsplit=%d\tsize=%d\tpartial=%t\n",
				p, in.Kind, in.Architecture, in.Name, in.ContextTrain, in.EmbeddingLen, in.SplitCount, in.Size, in.Partial)
			return nil
		})
	}
	return code
}
