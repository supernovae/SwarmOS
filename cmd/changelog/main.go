// Command changelog folds release fragments from the current directory.
package main

import (
	"fmt"
	"github.com/supernovae/swarmos/internal/changelog"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: changelog VERSION")
		os.Exit(1)
	}
	if err := changelog.Fold(".", os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
