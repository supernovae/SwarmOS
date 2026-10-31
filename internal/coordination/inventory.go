package coordination

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

const InventoryInputLimit int64 = 64 << 20

// InventoryDecoder consumes a pinned stream synchronously.
type InventoryDecoder func(context.Context, io.Reader, func(int64) error) ([]byte, error)

// InventoryPreviewStore is optional. A missing capability never permits an
// inventory caller to fall back to the larger or creating Store.Load path.
type InventoryPreviewStore interface {
	LoadInventoryPreview(context.Context, int, int64, func(int64) error, ...InventoryDecoder) ([]byte, error)
}

// LoadInventoryPreview reads atomically replaced local authority without locks.
// The decoder reserves bounded workspace and projections while the descriptor
// remains pinned. Failures never permit fallback to an ordinary state read.
func (s *FileStore) LoadInventoryPreview(ctx context.Context, n int, maxBytes int64, reserve func(int64) error, decoders ...InventoryDecoder) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 1 || maxBytes < 1 || maxBytes > InventoryInputLimit || reserve == nil {
		return nil, fmt.Errorf("invalid inventory preview budget")
	}
	path, err := filepath.Abs(s.repoRoot)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := safefs.Open(volume)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(volume, filepath.Join(path, ".swarm", "coordination", fmt.Sprintf("issue-%d.json", n)))
	if err != nil {
		return nil, err
	}
	f, err := root.OpenRegular(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() < 0 || before.Size() > maxBytes {
		return nil, fmt.Errorf("inventory state exceeds limit")
	}
	data, err := decodeInventory(ctx, f, maxBytes, reserve, decoders)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, statErr := root.Lstat(rel)
	if statErr != nil {
		return nil, statErr
	}
	if !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("inventory state changed")
	}
	return data, nil
}

// LoadInventoryObject reads a coordinator-pinned ledger object already present
// locally. It never fetches or resolves a candidate-supplied symbolic ref.
func (s *GitStore) LoadInventoryObject(ctx context.Context, sha string, reserve func(int64) error, decoders ...InventoryDecoder) ([]byte, error) {
	if !inventorySHA(sha) || reserve == nil {
		return nil, fmt.Errorf("invalid inventory object identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reserve(8192); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := gitcredential.ObservationCommand(child, s.root, "show", sha+":state.json")
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	diagnostics := &inventoryDiagnostic{}
	cmd.Stderr = diagnostics
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := decodeInventory(ctx, stdout, InventoryInputLimit, reserve, decoders)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if readErr != nil {
		if diagnostics.String() != "" {
			return nil, fmt.Errorf("pinned inventory object %s: %w: %s", sha, readErr, diagnostics.String())
		}
		return nil, fmt.Errorf("pinned inventory object %s: %w", sha, readErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("pinned inventory object %s unavailable (%s): %w", sha, diagnostics.String(), inventoryWaitError{waitErr})
	}
	return data, nil
}

func decodeInventory(ctx context.Context, r io.Reader, max int64, reserve func(int64) error, decoders []InventoryDecoder) ([]byte, error) {
	if len(decoders) != 1 {
		return nil, fmt.Errorf("inventory projection decoder required")
	}
	return decoders[0](ctx, &inventoryReader{ctx: ctx, r: r, remaining: max}, reserve)
}

type inventoryReader struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
}

func (r *inventoryReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.r.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("inventory input exceeds %d byte limit", InventoryInputLimit)
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

type inventoryDiagnostic struct{ data bytes.Buffer }

func (w *inventoryDiagnostic) Len() int { return w.data.Len() }
func (w *inventoryDiagnostic) String() string {
	return strings.Join(strings.Fields(w.data.String()), " ")
}

func (w *inventoryDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > 4096-w.Len() {
		p = p[:4096-w.Len()]
	}
	_, _ = w.data.Write(p)
	return n, nil
}

func inventorySHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

// ReadInventorySnapshot pins one complete set of local state files. Atomic
// replacement changes the directory identity or timestamps; such a traversal
// is rejected rather than combining independently timed state reads.
func (s *FileStore) ReadInventorySnapshot(ctx context.Context, limit int, reserve func(int64) error, decoders ...InventoryDecoder) (map[int][]byte, error) {
	base, err := safefs.Open(s.repoRoot)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	dir, err := base.Sub(filepath.Join(".swarm", "coordination"), false, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.OpenDirectory(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	out := map[int][]byte{}
	var failures []error
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, e := f.ReadDir(1)
		if len(entries) == 0 {
			if e != nil && !errors.Is(e, io.EOF) {
				return nil, e
			}
			break
		}
		count++
		if count > limit {
			return nil, fmt.Errorf("inventory entry limit")
		}
		name := entries[0].Name()
		if !strings.HasPrefix(name, "issue-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, e := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "issue-"), ".json"))
		if e != nil || n < 1 || name != fmt.Sprintf("issue-%d.json", n) {
			return nil, fmt.Errorf("invalid state name")
		}
		data, e := s.LoadInventoryPreview(ctx, n, InventoryInputLimit, reserve, decoders...)
		if e != nil {
			failures = append(failures, fmt.Errorf("issue %d: %w", n, e))
			out[n] = nil
			continue
		}
		out[n] = data
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := base.Sub(filepath.Join(".swarm", "coordination"), false, 0)
	if err != nil {
		return nil, err
	}
	defer current.Close()
	info, err := current.Lstat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, info) || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("inventory directory changed")
	}
	return out, errors.Join(failures...)
}

// FetchInventoryObjects refreshes only explicitly pinned state objects. It
// writes no state refs, FETCH_HEAD, checkpoint or ledger authority.
func (s *GitStore) FetchInventoryObjects(ctx context.Context, ids []string) error {
	if len(ids) > 10000 {
		return fmt.Errorf("inventory object limit")
	}
	for _, sha := range ids {
		if !inventorySHA(sha) {
			return fmt.Errorf("invalid inventory object")
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := gitcredential.NetworkLimited(ctx, s.root, true, 128<<10, append([]string{"fetch", "--no-tags", "--no-write-fetch-head", s.Remote()}, ids...)...)
	return err
}

// ReadInventorySnapshot obtains the complete branch-ledger ref set in one Git
// advertisement. Apply refreshes objects before pinning the final set. Hidden
// ledger coverage is deliberately rejected until explicitly supported here.
func (s *GitStore) ReadInventorySnapshot(ctx context.Context, apply bool, limit int, reserve func(int64) error, decoders ...InventoryDecoder) (map[int][]byte, error) {
	if !apply {
		return nil, fmt.Errorf("preview requires externally pinned bounded ref evidence")
	}
	list := func() (map[int]string, error) {
		if err := reserve(256 << 10); err != nil {
			return nil, err
		}
		rawBytes, err := gitcredential.NetworkLimited(ctx, s.root, false, 128<<10, "ls-remote", "--refs", s.Remote(), "refs/heads/swarm-state/*", "refs/swarm/state/*")
		raw := string(rawBytes)
		if err != nil {
			return nil, err
		}
		if len(raw) > 1<<20 {
			return nil, fmt.Errorf("inventory ref limit")
		}
		out := map[int]string{}
		for _, line := range strings.Split(raw, "\n") {
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 2 || !inventorySHA(fields[0]) || !strings.HasPrefix(fields[1], "refs/heads/swarm-state/issue-") {
				return nil, fmt.Errorf("unsupported state snapshot")
			}
			n, e := strconv.Atoi(strings.TrimPrefix(fields[1], "refs/heads/swarm-state/issue-"))
			if e != nil || n < 1 || fields[1] != fmt.Sprintf("refs/heads/swarm-state/issue-%d", n) || out[n] != "" {
				return nil, fmt.Errorf("ambiguous state snapshot")
			}
			if len(out) >= limit {
				return nil, fmt.Errorf("inventory ref limit")
			}
			if err := reserve(1024); err != nil {
				return nil, err
			}
			out[n] = fields[0]
		}
		return out, nil
	}
	refs, err := list()
	if err != nil {
		return nil, err
	}
	if apply {
		ids := make([]string, 0, len(refs))
		for _, sha := range refs {
			ids = append(ids, sha)
		}
		if err = reserve(256 << 10); err != nil {
			return nil, err
		}
		if err = s.FetchInventoryObjects(ctx, ids); err != nil {
			return nil, err
		}
		fresh, e := list()
		if e != nil {
			return nil, e
		}
		if len(fresh) != len(refs) {
			return nil, fmt.Errorf("state snapshot changed during refresh")
		}
		for n, sha := range fresh {
			if refs[n] != sha {
				return nil, fmt.Errorf("state snapshot changed during refresh")
			}
		}
		refs = fresh
	}
	out := map[int][]byte{}
	for n, sha := range refs {
		data, e := s.LoadInventoryObject(ctx, sha, reserve, decoders...)
		if e != nil {
			return nil, fmt.Errorf("issue %d: %w", n, e)
		}
		out[n] = data
	}
	return out, nil
}

// Unwrap preserves process error identity without exposing an opaque kill status.
type inventoryWaitError struct{ error }

func (e inventoryWaitError) Error() string {
	if strings.Contains(e.error.Error(), "signal: killed") {
		return "inventory process interrupted while reading"
	}
	return e.error.Error()
}
func (e inventoryWaitError) Unwrap() error { return e.error }
