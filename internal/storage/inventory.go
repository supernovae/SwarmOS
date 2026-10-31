// Package storage provides bounded, read-only observations of managed storage.
// Observations never authorize reclamation.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/storageuse"
)

var ErrLimit = errors.New("storage inventory limit reached")

type Limits struct {
	Repositories int   `json:"repositories"`
	Records      int   `json:"records"`
	Entries      int   `json:"entries"`
	Identities   int   `json:"identities"`
	Bytes        int64 `json:"working_bytes"`
}

func DefaultLimits() Limits { return Limits{256, 10000, 100000, 100000, 64 << 20} }

type Counters struct {
	Repositories int   `json:"repositories"`
	Records      int   `json:"records"`
	Entries      int   `json:"entries"`
	Identities   int   `json:"identities"`
	Bytes        int64 `json:"working_bytes"`
}

// Budget is shared by discovery and accounting. A scanner uses one worker.
// Charges are conservative and never released during a pass.
type Budget struct {
	Limits   Limits
	Counters Counters
}

func (b *Budget) Charge(n int64) error {
	if n < 0 || n > b.Limits.Bytes-b.Counters.Bytes {
		return ErrLimit
	}
	b.Counters.Bytes += n
	return nil
}
func (b *Budget) entry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.Counters.Entries >= b.Limits.Entries {
		return ErrLimit
	}
	b.Counters.Entries++
	return nil
}

type Resource struct {
	Owner          string     `json:"owner"`
	Repository     string     `json:"repository"`
	Class          string     `json:"class"`
	Path           string     `json:"path"`
	Ref            string     `json:"ref"`
	Identity       string     `json:"identity"`
	LogicalBytes   *int64     `json:"logical_bytes"`
	AllocatedBytes *int64     `json:"allocated_bytes"`
	UsageReason    string     `json:"usage_reason"`
	LastUse        *time.Time `json:"last_use"`
	Evidence       string     `json:"evidence"`
	Active         *bool      `json:"active"`
	Eligibility    string     `json:"eligibility"`
	Reason         string     `json:"reason"`
}
type Snapshot struct {
	Version                int        `json:"version"`
	Scope                  string     `json:"scope"`
	Complete               bool       `json:"complete"`
	Limits                 Limits     `json:"limits"`
	Counters               Counters   `json:"counters"`
	Reasons                []string   `json:"incomplete_reasons"`
	Resources              []Resource `json:"resources"`
	UniqueAllocatedBytes   *int64     `json:"unique_allocated_bytes"`
	ObservedAllocatedBytes int64      `json:"observed_allocated_bytes"`
}
type Options struct {
	UseRoot      string // optional trusted coordinator control root; never inferred from candidate data
	RegistryPath string
	CacheRoot    string // user-cache/swarm, never the entire user cache
	ScratchRoot  string
	SelectedRoot string
	Repository   string
	Limits       Limits
}
type scanner struct {
	ctx      context.Context
	opts     Options
	b        Budget
	out      Snapshot
	seen     map[string]bool
	inodes   map[fileIdentity]bool
	repos    map[string]string
	physical bool
}

// Scan opens only existing paths. Missing evidence stays unknown. It never
// obtains credentials or calls cleanup, preparation, migration or tracker APIs.
func Scan(ctx context.Context, opts Options) (Snapshot, error) {
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	l := opts.Limits
	if l.Repositories < 1 || l.Records < 1 || l.Entries < 1 || l.Identities < 1 || l.Bytes < 4096 {
		return Snapshot{}, fmt.Errorf("positive inventory limits required")
	}
	s := &scanner{ctx: ctx, opts: opts, b: Budget{Limits: l}, seen: map[string]bool{}, inodes: map[fileIdentity]bool{}, repos: map[string]string{}, physical: true}
	s.out = Snapshot{Version: 1, Scope: "known repositories and host-wide managed cache/scratch candidates", Complete: true, Limits: l, Reasons: []string{}, Resources: []Resource{}}
	if opts.Repository != "" || opts.SelectedRoot != "" {
		s.out.Scope = "selected repositories and host-wide managed cache/scratch candidates"
	}
	entries, err := registry.New(opts.RegistryPath).ReadSnapshot(ctx, l.Repositories, s.b.Charge)
	if err != nil {
		s.fail("registry", err)
	}
	selected := false
	for _, e := range entries {
		if opts.Repository != "" && e.Repo != opts.Repository {
			continue
		}
		if opts.SelectedRoot != "" && filepath.Clean(e.Path) != filepath.Clean(opts.SelectedRoot) {
			continue
		}
		s.repository(e.Repo, e.Path)
		selected = true
	}
	if opts.SelectedRoot != "" {
		s.repository(opts.Repository, opts.SelectedRoot)
		selected = true
	}
	if opts.Repository != "" && !selected {
		s.fail("repository selection", errors.New("repository not present in read-only registry"))
	}
	if opts.Repository == "" && opts.SelectedRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			if root, err := s.git(cwd, "rev-parse", "--show-toplevel"); err == nil {
				s.repository("", root)
			}
		}
	}
	s.caches()
	s.scratch()
	sort.Slice(s.out.Resources, func(i, j int) bool { a, b := s.out.Resources[i], s.out.Resources[j]; return a.Identity < b.Identity })
	s.out.Counters = s.b.Counters
	if s.physical && s.out.Complete {
		v := s.out.ObservedAllocatedBytes
		s.out.UniqueAllocatedBytes = &v
	}
	return s.out, ctx.Err()
}
func (s *scanner) fail(area string, err error) {
	s.out.Complete = false
	// One bounded reason per area; never retain attacker-controlled diagnostics.
	reason := area + ": incomplete observation"
	if errors.Is(err, ErrLimit) {
		reason = area + ": inventory limit reached"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		reason = area + ": canceled"
	}
	for _, r := range s.out.Reasons {
		if r == reason {
			return
		}
	}
	if len(s.out.Reasons) < 32 {
		s.out.Reasons = append(s.out.Reasons, reason)
	}
}
func (s *scanner) add(r Resource) {
	if s.seen[r.Identity] {
		if r.Eligibility == "blocked" || r.Eligibility == "retained" {
			for i := range s.out.Resources {
				if s.out.Resources[i].Identity == r.Identity {
					s.out.Resources[i].Eligibility = r.Eligibility
					s.out.Resources[i].Reason = r.Reason
				}
			}
		}
		return
	}
	if err := s.ctx.Err(); err != nil {
		s.fail("resources", err)
		return
	}
	if s.b.Counters.Records >= s.b.Limits.Records {
		s.fail("resources", ErrLimit)
		return
	}
	if err := s.b.Charge(int64(2048 + 8*(len(r.Path)+len(r.Identity)+len(r.Repository)+len(r.Ref)))); err != nil {
		s.fail("resources", err)
		return
	}
	s.b.Counters.Records++
	s.seen[r.Identity] = true
	if strings.HasSuffix(r.Class, "worktree") {
		if cwd, err := os.Getwd(); err == nil && filepath.Clean(cwd) == r.Path {
			r.Eligibility = "retained"
			r.Reason = "invoking checkout is retained"
		}
	}
	if r.Owner == "" {
		r.Owner = "unknown"
	}
	if r.Eligibility == "" {
		r.Eligibility = "unknown"
		r.Reason = "complete authoritative use and retention evidence unavailable"
	}
	if s.opts.UseRoot != "" && r.Repository != "" && r.Ref == "" {
		observation, err := storageuse.Observe(s.ctx, s.opts.UseRoot, storageuse.Resource{Repository: r.Repository, Class: r.Class, Identity: r.Identity, Location: r.Path}, 16<<10, s.b.Charge)
		if err == nil {
			r.Active = observation.Active
			r.LastUse = &observation.LastUse
			r.Evidence = "coordinator durable storage-use record"
			if r.Active != nil && *r.Active {
				r.Eligibility = "blocked"
				r.Reason = "unresolved durable use"
			}
			if observation.CompleteCoverage && r.Active != nil && !*r.Active && r.Eligibility == "unknown" && strings.Contains(r.Class, "build") {
				r.Eligibility = "eligible"
				r.Reason = "complete durable use coverage with no unresolved use; observation only"
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			s.fail("use evidence", err)
		}
	}
	if r.Evidence == "" {
		r.Evidence = "no durable last-use evidence"
	}
	if r.Ref == "" {
		s.account(&r)
		if r.LogicalBytes == nil && r.Eligibility == "eligible" {
			r.Eligibility = "unknown"
			r.Reason = "filesystem observation incomplete; use evidence alone cannot establish eligibility"
		}
	} else {
		r.UsageReason = "branch shares repository Git objects; no independent byte total"
	}
	s.out.Resources = append(s.out.Resources, r)
}

// openExisting validates every component from the filesystem root without
// following symbolic links. It creates no lock or directory.
func openExisting(path string) (*safefs.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute path required")
	}
	base := filepath.VolumeName(path) + string(filepath.Separator)
	root, err := safefs.Open(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(base, filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return root.Sub(rel, false, 0)
}

// directory visits one entry at a time, including unknown names. The entry
// counter is shared with recursive usage accounting.
func (s *scanner) directory(path string, visit func(os.DirEntry)) {
	if err := s.ctx.Err(); err != nil {
		s.fail("directory", err)
		return
	}
	root, err := openExisting(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.fail("directory", err)
		return
	}
	defer root.Close()
	f, err := root.OpenDirectory(".")
	if err != nil {
		s.fail("directory", err)
		return
	}
	defer f.Close()
	for {
		if err := s.b.entry(s.ctx); err != nil {
			s.fail("directory", err)
			return
		}
		entries, err := f.ReadDir(1)
		if len(entries) == 0 {
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, io.EOF) {
				s.fail("directory", err)
			}
			return
		}
		e := entries[0]
		if err := s.b.Charge(int64(256 + 4*len(e.Name()))); err != nil {
			s.fail("directory", err)
			return
		}
		visit(e)
	}
}
