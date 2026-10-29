// Command changelog folds release fragments from the current directory, or
// checks them without writing when run with --check.
package main

import (
	"fmt"
	"github.com/supernovae/swarmos/internal/changelog"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: changelog VERSION | changelog --check")
		os.Exit(1)
	}
	if os.Args[1] == "--check" {
		if err := changelog.Check("."); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := changelog.Fold(".", os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
