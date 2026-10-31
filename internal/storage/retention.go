package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
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
			err := refs.VisitStateRefs(s.ctx, tracker.InventoryBudget{Records: s.b.Limits.Records, Pages: 2, Bytes: 1 << 20, Reserve: s.b.Charge}, func(number int, sha string) error {
				issuePath := filepath.Join(root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", number))
				s.add(Resource{Repository: owner, Owner: owner, Class: "issue-worktree", Path: issuePath, Identity: "worktree:" + issuePath})
				data, err := store.LoadInventoryObject(s.ctx, sha, s.b.Charge)
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
		listingErr = visitor.VisitPullRequests(s.ctx, tracker.InventoryBudget{Records: s.b.Limits.Records, Pages: 100, Bytes: 4 << 20, Reserve: s.b.Charge}, func(pr *tracker.PullRequest) error {
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
