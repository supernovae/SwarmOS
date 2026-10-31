package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

func (s *scanner) retention(owner, root, backend string, start int) {
	if s.opts.Backend != "" {
		backend = s.opts.Backend
	}
	// Local coordination files are authoritative only for an explicitly selected
	// local backend. A familiar directory in a GitHub checkout grants no authority.
	states := map[int]lease.InventoryState{}
	retentionComplete := true
	if backend == "local" {
		store := coordination.NewFileStore(root)
		s.directory(filepath.Join(root, ".swarm", "coordination"), func(e os.DirEntry) {
			name := e.Name()
			if !strings.HasPrefix(name, "issue-") || !strings.HasSuffix(name, ".json") {
				return
			}
			number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "issue-"), ".json"))
			if err != nil || number < 1 {
				return
			}
			state, err := lease.ReadInventoryState(s.ctx, store, number, s.b.Charge)
			if err != nil {
				retentionComplete = false
				s.fail("authoritative retention", err)
				return
			}
			if err = s.b.Charge(4096); err != nil {
				retentionComplete = false
				s.fail("authoritative retention", err)
				return
			}
			states[number] = state
			// Registrations remain visible when their directory or branch disappeared.
			for _, item := range []struct{ class, path string }{{"issue-worktree", filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", number))}, {"pr-worktree", filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("pr-%d", state.PR))}} {
				if item.class == "pr-worktree" && state.PR == 0 {
					continue
				}
				s.add(Resource{Repository: owner, Owner: owner, Class: item.class, Path: item.path, Identity: "worktree:" + item.path})
			}
			if strings.HasPrefix(state.Branch, "swarm/") {
				common := ""
				for p, o := range s.repos {
					if o == owner {
						common = p
						break
					}
				}
				s.add(Resource{Repository: owner, Owner: owner, Class: "code-branch", Path: root, Ref: "refs/heads/" + state.Branch, Identity: "ref:" + common + ":refs/heads/" + state.Branch})
			}
		})
	}
	var visitor tracker.PullRequestVisitor
	var listingErr error
	if backend == "github" || len(s.out.Resources) > start {
		if s.opts.Tracker == nil {
			listingErr = errors.New("bounded tracker unavailable")
		} else {
			visitor, listingErr = s.opts.Tracker(owner, root, backend)
		}
	}
	if backend == "github" {
		refs, ok := visitor.(tracker.StateRefVisitor)
		if !ok {
			retentionComplete = false
			s.fail("authoritative retention", errors.New("bounded state refs unavailable"))
		} else {
			store := coordination.NewGitStore(root)
			err := refs.VisitStateRefs(s.ctx, tracker.InventoryBudget{Records: s.b.Limits.Records, Pages: 2, Bytes: 16 << 20, Reserve: s.b.Charge, Workspace: s.b.Workspace}, func(number int, sha string) error {
				issuePath := filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", number))
				s.add(Resource{Repository: owner, Owner: owner, Class: "issue-worktree", Path: issuePath, Identity: "worktree:" + issuePath})
				data, err := store.LoadInventoryObject(s.ctx, sha, s.b.Charge, lease.ProjectInventory)
				if err != nil {
					retentionComplete = false
					s.fail("authoritative retention", err)
					return s.ctx.Err()
				}
				state, err := lease.DecodeInventoryState(s.ctx, data, number)
				if err != nil {
					retentionComplete = false
					s.fail("authoritative retention", err)
					return nil
				}
				if err = s.b.Charge(4096); err != nil {
					return err
				}
				states[number] = state
				if state.PR > 0 {
					path := filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("pr-%d", state.PR))
					s.add(Resource{Repository: owner, Owner: owner, Class: "pr-worktree", Path: path, Identity: "worktree:" + path})
				}
				if strings.HasPrefix(state.Branch, "swarm/") {
					common := ""
					for p, o := range s.repos {
						if o == owner {
							common = p
							break
						}
					}
					s.add(Resource{Repository: owner, Owner: owner, Class: "code-branch", Path: root, Ref: "refs/heads/" + state.Branch, Identity: "ref:" + common + ":refs/heads/" + state.Branch})
				}
				return s.ctx.Err()
			})
			if err != nil {
				retentionComplete = false
				s.fail("authoritative retention", err)
			}
		}
	}
	if len(s.out.Resources) == start {
		return
	}
	prs := map[int]*tracker.PullRequest{}
	if visitor == nil && listingErr == nil {
		listingErr = errors.New("bounded tracker unavailable")
	}
	if listingErr == nil {
		listingErr = visitor.VisitPullRequests(s.ctx, tracker.InventoryBudget{Records: s.b.Limits.Records, Pages: 100, Bytes: 128 << 20, Reserve: s.b.Charge, Workspace: s.b.Workspace}, func(pr *tracker.PullRequest) error {
			if pr == nil || pr.Number < 1 {
				return errors.New("invalid PR identity")
			}
			if _, ok := prs[pr.Number]; ok {
				return errors.New("repeated PR identity makes snapshot ambiguous")
			}
			if err := s.b.Charge(4096); err != nil {
				return err
			}
			prs[pr.Number] = pr
			return nil
		})
	}

	if listingErr != nil {
		s.fail("pull request retention", listingErr)
	}
	for i := start; i < len(s.out.Resources); i++ {
		r := &s.out.Resources[i]
		if r.Eligibility == "blocked" || r.Eligibility == "retained" {
			continue
		}
		var state *lease.InventoryState
		for _, candidate := range states {
			issuePath := filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", candidate.Issue))
			prPath := filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("pr-%d", candidate.PR))
			if r.Path == issuePath || candidate.PR > 0 && r.Path == prPath || candidate.Branch != "" && r.Ref == "refs/heads/"+candidate.Branch {
				if state != nil {
					state = nil
					break
				} // ambiguous linkage cannot establish authority
				copy := candidate
				state = &copy
			}
		}
		if state == nil {
			continue
		}
		r.Evidence = "authoritative issue state (" + backend + ")"
		if state.Active || state.Pending {
			r.Eligibility = "blocked"
			r.Reason = "active attempt or pending revision/reservation; expiry does not establish abandonment"
			if state.Active {
				active := true
				r.Active = &active
			}
			continue
		}
		if !retentionComplete {
			r.Reason = "authoritative retention snapshot incomplete; eligibility unknown"
			continue
		}
		if listingErr != nil {
			r.Reason = "PR retention traversal incomplete; eligibility unknown"
			continue
		}
		pr := prs[state.PR]
		if pr == nil {
			r.Reason = "recorded PR absent from bounded snapshot; eligibility unknown"
			continue
		}
		if pr.State == "open" {
			r.Eligibility = "retained"
			r.Reason = "authoritatively linked PR remains open"
			continue
		}
		if pr.Merged && pr.MergedAt != nil && pr.MergeCommitSHA != "" && state.SubmittedHead != "" && pr.HeadSHA == state.SubmittedHead {
			r.Evidence = "authoritative issue state and merged PR snapshot"
			r.Reason = "merged submission observed; complete worktree/head inspection required for eligibility"
			if !state.Completed || state.ForkRepo != "" || !strings.HasPrefix(state.Branch, fmt.Sprintf("swarm/issue-%d-", state.Issue)) || pr.State != "closed" || pr.Head != state.Branch || pr.Base == "" {
				continue
			}
			if backend == "github" && (!strings.EqualFold(pr.HeadRepo, owner) || !strings.EqualFold(pr.BaseRepo, owner)) {
				continue
			}
			openUse := false
			for _, other := range prs {
				if other.State != "open" {
					continue
				}
				if backend == "github" && other.HeadRepo == "" {
					openUse = true
					break
				}
				if other.Head == state.Branch || other.Head == fmt.Sprintf("swarm/pr-%d", state.PR) {
					openUse = true
					break
				}
			}
			if openUse {
				r.Eligibility = "retained"
				r.Reason = "open PR retention evidence requires this branch"
				continue
			}
			if !strings.HasSuffix(r.Class, "worktree") || r.LogicalBytes == nil {
				continue
			}
			expectedBranch := state.Branch
			if r.Class == "pr-worktree" {
				expectedBranch = fmt.Sprintf("swarm/pr-%d", state.PR)
			}
			inspection := workspace.InspectInventoryWorktree(s.ctx, root, r.Path, expectedBranch, state.SubmittedHead, s.git, func() error { return s.b.entry(s.ctx) }, s.b.Charge)
			if !inspection.Complete {
				s.fail("worktree inspection", errors.New("incomplete"))
				r.Reason = inspection.Reason
				continue
			}
			if inspection.Blocked {
				r.Eligibility = "blocked"
				r.Reason = inspection.Reason
				continue
			}
			inactive := false
			r.Active = &inactive
			r.Eligibility = "eligible"
			r.Reason = "completed merged submission and bounded clean worktree observation; advisory only"

		}
	}
}

// RepositoryScope is supplied by the coordinator, never by a registration.
// CommonDirectory must match Git's independently observed canonical identity.
type RepositoryScope struct{ Root, CommonDirectory, Backend string }

// RetentionEvidence describes one complete coherent authoritative snapshot. In
// apply it is read freshly under caller-held exact-resource maintenance exclusion
// through deletion. Remote claims cannot consume this host storage. Preview
// evidence is advisory and must never be reused as apply authority.
type RetentionEvidence struct {
	Complete, Stable, Unfinished bool
	Reason                       string
}

// RetentionSource implementations pin a coherent state set before visiting.
// Apply refreshes Git objects first; preview never fetches or acquires admission.
// Sources must not acquire storage-use admission locks: the caller owns exclusion.
type RetentionSource interface {
	WithRetention(context.Context, RepositoryScope, func(RetentionEvidence) error) error
}

type retentionFunc func(context.Context, RepositoryScope, func(RetentionEvidence) error) error

func (f retentionFunc) WithRetention(c context.Context, s RepositoryScope, v func(RetentionEvidence) error) error {
	return f(c, s, v)
}

type ReclaimOptions struct {
	Scopes                              []RepositoryScope
	CacheRoot, ScratchRoot, ControlRoot string
	Limits                              Limits
	Retention                           RetentionSource
}

type ReclamationOutcome string

const (
	ReclaimEligible ReclamationOutcome = "eligible"
	ReclaimRetained ReclamationOutcome = "retained"
	ReclaimRemoved  ReclamationOutcome = "removed"
	ReclaimAbsent   ReclamationOutcome = "absent"
	ReclaimFailed   ReclamationOutcome = "failed"
	ReclaimPartial  ReclamationOutcome = "partial"
	ReclaimCanceled ReclamationOutcome = "canceled"
)

// UsageObservation describes managed namespace observations, not physical savings.
// Nil values mean unknown. Hardlinks and filesystem sharing can prevent freeing
// any physical space even when the observed allocated bytes disappear.
type UsageObservation struct{ LogicalBytes, AllocatedBytes *int64 }
type ReclamationResult struct {
	Resource                                     storageuse.Resource
	Outcome                                      ReclamationOutcome
	Reason                                       string
	Before, After                                UsageObservation
	ObservedLogicalDelta, ObservedAllocatedDelta *int64
}

// ReclamationReport totals cover inspected admitted leaves only; retained unknown
// or outside-scope resources have no size claim.
type ReclamationReport struct {
	Complete                                     bool
	Resources                                    []ReclamationResult
	Before, After                                UsageObservation
	ObservedLogicalDelta, ObservedAllocatedDelta *int64
}

// PreviewReclamation observes only existing resources; it creates no admission
// locks or control records. Its result cannot authorize a later apply.
func PreviewReclamation(ctx context.Context, opts ReclaimOptions) (ReclamationReport, error) {
	return reclaim(ctx, opts, false)
}

// ApplyReclamation discovers candidates afresh and holds maintenance exclusion
// and fresh authoritative retention evidence through removal and observation.
func ApplyReclamation(ctx context.Context, opts ReclaimOptions) (ReclamationReport, error) {
	return reclaim(ctx, opts, true)
}

func reclaimScanner(ctx context.Context, opts ReclaimOptions) *scanner {
	return &scanner{ctx: ctx, opts: Options{CacheRoot: opts.CacheRoot, ScratchRoot: opts.ScratchRoot, UseRoot: opts.ControlRoot}, b: Budget{Limits: opts.Limits}, seen: map[string]bool{}, inodes: map[fileIdentity]bool{}, repos: map[string]string{}, physical: true, out: Snapshot{Complete: true}}
}

func canonicalScope(s *scanner, scope RepositoryScope) (RepositoryScope, error) {
	if !filepath.IsAbs(scope.Root) || !filepath.IsAbs(scope.CommonDirectory) || (scope.Backend != "local" && scope.Backend != "github") {
		return scope, errors.New("explicit absolute repository scope and backend required")
	}
	root, err := filepath.EvalSymlinks(scope.Root)
	if err != nil {
		return scope, err
	}
	anchor, err := openExisting(root)
	if err != nil {
		return scope, err
	}
	anchor.Close()
	common, err := s.git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return scope, err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return scope, err
	}
	supplied, err := filepath.EvalSymlinks(scope.CommonDirectory)
	if err != nil || supplied != common {
		return scope, errors.New("repository common-directory mismatch")
	}
	scope.Root, scope.CommonDirectory = root, common
	return scope, nil
}

// reclaimLayout independently checks exact producer leaf layouts. Category and
// repository parents are never leaves. Registrations alone convey no authority.
func reclaimLayout(o ReclaimOptions, r storageuse.Resource, key string) bool {
	if r.Repository != key || r.Location != filepath.Clean(r.Location) {
		return false
	}
	rel, err := filepath.Rel(o.CacheRoot, r.Location)
	parts := strings.Split(rel, string(filepath.Separator))
	cache := func(class string, count int) bool {
		return err == nil && filepath.IsLocal(rel) && len(parts) == count && parts[0] == class && parts[1] == key && privateLayout(o.CacheRoot, rel)
	}
	switch r.Class {
	case "agent-build", "validate-build", "agent-deps", "validate-deps":
		if r.Identity != "cache:"+r.Location || !cache(r.Class, 3) {
			return false
		}
		switch parts[2] {
		case "go", "rust", "node", "python", "c-cpp":
			return true
		}
		return false
	case "legacy-go-build":
		return r.Identity == "cache:"+r.Location && cache("go-build", 2)
	case "native-scratch", "sandbox-scratch", "prefetch-scratch", "review-scratch", "validation-scratch":
		if r.Identity != "scratch:"+r.Location {
			return false
		}
		scratchRel, e := filepath.Rel(o.ScratchRoot, r.Location)
		valid := e == nil && filepath.IsLocal(scratchRel) && filepath.Base(scratchRel) == scratchRel && scratchClass(scratchRel) == r.Class
		if !valid && r.Class == "prefetch-scratch" {
			valid = err == nil && filepath.IsLocal(rel) && len(parts) == 3 && (parts[0] == "agent-deps" || parts[0] == "validate-deps") && parts[1] == key && strings.HasPrefix(parts[2], ".prefetch-rust-") && len(parts[2]) > len(".prefetch-rust-") && privateLayout(o.CacheRoot, filepath.Dir(rel))
		}
		if !valid {
			return false
		}
		leaf, e := openExisting(r.Location)
		if e != nil {
			return false
		}
		defer leaf.Close()
		f, e := leaf.OpenDirectory(".")
		if e != nil {
			return false
		}
		defer f.Close()
		info, e := f.Stat()
		return e == nil && info.Mode().Perm()&0077 == 0
	}
	return false
}

func usage(s *scanner, path string) UsageObservation {
	r := Resource{Path: path}
	s.account(&r)
	return UsageObservation{r.LogicalBytes, r.AllocatedBytes}
}
func usageDelta(before, after UsageObservation) (*int64, *int64) {
	delta := func(a, b *int64) *int64 {
		if a == nil || b == nil {
			return nil
		}
		n := *a - *b
		return &n
	}
	return delta(before.LogicalBytes, after.LogicalBytes), delta(before.AllocatedBytes, after.AllocatedBytes)
}

// reclamationIO provides per-operation fault injection without package globals.
type reclamationIO struct {
	remove    func(*safefs.Root, context.Context, string, os.FileInfo, func() error) error
	afterStat func(*safefs.Root, string) (os.FileInfo, error)
}

func reclaim(ctx context.Context, opts ReclaimOptions, apply bool) (ReclamationReport, error) {
	return reclaimWithIO(ctx, opts, apply, reclamationIO{
		remove: func(p *safefs.Root, ctx context.Context, name string, info os.FileInfo, visit func() error) error {
			return p.RemoveAllCheckedContext(ctx, name, info, visit)
		},
		afterStat: func(p *safefs.Root, name string) (os.FileInfo, error) { return p.Lstat(name) },
	})
}

func reclaimWithIO(ctx context.Context, opts ReclaimOptions, apply bool, ops reclamationIO) (ReclamationReport, error) {
	out := ReclamationReport{Complete: true, Resources: []ReclamationResult{}}
	invalid := func(err error) (ReclamationReport, error) { out.Complete = false; return out, err }
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	l := opts.Limits
	if len(opts.Scopes) == 0 || len(opts.Scopes) > l.Repositories || l.Records < 1 || l.Entries < 1 || l.Identities < 1 || l.Bytes < 4096 {
		return invalid(errors.New("explicit scopes and positive shared limits required"))
	}
	roots := []*string{&opts.CacheRoot, &opts.ScratchRoot, &opts.ControlRoot}
	for _, p := range roots {
		if !filepath.IsAbs(*p) || filepath.Clean(*p) == string(filepath.Separator) {
			return invalid(errors.New("explicit trusted roots required"))
		}
		// Resolve an existing prefix without creating missing producer roots.
		prefix := *p
		tail := []string{}
		var q string
		for {
			var err error
			q, err = filepath.EvalSymlinks(prefix)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return invalid(err)
			}
			parent := filepath.Dir(prefix)
			if parent == prefix {
				return invalid(err)
			}
			tail = append(tail, filepath.Base(prefix))
			prefix = parent
		}
		for i := len(tail) - 1; i >= 0; i-- {
			q = filepath.Join(q, tail[i])
		}
		*p = q
	}
	overlap := func(a, b string) bool {
		rel, e := filepath.Rel(a, b)
		return e == nil && (rel == "." || filepath.IsLocal(rel))
	}
	for i, a := range roots {
		for j, b := range roots {
			if i != j && overlap(*a, *b) {
				return invalid(errors.New("trusted storage roots overlap"))
			}
		}
	}
	s := reclaimScanner(ctx, opts)
	scopes := map[string]RepositoryScope{}
	commonInfo := map[string]os.FileInfo{}
	for _, requested := range opts.Scopes {
		scope, err := canonicalScope(s, requested)
		if err != nil {
			return invalid(err)
		}
		if overlap(opts.CacheRoot, scope.Root) || overlap(scope.Root, opts.CacheRoot) || overlap(opts.ControlRoot, scope.Root) || overlap(scope.Root, opts.ControlRoot) {
			return invalid(errors.New("repository overlaps managed roots"))
		}
		h := sha256.Sum256([]byte(scope.CommonDirectory))
		key := hex.EncodeToString(h[:16])
		if prior, ok := scopes[key]; ok && prior.Backend != scope.Backend {
			return invalid(errors.New("conflicting repository backends"))
		}
		anchor, e := openExisting(scope.CommonDirectory)
		if e != nil {
			return invalid(e)
		}
		observed, e := anchor.Lstat(".")
		anchor.Close()
		if e != nil {
			return invalid(e)
		}
		commonInfo[key] = observed
		scopes[key] = scope
	}
	var manager *storageuse.Manager
	defer func() {
		if manager != nil {
			manager.Close()
		}
	}()
	var candidates []storageuse.Resource
	err := storageuse.VisitResources(ctx, opts.ControlRoot, l.Records, func() error { return s.b.entry(ctx) }, s.b.Workspace, func(r storageuse.Resource) error {
		if e := s.b.Charge(int64(4096 + 8*(len(r.Location)+len(r.Identity)+len(r.Class)+len(r.Repository)))); e != nil {
			return e
		}
		// Prune absent completed evidence as it is read. Even if a later report
		// bound is reached, this pass makes progress and a rerun can finish.
		// Resource deletion still waits for complete candidate discovery below.
		if apply && storageuse.LocationAbsent(r.Location) {
			if _, selected := scopes[r.Repository]; selected {
				if manager == nil {
					var e error
					manager, e = storageuse.Open(opts.ControlRoot, opts.CacheRoot, opts.ScratchRoot)
					if e != nil {
						return e
					}
				}
				pruned, e := manager.PruneAbsent(ctx, r)
				if e != nil && !errors.Is(e, storageuse.ErrUnsafe) {
					return e
				}
				if pruned {
					out.Resources = append(out.Resources, ReclamationResult{Resource: r, Outcome: ReclaimAbsent, Reason: "completed absent registration pruned; admission lock retained for waiting producers"})
					return nil
				}
			}
		}
		candidates = append(candidates, r)
		return nil
	}, apply)
	// An absent control root has no admitted resources. Discovery still reports
	// unregistered candidates, retaining them without initializing any roots.
	if errors.Is(err, os.ErrNotExist) && len(candidates) == 0 {
		err = nil
	}
	// Incomplete discovery cannot authorize even a previously observed candidate.
	if err != nil {
		out.Complete = false
		for _, r := range candidates {
			out.Resources = append(out.Resources, ReclamationResult{Resource: r, Outcome: ReclaimRetained, Reason: "registration discovery incomplete"})
		}
		return out, err
	}
	// Prefix-only and orphan candidates remain visible, but never acquire
	// deletion authority. Use the existing bounded, read-only observers.
	discovered := reclaimScanner(ctx, opts)
	discovered.b = s.b
	for _, scope := range scopes {
		discovered.repos[scope.CommonDirectory] = scope.Root
	}
	discovered.caches()
	discovered.scratch()
	s.b = discovered.b
	registered := map[string]bool{}
	for _, r := range candidates {
		registered[r.Location] = true
	}
	for _, r := range discovered.out.Resources {
		if !registered[r.Path] {
			out.Resources = append(out.Resources, ReclamationResult{Resource: storageuse.Resource{Repository: r.useRepository, Class: r.Class, Identity: r.Identity, Location: r.Path}, Outcome: ReclaimRetained, Reason: "unregistered or orphan resource; ownership and use coverage unproven"})
		}
	}
	if !discovered.out.Complete {
		out.Complete = false
		for _, r := range candidates {
			out.Resources = append(out.Resources, ReclamationResult{Resource: r, Outcome: ReclaimRetained, Reason: "bounded candidate discovery incomplete"})
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errors.New("candidate discovery incomplete")
	}
	if apply && len(candidates) > 0 && manager == nil {
		manager, err = storageuse.Open(opts.ControlRoot, opts.CacheRoot, opts.ScratchRoot)
		if err != nil {
			return invalid(err)
		}
	}
	afterScanner := reclaimScanner(ctx, opts)
	observeAfter := func(path string) UsageObservation {
		afterScanner.b = s.b
		u := usage(afterScanner, path)
		s.b = afterScanner.b
		return u
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Location < candidates[j].Location })
	counts := map[string]int{}
	for _, r := range candidates {
		counts[r.Location]++
	}
	previewEvidence := map[string]RetentionEvidence{}
	var operationErr error
	var logicalBefore, logicalAfter int64
	logicalComplete := true
	for _, r := range candidates {
		if err := ctx.Err(); err != nil {
			out.Complete = false
			return out, err
		}
		result := ReclamationResult{Resource: r, Outcome: ReclaimRetained, Reason: "outside explicit scope or unsupported/unsafe producer leaf"}
		if counts[r.Location] != 1 {
			result.Reason = "conflicting or duplicate registration"
			out.Resources = append(out.Resources, result)
			continue
		}
		scope, ok := scopes[r.Repository]
		protected := false
		for _, repository := range scopes {
			if overlap(r.Location, repository.Root) || overlap(repository.Root, r.Location) || overlap(r.Location, repository.CommonDirectory) || overlap(repository.CommonDirectory, r.Location) {
				protected = true
			}
		}
		if protected {
			result.Reason = "resource overlaps repository or Git evidence"
			out.Resources = append(out.Resources, result)
			continue
		}
		// Dead completed registrations need no retention lookup, but pruning still
		// requires this resource's exact maintenance exclusion and selected scope.
		if storageuse.LocationAbsent(r.Location) {
			result.Outcome = ReclaimAbsent
			result.Reason = "registered leaf absent"
			if apply && ok {
				pruned, e := manager.PruneAbsent(ctx, r)
				if e != nil {
					result.Outcome = ReclaimRetained
					result.Reason = "absent registration retained: " + e.Error()
				} else if pruned {
					result.Reason = "completed absent registration pruned; admission lock retained for waiting producers"
				}
			}
			out.Resources = append(out.Resources, result)
			continue
		}
		// Observe absence separately; it is never claimed as a new saving.
		parent, e := openExisting(filepath.Dir(r.Location))
		if e != nil {
			out.Resources = append(out.Resources, result)
			continue
		}
		info, e := parent.Lstat(filepath.Base(r.Location))
		parent.Close()
		if errors.Is(e, os.ErrNotExist) {
			result.Outcome = ReclaimAbsent
			result.Reason = "registered leaf absent"
			out.Resources = append(out.Resources, result)
			continue
		}
		if e != nil || !ok || !reclaimLayout(opts, r, r.Repository) {
			out.Resources = append(out.Resources, result)
			continue
		}
		if !apply {
			result.Before = usage(s, r.Location)
		}
		process := func() error {
			if opts.Retention == nil {
				result.Reason = "authoritative retention provider unavailable"
				return nil
			}
			called := false
			source := opts.Retention
			if !apply {
				source = retentionFunc(func(c context.Context, sc RepositoryScope, v func(RetentionEvidence) error) error {
					if ev, ok := previewEvidence[sc.CommonDirectory]; ok {
						return v(ev)
					}
					return opts.Retention.WithRetention(c, sc, func(ev RetentionEvidence) error {
						previewEvidence[sc.CommonDirectory] = ev
						return v(ev)
					})
				})
			}
			return source.WithRetention(ctx, scope, func(ev RetentionEvidence) error {
				if called {
					return errors.New("repeated retention callback")
				}
				called = true
				result.Reason = "authoritative retention incomplete or unstable: " + ev.Reason
				if !ev.Complete || !ev.Stable {
					out.Complete = false
					return nil
				}
				if strings.HasSuffix(r.Class, "scratch") && ev.Unfinished {
					result.Reason = "unfinished work requires recovery retention"
					return nil
				}
				commonRoot, e := openExisting(scope.CommonDirectory)
				if e != nil {
					result.Reason = "repository identity unavailable"
					return nil
				}
				observed, e := commonRoot.Lstat(".")
				commonRoot.Close()
				if e != nil || !os.SameFile(observed, commonInfo[r.Repository]) {
					result.Reason = "repository identity changed"
					return nil
				}
				fresh, e := canonicalScope(s, scope)
				if e != nil || fresh.CommonDirectory != scope.CommonDirectory || !reclaimLayout(opts, r, r.Repository) {
					result.Reason = "ownership or layout changed"
					return nil
				}
				result.Reason = "complete idle coverage and stable retention evidence"
				if !apply {
					result.Outcome = ReclaimEligible
					result.Reason += "; advisory preview only"
					return nil
				}
				parent, e := openExisting(filepath.Dir(r.Location))
				if e != nil {
					result.Outcome = ReclaimFailed
					return e
				}
				defer parent.Close()
				current, e := parent.Lstat(filepath.Base(r.Location))
				if e != nil {
					result.Outcome = ReclaimFailed
					return e
				}
				if !os.SameFile(info, current) {
					result.Reason = "resource identity changed"
					return nil
				}
				if e = ctx.Err(); e != nil {
					return e
				}
				// Reobserve usage under exclusion; missing usage remains advisory.
				result.Before = usage(s, r.Location)

				result.Outcome = ReclaimPartial
				e = ops.remove(parent, ctx, filepath.Base(r.Location), info, func() error { return ctx.Err() })
				_, afterErr := ops.afterStat(parent, filepath.Base(r.Location))
				if errors.Is(afterErr, os.ErrNotExist) {
					zero := int64(0)
					result.After = UsageObservation{&zero, &zero}
					if e == nil {
						result.Outcome = ReclaimRemoved
					}
				} else if afterErr != nil {
					e = errors.Join(e, afterErr)
				} else {
					result.After = observeAfter(r.Location)
				}
				result.ObservedLogicalDelta, result.ObservedAllocatedDelta = usageDelta(result.Before, result.After)
				return e
			})
		}
		if apply {
			guard, e := manager.AcquireMaintenance(ctx, r)
			if e != nil {
				result.Reason = "use evidence missing, incomplete or unresolved"
				if !errors.Is(e, storageuse.ErrUnsafe) {
					result.Outcome = ReclaimFailed
					err = e
				}
			} else {
				result.Before = usage(s, r.Location)
				err = process()
				if result.After.LogicalBytes == nil && result.Outcome != ReclaimPartial {
					result.After = observeAfter(r.Location)
				}
				guard.Close()
				if err == nil && result.Outcome == ReclaimRemoved {
					pruned, pruneErr := manager.PruneAbsent(ctx, r)
					if pruneErr != nil {
						err = pruneErr
					} else if pruned {
						result.Reason += "; registration pruned; admission lock retained for waiting producers"
					}
				}
			}
		} else {
			observation, e := storageuse.Observe(ctx, opts.ControlRoot, r, 16<<10, s.b.Workspace)
			if e != nil || !observation.CompleteCoverage || observation.Active == nil || *observation.Active {
				result.Reason = "use evidence missing, incomplete or unresolved"
			} else {
				err = process()
			}
		}
		if err != nil {
			out.Complete = false
			if result.Outcome == ReclaimRemoved {
				result.Outcome = ReclaimPartial
			}
			operationErr = errors.Join(operationErr, err)
			result.Reason = "resource operation failed; evidence preserved"
			if errors.Is(err, safefs.ErrCleanupLimit) {
				result.Outcome = ReclaimRetained
				result.Reason = "cleanup traversal limit reached; leaf and ownership evidence retained"
			}
		}
		if ctx.Err() != nil {
			result.Outcome = ReclaimCanceled
			out.Complete = false
		}
		if result.After.LogicalBytes == nil && result.Outcome != ReclaimRemoved && result.Outcome != ReclaimPartial && result.Outcome != ReclaimCanceled {
			result.After = observeAfter(r.Location)
		}
		if result.Before.LogicalBytes == nil || result.After.LogicalBytes == nil {
			logicalComplete = false
		} else if *result.Before.LogicalBytes > math.MaxInt64-logicalBefore || *result.After.LogicalBytes > math.MaxInt64-logicalAfter {
			logicalComplete = false
		} else {
			logicalBefore += *result.Before.LogicalBytes
			logicalAfter += *result.After.LogicalBytes
		}
		out.Resources = append(out.Resources, result)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		err = nil
	}
	if !s.out.Complete || !afterScanner.out.Complete {
		out.Complete = false
	}
	if logicalComplete {
		out.Before.LogicalBytes = &logicalBefore
		out.After.LogicalBytes = &logicalAfter
	}
	// Aggregate physical observations use one device/inode set per pass. Actual
	// freed physical space remains unknown, including for external hardlinks.
	if s.physical && s.out.Complete {
		n := s.out.ObservedAllocatedBytes
		out.Before.AllocatedBytes = &n
	}
	if afterScanner.physical && afterScanner.out.Complete {
		n := afterScanner.out.ObservedAllocatedBytes
		out.After.AllocatedBytes = &n
	}
	out.ObservedLogicalDelta, out.ObservedAllocatedDelta = usageDelta(out.Before, out.After)
	return out, operationErr
}

// BackendRetention reads bounded authoritative state and PR evidence. Apply is
// selected by the coordinator, never by resource registration metadata.
type BackendRetention struct {
	Failures map[string]error
	Budget   *Budget
	Apply    bool
	Remote   string
	Tracker  func(RepositoryScope) (tracker.PullRequestVisitor, error)
}

// Each object is decoded serially; only compact output survives the read.
// Snapshot readers reserve reusable transport/parser workspace separately from
// their small retained ref entries. Projection storage is charged by the decoder.
func inventoryReservation(b *Budget) func(int64) error {
	return func(n int64) error {
		if n < 8192 {
			return b.Charge(n)
		}
		return b.Workspace(n)
	}
}
func inventoryProjection(b *Budget) coordination.InventoryDecoder {
	return func(ctx context.Context, r io.Reader, reserve func(int64) error) ([]byte, error) {
		// ProjectInventory caps output at 16 KiB; include the retained map entry.
		if err := b.Charge(17 << 10); err != nil {
			return nil, err
		}
		return lease.ProjectInventory(ctx, r, reserve)
	}
}

// ReadRetentionSnapshot is shared by advisory task inspection and reclamation.
func ReadRetentionSnapshot(ctx context.Context, scope RepositoryScope, remote string, apply bool, budget *Budget, visitors ...tracker.PullRequestVisitor) (states map[int][]byte, resultErr error) {
	defer func() {
		for n, data := range states {
			if data == nil {
				continue
			}
			if _, err := lease.DecodeInventoryState(ctx, data, n); err != nil {
				states[n] = nil
				resultErr = errors.Join(resultErr, fmt.Errorf("issue %d: %w", n, err))
			}
		}
	}()
	if scope.Backend == "local" {
		return coordination.NewFileStore(scope.Root).ReadInventorySnapshot(ctx, budget.Limits.Records, inventoryReservation(budget), inventoryProjection(budget))
	}
	if scope.Backend == "github" {
		store := coordination.NewGitStoreWithRemote(scope.Root, remote)
		if apply {
			return store.ReadInventorySnapshot(ctx, true, budget.Limits.Records, inventoryReservation(budget), inventoryProjection(budget))
		}
		if len(visitors) != 1 {
			return nil, errors.New("bounded pinned state visitor unavailable")
		}
		refs, ok := visitors[0].(tracker.StateRefVisitor)
		if !ok {
			return nil, errors.New("bounded pinned state visitor unavailable")
		}
		pinned := map[int]string{}
		err := refs.VisitStateRefs(ctx, tracker.InventoryBudget{Records: budget.Limits.Records, Pages: 2, Bytes: 16 << 20, Reserve: budget.Charge, Workspace: budget.Workspace}, func(n int, sha string) error {
			if n < 1 || pinned[n] != "" {
				return errors.New("ambiguous state snapshot")
			}
			if err := budget.Charge(1024); err != nil {
				return err
			}
			pinned[n] = sha
			return nil
		})
		if err != nil {
			return nil, err
		}
		out := map[int][]byte{}
		var failures []error
		for n, sha := range pinned {
			data, err := store.LoadInventoryObject(ctx, sha, inventoryReservation(budget), inventoryProjection(budget))
			if err != nil {
				failures = append(failures, fmt.Errorf("issue %d: %w", n, err))
				out[n] = nil
				continue
			}
			out[n] = data
		}
		return out, errors.Join(failures...)
	}
	return nil, errors.New("unsupported retention backend")
}
func (r BackendRetention) WithRetention(ctx context.Context, scope RepositoryScope, visit func(RetentionEvidence) error) error {
	ev := RetentionEvidence{}
	fail := func(err error) error {
		ev.Reason = err.Error()
		if r.Failures != nil {
			r.Failures[scope.CommonDirectory] = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return visit(ev)
	}
	b := &Budget{Limits: DefaultLimits()}
	if r.Budget != nil {
		b.Limits = r.Budget.Limits
	}
	if r.Tracker == nil {
		return fail(errors.New("bounded PR evidence unavailable"))
	}
	tr, err := r.Tracker(scope)
	if err != nil {
		return fail(err)
	}
	states, err := ReadRetentionSnapshot(ctx, scope, r.Remote, r.Apply, b, tr)
	if err != nil {
		return fail(err)
	}
	prs := map[int]*tracker.PullRequest{}
	err = tr.VisitPullRequests(ctx, tracker.InventoryBudget{Records: b.Limits.Records, Pages: 100, Bytes: 128 << 20, Reserve: b.Charge, Workspace: b.Workspace}, func(pr *tracker.PullRequest) error {
		if pr == nil || pr.Number < 1 || prs[pr.Number] != nil {
			return errors.New("ambiguous PR evidence")
		}
		if err := b.Charge(4096); err != nil {
			return err
		}
		prs[pr.Number] = pr
		return nil
	})
	if err != nil {
		return fail(err)
	}
	used := map[int]bool{}
	for n, data := range states {
		st, e := lease.DecodeInventoryState(ctx, data, n)
		if e != nil {
			return fail(e)
		}
		pr := prs[st.PR]
		if st.PR > 0 && used[st.PR] {
			return fail(errors.New("ambiguous issue PR linkage"))
		}
		used[st.PR] = true
		if st.Active || st.Pending || !st.Completed || st.ForkRepo != "" || !strings.HasPrefix(st.Branch, fmt.Sprintf("swarm/issue-%d-", n)) || st.PR == 0 || pr == nil || pr.State != "closed" || !pr.Merged || pr.MergedAt == nil || pr.MergeCommitSHA == "" || st.SubmittedHead == "" || pr.HeadSHA != st.SubmittedHead || pr.Head != st.Branch {
			ev.Unfinished = true
		}
		if scope.Backend == "github" && pr != nil {
			upstream, ok := tr.(interface{ UpstreamRepo() string })
			if !ok || upstream.UpstreamRepo() == "" || !strings.EqualFold(pr.HeadRepo, upstream.UpstreamRepo()) || !strings.EqualFold(pr.BaseRepo, upstream.UpstreamRepo()) {
				ev.Unfinished = true
			}
		}
	}
	for _, pr := range prs {
		if pr.State == "open" || !used[pr.Number] {
			ev.Unfinished = true
		}
	}
	ev.Complete = true
	ev.Stable = true
	ev.Reason = "fresh coherent authority under host-local maintenance exclusion"
	if !r.Apply {
		ev.Reason = "coherent pinned preview; eligibility is advisory"
	}
	return visit(ev)
}
