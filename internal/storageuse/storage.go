// Package storageuse keeps coordinator-owned, durable evidence of storage use.
// A lost coordinator leaves pending records. Neither time nor process IDs can
// turn those records into permission to perform maintenance.
package storageuse

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
)

var ErrUnsafe = errors.New("storage coverage is incomplete or storage is in use")

// Resource identifies repository-lifetime storage independently of claims.
type Resource struct {
	Repository string
	Class      string
	Identity   string
	Location   string
}

type record struct {
	Version          int
	Resource         Resource
	CompleteCoverage bool
	Created          time.Time
	LastUse          time.Time
	Uses             map[string]bool // false means unresolved; completed records are retained
}

// Manager must remain in the coordinator; it is never part of worker options.
type Manager struct {
	root *safefs.Root
	path string
}

// Open opens private control storage outside every granted root. Paths are
// traversed through safefs, including existing parent components.
func Open(path string, forbidden ...string) (*Manager, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, errors.New("control root must be absolute and private")
	}
	path = filepath.Clean(path)
	for _, p := range forbidden {
		if p == "" {
			continue
		}
		if overlaps(path, p) {
			return nil, errors.New("storage control root overlaps a granted root")
		}
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	rel, err := filepath.Rel(volume, path)
	if err != nil {
		return nil, err
	}
	r, err := base.Sub(rel, true, 0700)
	if err != nil {
		return nil, err
	}
	f, err := r.OpenDirectory(".")
	if err != nil {
		r.Close()
		return nil, err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil || info.Mode().Perm()&0077 != 0 {
		r.Close()
		return nil, errors.New("storage control root is not private")
	}
	return &Manager{root: r, path: path}, nil
}

func overlaps(a, b string) bool {
	resolve := func(p string) string {
		p, _ = filepath.Abs(p)
		// Resolve the existing prefix even when the leaf has not been created.
		var tail []string
		for {
			if q, err := filepath.EvalSymlinks(p); err == nil {
				for i := len(tail) - 1; i >= 0; i-- {
					q = filepath.Join(q, tail[i])
				}
				return q
			}
			parent := filepath.Dir(p)
			if parent == p {
				return p
			}
			tail = append(tail, filepath.Base(p))
			p = parent
		}
	}
	a, b = resolve(a), resolve(b)
	inside := func(a, b string) bool {
		r, e := filepath.Rel(a, b)
		return e == nil && (r == "." || filepath.IsLocal(r))
	}
	return inside(a, b) || inside(b, a)
}

func (m *Manager) Close() error { return m.root.Close() }

func (m *Manager) key(r Resource) (string, error) {
	if r.Repository == "" || r.Class == "" || r.Identity == "" || !filepath.IsAbs(r.Location) || filepath.Clean(r.Location) != r.Location {
		return "", errors.New("incomplete storage identity")
	}
	if overlaps(m.path, r.Location) {
		return "", errors.New("storage resource overlaps control root")
	}
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func unlock(f *os.File) { _ = filelock.Unlock(f); _ = f.Close() }

func (m *Manager) lock(ctx context.Context, key string) (*os.File, bool, error) {
	old, err := m.root.OpenRegular(key + ".lock")
	fresh := errors.Is(err, os.ErrNotExist)
	if err == nil {
		old.Close()
	} else if !fresh {
		return nil, false, err
	}
	f, err := m.root.OpenLock(key + ".lock")
	if err != nil {
		return nil, false, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, false, err
		}
		err = filelock.TryLockExclusive(f)
		if err == nil {
			if err = ctx.Err(); err != nil {
				unlock(f)
				return nil, false, err
			}
			return f, fresh, nil
		}
		if !lockBusy(err) {
			f.Close()
			return nil, false, err
		}
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, false, ctx.Err()
		case <-t.C:
		}
	}
}

func (m *Manager) read(key string, r Resource) (*record, error) {
	b, err := m.root.ReadFile(key+".json", 16<<20)
	if err != nil {
		return nil, err
	}
	var v record
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, ErrUnsafe
	}
	if v.Version != 1 || v.Resource != r || v.Uses == nil || v.Created.IsZero() || v.LastUse.Before(v.Created) {
		return nil, ErrUnsafe
	}
	for token := range v.Uses {
		if len(token) != 64 {
			return nil, ErrUnsafe
		}
		if _, err := hex.DecodeString(token); err != nil {
			return nil, ErrUnsafe
		}
	}
	return &v, nil
}
func (m *Manager) write(key string, v *record) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.root.WriteFileDurable(key+".json", b, 0600)
}

// Use is an unexported token bound to its coordinator manager. Dropping it or
// closing the manager does not complete the durable use.
type Use struct {
	mu         sync.Mutex
	manager    *Manager
	resource   Resource
	key, token string
	finished   bool
}

// AcquireUse persists pending evidence before the caller creates or mounts
// the resource. Previously existing storage retains unknown legacy coverage.
func (m *Manager) AcquireUse(ctx context.Context, r Resource) (*Use, error) {
	key, err := m.key(r)
	if err != nil {
		return nil, err
	}
	f, fresh, err := m.lock(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlock(f)
	v, err := m.read(key, r)
	if errors.Is(err, os.ErrNotExist) {
		_, statErr := os.Lstat(r.Location)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		now := time.Now().UTC()
		v = &record{Version: 1, Resource: r, CompleteCoverage: fresh && errors.Is(statErr, os.ErrNotExist), Created: now, LastUse: now, Uses: map[string]bool{}}
	} else if err != nil {
		return nil, fmt.Errorf("read storage registration: %w", err)
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	v.Uses[token] = false
	v.LastUse = time.Now().UTC()
	if err = m.write(key, v); err != nil {
		return nil, err
	}
	return &Use{manager: m, resource: r, key: key, token: token}, nil
}

// Finish records positive completion. Call only after all consumers are proven
// to have exited; exec.Cmd.Wait and process-group kill alone are insufficient.
func (u *Use) Finish(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finished {
		return nil
	}
	f, _, err := u.manager.lock(ctx, u.key)
	if err != nil {
		return err
	}
	defer unlock(f)
	v, err := u.manager.read(u.key, u.resource)
	if err != nil {
		return err
	}
	if _, ok := v.Uses[u.token]; !ok {
		return ErrUnsafe
	}
	v.Uses[u.token] = true
	v.LastUse = time.Now().UTC()
	if err = u.manager.write(u.key, v); err != nil {
		return err
	}
	u.finished = true
	return nil
}

// Maintenance holds the admission mutex until Close. It never removes data.
type Maintenance struct {
	once sync.Once
	file *os.File
}

func (g *Maintenance) Close() error { g.once.Do(func() { unlock(g.file) }); return nil }
func (m *Manager) AcquireMaintenance(ctx context.Context, r Resource) (*Maintenance, error) {
	key, err := m.key(r)
	if err != nil {
		return nil, err
	}
	f, _, err := m.lock(ctx, key)
	if err != nil {
		return nil, err
	}
	v, err := m.read(key, r)
	if err != nil {
		unlock(f)
		return nil, fmt.Errorf("%w: %v", ErrUnsafe, err)
	}
	if !v.CompleteCoverage {
		unlock(f)
		return nil, ErrUnsafe
	}
	for _, complete := range v.Uses {
		if !complete {
			unlock(f)
			return nil, ErrUnsafe
		}
	}
	return &Maintenance{file: f}, nil
}

// Observation contains no use tokens. Active is unknown unless complete
// coverage exists or an unresolved use positively establishes activity.
type Observation struct {
	CompleteCoverage bool
	LastUse          time.Time
	Active           *bool
}

// Observe opens existing control storage only. It never admits a use, acquires
// a lock, completes a pending use, or updates recency. reserve is called before
// reading or decoding any record; the cap includes JSON map expansion.
func Observe(ctx context.Context, path string, resource Resource, maxBytes int64, reserve func(int64) error) (Observation, error) {
	var out Observation
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) || maxBytes <= 0 || maxBytes > 1<<20 || reserve == nil {
		return out, ErrUnsafe
	}
	volume := filepath.VolumeName(path) + string(filepath.Separator)
	base, err := safefs.Open(volume)
	if err != nil {
		return out, err
	}
	defer base.Close()
	rel, err := filepath.Rel(volume, filepath.Clean(path))
	if err != nil {
		return out, err
	}
	root, err := base.Sub(rel, false, 0)
	if err != nil {
		return out, err
	}
	defer root.Close()
	f, err := root.OpenDirectory(".")
	if err != nil {
		return out, err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return out, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return out, ErrUnsafe
	}
	m := &Manager{root: root, path: filepath.Clean(path)}
	key, err := m.key(resource)
	if err != nil {
		return out, err
	}
	if err := reserve(4096 + maxBytes*16); err != nil {
		return out, err
	}
	data, err := root.ReadFile(key+".json", maxBytes)
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	var v record
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return out, err
	}
	if d.Decode(new(any)) != io.EOF || v.Version != 1 || v.Resource != resource || v.Uses == nil || v.Created.IsZero() || v.LastUse.Before(v.Created) {
		return out, ErrUnsafe
	}
	active := false
	for token, complete := range v.Uses {
		if err := ctx.Err(); err != nil {
			return Observation{}, err
		}
		if len(token) != 64 {
			return Observation{}, ErrUnsafe
		}
		if _, err := hex.DecodeString(token); err != nil {
			return Observation{}, ErrUnsafe
		}
		if !complete {
			active = true
		}
	}
	out.CompleteCoverage = v.CompleteCoverage
	out.LastUse = v.LastUse
	if active || v.CompleteCoverage {
		out.Active = &active
	}
	return out, nil
}
