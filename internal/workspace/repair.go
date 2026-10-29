package workspace

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// RepairPaths fixes the repair scope from the committed PR diff. Disabling
// rename detection includes both endpoints without parsing quoted filenames.
func RepairPaths(ctx context.Context, root, base string) ([]string, error) {
	if err := prCommit(ctx, root, base); err != nil {
		return nil, err
	}
	out, err := gitOutputContext(ctx, root, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base, "HEAD", "--")
	if err != nil {
		return nil, err
	}
	return nulPaths(out), nil
}

func TrackedRepairFiles(ctx context.Context, root string) ([]string, error) {
	out, err := gitOutputContext(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	return nulPaths(out), nil
}
func nulPaths(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// ExactRepairFiles validates explicit selections against both index and safe root.
func ExactRepairFiles(ctx context.Context, root string, files []string) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > 256 {
		return nil, fmt.Errorf("too many repair selections")
	}
	out, err := gitOutputContext(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, entry := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if ok && len(fields) == 3 && fields[2] == "0" && (fields[0] == "100644" || fields[0] == "100755") {
			tracked[name] = true
		}
	}
	safe, err := safefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer safe.Close()
	files = append([]string(nil), files...)
	for _, p := range files {
		if p == "" || len(p) > 4096 || path.Clean(p) != p || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\\:*?[]\x00") || !tracked[p] {
			return nil, fmt.Errorf("invalid exact repair file %q", p)
		}
		info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("repair file must be regular: %q", p)
		}
		if _, e = safe.ReadFile(filepath.FromSlash(p), 32<<20); e != nil {
			return nil, e
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}
