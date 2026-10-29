package changelog

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// Check parses every unreleased fragment without writing and reports each
// malformed one. A missing or empty fragment directory passes.
func Check(root string) error {
	r, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer r.Close()
	dir, err := r.Sub("changes/unreleased", false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	// List the pinned directory so a swapped path cannot redirect the check.
	listing, err := dir.OpenDirectory(".")
	if err != nil {
		return err
	}
	entries, err := listing.ReadDir(-1)
	listing.Close()
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var problems []error
	for _, name := range names {
		data, err := dir.ReadFile(name, 4<<20)
		if err == nil {
			_, err = fragment(string(data))
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("fragment %s: %w", name, err))
		}
	}
	return errors.Join(problems...)
}
