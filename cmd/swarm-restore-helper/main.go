// The restore helper is an internal Bubblewrap entrypoint, not a user CLI.
package main

import (
	"github.com/supernovae/swarmos/internal/sandbox"
	"os"
)

func main() {
	if sandbox.RunRestoreHelper(os.Args[1:]) != nil {
		os.Exit(1)
	}
}
