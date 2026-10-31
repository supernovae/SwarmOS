package coordination

import (
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

// InventoryPreviewStore is optional. A missing capability never permits an
// inventory caller to fall back to the larger or creating Store.Load path.
type InventoryPreviewStore interface {
	LoadInventoryPreview(context.Context, int, int64, func(int64) error) ([]byte, error)
}

// LoadInventoryPreview reads atomically replaced local authority without locks.
// It reserves raw input and parser space before reading; callers still validate
// the projected state identity and treat failures as unknown retention.
func (s *FileStore) LoadInventoryPreview(ctx context.Context, n int, maxBytes int64, reserve func(int64) error) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n < 1 || maxBytes < 1 || maxBytes > 1<<20 || reserve == nil {
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
	if err = reserve(8192 + 128*before.Size()); err != nil {
		return nil, err
	}
	// The file descriptor is pinned. Read at most the observed size plus one,
	// rejecting replacement/growth instead of allocating beyond the reservation.
	data := make([]byte, before.Size()+1)
	used := 0
	for used < len(data) {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		n, e := f.Read(data[used:])
		used += n
		if e != nil {
			if e != io.EOF {
				return nil, e
			}
			break
		}
	}
	if int64(used) != before.Size() {
		return nil, fmt.Errorf("inventory state changed")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("inventory state changed")
	}
	return data[:used], nil
}

// LoadInventoryObject reads a coordinator-pinned ledger object already present
// locally. It never fetches or resolves a candidate-supplied symbolic ref.
func (s *GitStore) LoadInventoryObject(ctx context.Context, sha string, reserve func(int64) error) ([]byte, error) {
	if !inventorySHA(sha) || reserve == nil {
		return nil, fmt.Errorf("invalid inventory object identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := reserve(65536); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := gitcredential.ObservationCommand(child, s.root, "show", sha+":state.json")
	if err != nil {
		return nil, err
	}
	out := &inventoryOutput{remaining: 128 << 10, reserve: reserve, cancel: cancel}
	cmd.Stdout = out
	// A shared pipe serializes shared-budget admissions from both streams.
	// Diagnostics mixed with state fail subsequent JSON validation.
	cmd.Stderr = out
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("pinned inventory object unavailable: %w", err)
	}
	return out.data, nil
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

type inventoryOutput struct {
	data      []byte
	remaining int
	reserve   func(int64) error
	cancel    context.CancelFunc
}

func (w *inventoryOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.cancel()
		return 0, fmt.Errorf("inventory Git output limit")
	}
	if err := w.reserve(128 * int64(len(p))); err != nil {
		w.cancel()
		return 0, err
	}
	w.remaining -= len(p)
	w.data = append(w.data, p...)
	return len(p), nil
}

// ReadInventorySnapshot pins one complete set of local state files. Atomic
// replacement changes the directory identity or timestamps; such a traversal
// is rejected rather than combining independently timed state reads.
func (s *FileStore) ReadInventorySnapshot(ctx context.Context, limit int, reserve func(int64) error) (map[int][]byte, error) {
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
		data, e := s.LoadInventoryPreview(ctx, n, 128<<10, reserve)
		if e != nil {
			return nil, e
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
	return out, nil
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
func (s *GitStore) ReadInventorySnapshot(ctx context.Context, apply bool, limit int, reserve func(int64) error) (map[int][]byte, error) {
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
		data, e := s.LoadInventoryObject(ctx, sha, reserve)
		if e != nil {
			return nil, e
		}
		out[n] = data
	}
	return out, nil
}
