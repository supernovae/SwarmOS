package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

const timeoutPatchBytes = 4 << 20

type timeoutFile struct {
	data       []byte
	executable bool
	omitted    bool
}

func (f timeoutFile) gitMode() string {
	if f.executable {
		return "100755"
	}
	return "100644"
}

// timeoutFiles uses pinned directory handles and never opens special files.
func timeoutFiles(ctx context.Context, dir string) (map[string]timeoutFile, error) {
	root, err := safefs.Open(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files := map[string]timeoutFile{}
	entries := 0
	var total, retained int64
	var walk func(string) error
	walk = func(dir string) error {
		in, err := root.OpenDirectory(dir)
		if err != nil {
			return err
		}
		defer in.Close()
		for {
			children, err := in.ReadDir(128)
			if err != nil && err != io.EOF {
				return err
			}
			for _, entry := range children {
				if err := ctx.Err(); err != nil {
					return err
				}
				name := filepath.Join(dir, entry.Name())
				if name == ".git" || name == filepath.Join(".swarm", "active-output") || (entry.IsDir() && IsEphemeralBuildDir(entry.Name())) {
					continue
				}
				if _, err := workerPath(filepath.ToSlash(name)); err != nil {
					return err
				}
				entries++
				if entries > workerMaxEntries {
					return fmt.Errorf("timeout snapshot entry limit exceeded")
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.IsDir() {
					if err := walk(name); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("timeout snapshot rejects non-regular file %q", name)
				}
				total += info.Size()
				if total > workerMaxBytes {
					return fmt.Errorf("timeout snapshot exceeds aggregate limit")
				}
				// Oversized entries are recorded as binary omissions without reading them.
				if info.Size() > timeoutPatchBytes || retained+info.Size() > 32<<20 {
					files[name] = timeoutFile{omitted: true}
					continue
				}
				data, err := root.ReadFile(name, timeoutPatchBytes)
				if err != nil {
					return err
				}
				retained += int64(len(data))
				files[name] = timeoutFile{data: data, executable: info.Mode().Perm()&0111 != 0}
			}
			if err == io.EOF {
				return nil
			}
		}
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	return files, nil
}

func patchLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.SplitAfter(string(data), "\n")
}
func nonemptyLines(data []byte) []string {
	lines := patchLines(data)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
func patchName(prefix, name string) string {
	value := prefix + filepath.ToSlash(name)
	if strings.ContainsAny(value, "\t\n\r\\\" ") {
		return strconv.Quote(value)
	}
	return value
}

// A single replacement hunk with common prefix/suffix removed is linear and
// bounded even for files with millions of repeated lines.
func textPatch(name string, before, after []byte, existed, exists bool) []byte {
	a, b := nonemptyLines(before), nonemptyLines(after)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	endA, endB := len(a), len(b)
	for endA > prefix && endB > prefix && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	if prefix == endA && prefix == endB && existed == exists {
		return nil
	}
	oldName, newName := patchName("a/", name), patchName("b/", name)
	var out bytes.Buffer
	fmt.Fprintf(&out, "diff --git %s %s\n", oldName, newName)
	if !existed {
		out.WriteString("new file mode 100644\n")
		oldName = "/dev/null"
	}
	if !exists {
		out.WriteString("deleted file mode 100644\n")
		newName = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	if len(a) == 0 && len(b) == 0 {
		return out.Bytes()
	}
	start := max(0, prefix-3)
	suffix := min(3, min(len(a)-endA, len(b)-endB))
	stopA, stopB := endA+suffix, endB+suffix
	startA, startB := start+1, start+1
	if stopA == start {
		startA = start
	}
	if stopB == start {
		startB = start
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", startA, stopA-start, startB, stopB-start)
	for _, part := range []struct {
		lines  []string
		marker byte
	}{
		{a[start:prefix], ' '}, {a[prefix:endA], '-'}, {b[prefix:endB], '+'}, {a[endA:stopA], ' '},
	} {
		for _, line := range part.lines {
			out.WriteByte(part.marker)
			out.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				out.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return out.Bytes()
}

func preserveTimeoutPatch(ctx context.Context, input, work, destination, role string) (string, error) {
	switch role {
	case "planner", "reviewer", "developer", "tester":
	default:
		return "", fmt.Errorf("invalid recovery role %q", role)
	}
	baseline, err := os.MkdirTemp(filepath.Dir(input), "timeout-baseline-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(baseline)
	if _, err := unpackWorker(ctx, input, baseline); err != nil {
		return "", err
	}
	before, err := timeoutFiles(ctx, baseline)
	if err != nil {
		return "", err
	}
	after, err := timeoutFiles(ctx, work)
	if err != nil {
		return "", err
	}
	patch, omitted, err := buildPatch(ctx, before, after)
	if err != nil {
		return "", err
	}
	root, err := safefs.Open(destination)
	if err != nil {
		return "", err
	}
	defer root.Close()
	rel := filepath.Join(".swarm", "active-output", role+"-timeout.patch")
	if err := root.MkdirAll(filepath.Dir(rel), 0700); err != nil {
		return "", err
	}
	if err := root.WriteFile(rel, patch, 0600); err != nil {
		return "", err
	}
	result := filepath.Join(destination, rel)
	if omitted > 0 {
		result += fmt.Sprintf(" (%d binary or oversized changes omitted)", omitted)
	}
	return result, nil
}

// buildPatch renders a bounded text patch from before to after. Binary,
// oversized, or over-budget changes are counted in omitted, not rendered.
func buildPatch(ctx context.Context, before, after map[string]timeoutFile) ([]byte, int, error) {
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var patch bytes.Buffer
	omitted := 0
	for _, name := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		a, existed := before[name]
		b, exists := after[name]
		if a.omitted || b.omitted || bytes.IndexByte(a.data, 0) >= 0 || bytes.IndexByte(b.data, 0) >= 0 {
			omitted++
			continue
		}
		if bytes.Equal(a.data, b.data) && a.executable == b.executable && existed == exists {
			continue
		}
		hunk := textPatch(name, a.data, b.data, existed, exists)
		if !existed {
			hunk = bytes.Replace(hunk, []byte("new file mode 100644"), []byte("new file mode "+b.gitMode()), 1)
		}
		if !exists {
			hunk = bytes.Replace(hunk, []byte("deleted file mode 100644"), []byte("deleted file mode "+a.gitMode()), 1)
		}
		if existed && exists && a.executable != b.executable {
			header := fmt.Sprintf("diff --git %s %s\n", patchName("a/", name), patchName("b/", name))
			mode := fmt.Sprintf("old mode %s\nnew mode %s\n", a.gitMode(), b.gitMode())
			if len(hunk) == 0 {
				hunk = []byte(header + mode)
			} else {
				hunk = bytes.Replace(hunk, []byte(header), []byte(header+mode), 1)
			}
		}
		if patch.Len()+len(hunk) > timeoutPatchBytes {
			omitted++
			continue
		}
		patch.Write(hunk)
	}
	return patch.Bytes(), omitted, nil
}
