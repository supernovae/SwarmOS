// Package storageuse keeps coordinator-owned, durable evidence of storage use.
// Interrupted uses remain pending unless explicit recovery proves that the
// coordinator and every exclusively contained PID-namespace consumer have ended.
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
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/safefs"
	"strings"
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
	Uses             map[string]bool         // false means unresolved; completed records are retained
	Evidence         map[string]*useEvidence `json:",omitempty"`
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
	return m.readLimited(key, r, 16<<20)
}
func (m *Manager) readLimited(key string, r Resource, limit int64) (*record, error) {
	b, err := m.root.ReadFile(key+".json", limit)
	if err != nil {
		return nil, err
	}
	if err := uniqueJSON(b); err != nil {
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
	if v.Resource != r || v.validate() != nil {
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
	// A pruned registration leaves its admission inode in place. Its marker
	// certifies completed coverage only until the next admission. Clear and
	// sync it before writing any pending token, so crashes cannot reuse it.
	marker := make([]byte, len(prunedMarker)+1)
	n, markerErr := f.ReadAt(marker, 0)
	pruned := (markerErr == nil || errors.Is(markerErr, io.EOF)) && string(marker[:n]) == prunedMarker
	if pruned {
		if err := f.Truncate(0); err != nil {
			return nil, err
		}
		if err := f.Sync(); err != nil {
			return nil, err
		}
	}
	v, err := m.read(key, r)
	if errors.Is(err, os.ErrNotExist) {
		_, statErr := os.Lstat(r.Location)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		now := time.Now().UTC()
		v = &record{Version: 1, Resource: r, CompleteCoverage: (fresh || pruned) && errors.Is(statErr, os.ErrNotExist), Created: now, LastUse: now, Uses: map[string]bool{}}
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

// Finish records completion after the execution boundary and all coordinator
// consumers finish. Host commands require exit plus successful StopVerified;
// Bubblewrap retains its stronger PID-namespace completion. Deliberately detached
// sessions are not tracked: cleanup may remove a cache beneath them, while they
// retain only their existing sandbox access. Unverified stops must not call Finish.
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
	once        sync.Once
	file        *os.File
	observation Observation
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
	active := false
	return &Maintenance{file: f, observation: Observation{CompleteCoverage: true, LastUse: v.LastUse, Active: &active}}, nil
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
	if err := uniqueJSON(data); err != nil {
		return out, err
	}
	var v record
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return out, err
	}
	if d.Decode(new(any)) != io.EOF || v.Resource != resource || v.validate() != nil {
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

// UserCacheDir resolves coordinator storage and can be replaced by package test
// harnesses alongside their producer cache resolver.
var UserCacheDir = os.UserCacheDir

// ControlRoot resolves private coordinator evidence separately from cache grants.
// It does not create storage and may also be used by read-only inventory.
func ControlRoot() (string, error) {
	base, err := UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "swarm-use"), nil
}

// ResourceFor is shared by producers and inventory. Repository is the canonical
// Git-common-directory cache key, never the display name of a repository.
func ResourceFor(repository, class, path string) Resource {
	prefix := "cache:"
	if strings.HasSuffix(class, "scratch") {
		prefix = "scratch:"
	}
	return Resource{Repository: repository, Class: class, Identity: prefix + filepath.Clean(path), Location: filepath.Clean(path)}
}

type scopeKey struct{}

type temporary struct {
	parent *safefs.Root
	name   string
	info   os.FileInfo
}

// Scope holds coordinator-only lifetime state. It is never serialized to workers.
// Retain permanently records uncertainty for this scope; neither cancellation nor
// a later successful command clears that uncertainty.
type Scope struct {
	mu               sync.Mutex
	repository       string
	forbidden        []string
	manager          *Manager
	uses             []*Use
	temps            map[string]temporary
	retained         bool
	outside          bool
	outsideResources []Resource
	closed           bool
}

func FromContext(ctx context.Context) *Scope { s, _ := ctx.Value(scopeKey{}).(*Scope); return s }
func (s *Scope) Repository() string          { return s.repository }

// Ensure installs a scope only when there is no enclosing coordinator owner.
// The returned owner is nil for inherited scopes. Creating a scope alone does
// not touch control storage, grant credentials or create claim authority.
func Ensure(ctx context.Context, repository string) (context.Context, *Scope, error) {
	if inherited := FromContext(ctx); inherited != nil {
		if filepath.IsAbs(repository) {
			inherited.mu.Lock()
			if inherited.manager != nil && overlaps(inherited.manager.path, repository) {
				inherited.mu.Unlock()
				return ctx, nil, errors.New("control root overlaps coordinator source")
			}
			inherited.forbidden = append(inherited.forbidden, repository)
			inherited.mu.Unlock()
		}
		return ctx, nil, nil
	}
	identity := "host-unknown"
	if raw, err := hex.DecodeString(repository); err == nil && len(raw) == 16 {
		identity = repository
	}
	if repository != "" && identity == "host-unknown" {
		cmd, _, err := gitcredential.Command(ctx, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err == nil {
			if out, err := cmd.Output(); err == nil {
				common := strings.TrimSpace(string(out))
				if filepath.IsAbs(common) {
					if resolved, err := filepath.EvalSymlinks(common); err == nil {
						common = resolved
					}
					sum := sha256.Sum256([]byte(filepath.Clean(common)))
					identity = hex.EncodeToString(sum[:16])
				}
			}
		}
	}
	s := &Scope{repository: identity}
	if filepath.IsAbs(repository) {
		s.forbidden = []string{repository}
	}
	return context.WithValue(ctx, scopeKey{}, s), s, nil
}

func (s *Scope) Acquire(ctx context.Context, resource Resource) error {
	return s.acquire(ctx, resource, false)
}

// AcquirePrepared admits coordinator-only directory preparation. A use remains
// pending unless every subsequent accessor has durable boundary evidence.
func (s *Scope) AcquirePrepared(ctx context.Context, resource Resource) error {
	return s.acquire(ctx, resource, true)
}

func (s *Scope) acquire(ctx context.Context, resource Resource, prepared bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("storage scope is closed")
	}
	if s.manager == nil {
		path, err := ControlRoot()
		if err != nil {
			return err
		}
		s.manager, err = Open(path, append(s.forbidden, resource.Location)...)
		if err != nil {
			return err
		}
	}
	use, err := s.manager.AcquireUse(ctx, resource)
	if err != nil {
		return err
	}
	s.uses = append(s.uses, use)
	if !prepared {
		s.outsideResources = append(s.outsideResources, resource)
		for _, prior := range s.uses {
			if !overlaps(prior.resource.Location, resource.Location) {
				continue
			}
			if err := prior.evidence(ctx, func(v *record) error {
				if e := v.Evidence[prior.token]; e != nil {
					e.Outside = true
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	if prepared {
		p, captureErr := CaptureProcess(os.Getpid())
		if captureErr == nil {
			return use.evidence(ctx, func(v *record) error {
				if v.Evidence == nil {
					v.Evidence = map[string]*useEvidence{}
				}
				outside := s.outside
				for _, prior := range s.outsideResources {
					outside = outside || overlaps(prior.Location, resource.Location)
				}
				v.Evidence[use.token] = &useEvidence{Coordinator: p, Outside: outside, Consumers: map[string]ProcessIdentity{}}
				return nil
			})
		}
	}
	return nil
}

func (s *Scope) Retain() { s.mu.Lock(); s.retained = true; s.mu.Unlock() }

// Finish runs independently of canceled work after all consumers have stopped.
// A retained scope closes handles but leaves every pending durable token intact.
func (s *Scope) Finish() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.manager == nil {
		return nil
	}
	defer s.manager.Close()
	defer func() {
		for _, temp := range s.temps {
			temp.parent.Close()
		}
	}()
	if s.retained {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result error
	for _, use := range s.uses {
		result = errors.Join(result, use.Finish(ctx))
	}
	return result
}

// MarkUnknown preserves legacy provenance before migrating into a fresh target.
func (s *Scope) MarkUnknown(ctx context.Context, resource Resource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manager == nil || s.closed {
		return ErrUnsafe
	}
	key, err := s.manager.key(resource)
	if err != nil {
		return err
	}
	f, _, err := s.manager.lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock(f)
	record, err := s.manager.read(key, resource)
	if err != nil {
		return err
	}
	record.CompleteCoverage = false
	return s.manager.write(key, record)
}

// TempDir admits the exact absent resource before exclusive, root-bound mkdir.
func (s *Scope) TempDir(ctx context.Context, parent, pattern, class string) (string, error) {
	if parent == "" {
		parent = os.TempDir()
		// The OS temporary directory has trusted platform aliases on macOS.
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", err
		}
		parent = resolved
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return "", err
	}
	// Validate all existing components before opening the pinned parent.
	volume := filepath.VolumeName(parent) + string(filepath.Separator)
	root, err := safefs.Open(volume)
	if err != nil {
		return "", err
	}
	defer root.Close()
	rel, err := filepath.Rel(volume, parent)
	if err != nil {
		return "", err
	}
	dir, err := root.Sub(rel, false, 0)
	if err != nil {
		return "", err
	}
	keepParent := false
	defer func() {
		if !keepParent {
			dir.Close()
		}
	}()
	for attempt := 0; attempt < 10; attempt++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", err
		}
		name := strings.Replace(pattern, "*", hex.EncodeToString(nonce[:]), 1)
		if name == pattern {
			name += hex.EncodeToString(nonce[:])
		}
		if filepath.Base(name) != name || !filepath.IsLocal(name) {
			return "", ErrUnsafe
		}
		path := filepath.Join(parent, name)
		if err := s.Acquire(ctx, ResourceFor(s.repository, class, path)); err != nil {
			return "", err
		}
		if err := dir.Mkdir(name, 0700); err != nil {
			if errors.Is(err, os.ErrExist) {
				if unknownErr := s.MarkUnknown(ctx, ResourceFor(s.repository, class, path)); unknownErr != nil {
					s.Retain()
					return "", unknownErr
				}
				continue
			}
			return "", err
		}
		info, err := dir.Lstat(name)
		if err != nil {
			s.Retain()
			return "", err
		}
		s.mu.Lock()
		if s.temps == nil {
			s.temps = map[string]temporary{}
		}
		s.temps[path] = temporary{dir, name, info}
		s.mu.Unlock()
		keepParent = true
		return path, nil
	}
	return "", errors.New("managed temporary directory collisions")
}

// MkdirTemp requires an enclosing owner so the use outlives allocation.
func MkdirTemp(ctx context.Context, parent, pattern, class string) (string, error) {
	s := FromContext(ctx)
	if s == nil {
		return "", errors.New("managed temporary allocation requires a storage scope")
	}
	return s.TempDir(ctx, parent, pattern, class)
}

// Retained reports unresolved completion without exposing tokens.
func (s *Scope) Retained() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.retained }

// Complete is an owner defer: panics leave unresolved evidence and propagate.
// Pass the named return error so a completion-write failure is never hidden.
func (s *Scope) Complete(result *error) {
	if p := recover(); p != nil {
		s.Retain()
		_ = s.Finish()
		panic(p)
	}
	if finishErr := s.Finish(); finishErr != nil {
		*result = errors.Join(*result, finishErr)
	}
}

// CleanupTemp is deferred directly by the allocation owner. Panics and uncertain
// stops preserve scratch; normal cleanup checks identity through the pinned parent.
func (s *Scope) CleanupTemp(path string, result *error) {
	if p := recover(); p != nil {
		s.Retain()
		panic(p)
	}

	s.cleanupTemp(path, result, func(temp temporary) error {
		return temp.parent.RemoveAllChecked(temp.name, temp.info)
	})
}

func (s *Scope) cleanupTemp(path string, result *error, remove func(temporary) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retained {
		return
	}
	temp, ok := s.temps[path]
	if !ok {
		s.retained = true
		*result = errors.Join(*result, ErrUnsafe)
		return
	}
	if cleanupErr := remove(temp); cleanupErr != nil {
		s.retained = true
		if !errors.Is(cleanupErr, safefs.ErrCleanupIncomplete) {
			*result = errors.Join(*result, cleanupErr)
		}
	}
}

// Covers reports whether an admitted managed temporary root encloses path.
// Nested fallback caches share that root's lifetime rather than inventing a
// persistent-cache identity outside the managed cache layout.
func (s *Scope) Covers(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for root := range s.temps {
		rel, err := filepath.Rel(root, path)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
}

// Observation returns token-free evidence pinned by the held admission lock.
func (g *Maintenance) Observation() Observation {
	out := g.observation
	active := false
	out.Active = &active
	return out
}

// ProcessIdentity is local coordinator evidence, never worker input.
type ProcessIdentity struct {
	Boot  string
	PID   int
	Start uint64
}

type useEvidence struct {
	Coordinator ProcessIdentity
	Outside     bool
	Consumers   map[string]ProcessIdentity
}

func validIdentity(p ProcessIdentity) bool {
	if p.PID <= 0 || p.Start == 0 || len(p.Boot) != 36 {
		return false
	}
	for i, c := range p.Boot {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// CaptureProcess reads Linux boot identity and start ticks. Other platforms and
// unavailable proc data cannot establish a recoverable identity.
func CaptureProcess(pid int) (ProcessIdentity, error) {
	if runtime.GOOS != "linux" || pid <= 0 {
		return ProcessIdentity{}, ErrUnsafe
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ProcessIdentity{}, err
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ProcessIdentity{}, err
	}
	start, err := processStart(data, pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	p := ProcessIdentity{Boot: strings.TrimSpace(string(boot)), PID: pid, Start: start}
	if !validIdentity(p) {
		return ProcessIdentity{}, ErrUnsafe
	}
	return p, nil
}

func processStart(data []byte, pid int) (uint64, error) {
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	begin := strings.IndexByte(text, '(')
	if begin < 1 || end <= begin || strings.TrimSpace(text[:begin]) != strconv.Itoa(pid) {
		return 0, ErrUnsafe
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return 0, ErrUnsafe
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, ErrUnsafe
	}
	return start, nil
}

func processGone(p ProcessIdentity) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return processGoneRead(p, os.ReadFile, os.Stat)
}
func processGoneRead(p ProcessIdentity, read func(string) ([]byte, error), stat func(string) (os.FileInfo, error)) bool {
	if !validIdentity(p) {
		return false
	}
	boot, err := read("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false
	}
	now := strings.TrimSpace(string(boot))
	check := p
	check.Boot = now
	if !validIdentity(check) {
		return false
	}
	if now != p.Boot {
		return true
	}
	data, err := read("/proc/" + strconv.Itoa(p.PID) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		// A missing stat file alone is not evidence that the PID entry is gone.
		_, pidErr := stat("/proc/" + strconv.Itoa(p.PID))
		return errors.Is(pidErr, os.ErrNotExist)
	}
	if err != nil {
		return false
	}
	start, err := processStart(data, p.PID)
	return err == nil && start != p.Start
}

func (v *record) validate() error {
	if v.Version != 1 || v.Uses == nil || v.Created.IsZero() || v.LastUse.Before(v.Created) {
		return ErrUnsafe
	}
	for token := range v.Uses {
		if len(token) != 64 {
			return ErrUnsafe
		}
		if _, err := hex.DecodeString(token); err != nil {
			return ErrUnsafe
		}
	}
	for token, evidence := range v.Evidence {
		if _, ok := v.Uses[token]; !ok || evidence == nil {
			return ErrUnsafe
		}
		// Incomplete identities and reservations deliberately remain readable and
		// pending; they never satisfy recovery.
		if len(evidence.Consumers) > 256 {
			return ErrUnsafe
		}
	}
	return nil
}

// Recover resolves only fully recorded, exclusively PID-namespace consumers.
// It is explicit: admission, observation and manager reopen never invoke it.
func (m *Manager) Recover(ctx context.Context, r Resource, now time.Time, reserve ...func(int64) error) (bool, error) {
	return m.recover(ctx, r, now, processGone, reserve...)
}

func (m *Manager) recover(ctx context.Context, r Resource, now time.Time, gone func(ProcessIdentity) bool, reserve ...func(int64) error) (bool, error) {
	key, err := m.key(r)
	if err != nil {
		return false, err
	}
	f, _, err := m.lock(ctx, key)
	if err != nil {
		return false, err
	}
	defer unlock(f)
	// Match the read-only inventory cap, rather than expanding large ledgers
	// during a bounded maintenance pass.
	info, err := m.root.Lstat(key + ".json")
	if err != nil {
		return false, err
	}
	if info.Size() > 16384 {
		return false, ErrUnsafe
	}
	if len(reserve) > 0 {
		if err := reserve[0](8192 + 128*info.Size()); err != nil {
			return false, err
		}
	}
	v, err := m.readLimited(key, r, info.Size())
	if err != nil {
		return false, err
	}
	changed := false
	for token, done := range v.Uses {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		e := v.Evidence[token]
		if done || e == nil || e.Outside || !validIdentity(e.Coordinator) || len(e.Consumers) == 0 || !gone(e.Coordinator) {
			continue
		}
		eligible := true
		seen := map[ProcessIdentity]bool{}
		for id, p := range e.Consumers {
			_, idErr := hex.DecodeString(id)
			if len(id) != 64 || idErr != nil || seen[p] || !validIdentity(p) || !gone(p) {
				eligible = false
				break
			}
			seen[p] = true
		}
		if eligible {
			v.Uses[token] = true
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.After(v.LastUse) {
		v.LastUse = now
	}
	if err := m.write(key, v); err != nil {
		return false, err
	}
	return true, nil
}

func (u *Use) evidence(ctx context.Context, change func(*record) error) error {
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
	if err := change(v); err != nil {
		return err
	}
	return u.manager.write(u.key, v)
}

// OutsideConsumer permanently disqualifies this scope, including later uses.
// Call before launching any native or otherwise unclassified consumer.
func (s *Scope) OutsideConsumer(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outside = true
	for _, u := range s.uses {
		if err := u.evidence(ctx, func(v *record) error {
			if e := v.Evidence[u.token]; e != nil {
				e.Outside = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// BoundaryReservation is coordinator-only launch state. It may only be sealed
// with the verified namespace init identity, never the launcher or a wrapper.
type BoundaryReservation struct {
	scope *Scope
	id    string
	uses  []*Use
}

// ReserveBoundary persists incomplete consumers before starting the process.
func (s *Scope) ReserveBoundary(ctx context.Context, locations []string) (*BoundaryReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	b := &BoundaryReservation{scope: s, id: hex.EncodeToString(nonce[:])}
	for _, u := range s.uses {
		matched := false
		for _, location := range locations {
			if overlaps(u.resource.Location, location) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if err := u.evidence(ctx, func(v *record) error {
			if e := v.Evidence[u.token]; e != nil && !e.Outside {
				if e.Consumers == nil {
					e.Consumers = map[string]ProcessIdentity{}
				}
				if len(e.Consumers) >= 256 {
					// Bound evidence growth without limiting the number of tool launches.
					// Once tracking is exhausted the use stays permanently nonqualifying.
					e.Outside = true
					return nil
				}
				e.Consumers[b.id] = ProcessIdentity{}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		b.uses = append(b.uses, u)
	}
	return b, nil
}

func (b *BoundaryReservation) Seal(ctx context.Context, p ProcessIdentity) error {
	if !validIdentity(p) {
		return ErrUnsafe
	}
	b.scope.mu.Lock()
	defer b.scope.mu.Unlock()
	for _, u := range b.uses {
		if err := u.evidence(ctx, func(v *record) error {
			if e := v.Evidence[u.token]; e != nil && !e.Outside {
				old, ok := e.Consumers[b.id]
				if !ok || old != (ProcessIdentity{}) {
					return ErrUnsafe
				}
				e.Consumers[b.id] = p
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// uniqueJSON rejects duplicate object keys instead of letting later consumers
// or evidence silently replace an earlier reservation during decoding.
func uniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return ErrUnsafe
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, compound := t.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrUnsafe
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return ErrUnsafe
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrUnsafe
	}
	return nil
}
