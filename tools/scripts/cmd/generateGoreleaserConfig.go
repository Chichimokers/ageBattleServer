package main

import (
	"fmt"
	"os"

	"scripts/internal/goreleaser"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: cd tools/scripts && go run ./cmd/generateGoreleaserConfig.go GOROOT_LEGACY")
		os.Exit(2)
	}

	if err := goreleaser.Generate(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "generate GoReleaser config: %v\n", err)
		os.Exit(1)
	}
}
