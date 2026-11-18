// cmd/fakeupstream/main.go — фейковый llama-server (§14.3).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/strigov/strigov-locallm/internal/fakeupstream"
)

func main() {
	c, err := fakeupstream.ParseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		os.Exit(2)
	}
	if err := fakeupstream.Run(context.Background(), c, os.Stdout, os.Stderr, os.Exit); err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(1)
	}
}
