package coordination

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RefMove is one ledger ref renamed from the branch to the hidden namespace.
type RefMove struct {
	From string `json:"from"`
	To   string `json:"to"`
	SHA  string `json:"sha"`
}

// MigrationReport describes a ledger namespace migration snapshot.
type MigrationReport struct {
	Moves []RefMove `json:"moves"`
	// AlreadyHidden counts ledger refs already in the hidden namespace.
	AlreadyHidden int  `json:"already_hidden"`
	DryRun        bool `json:"dry_run"`
}

// beforeMigrationPush lets tests race the snapshot against the push.
var beforeMigrationPush func()

// MigrateLedgerRefs moves every branch-namespace ledger ref (state, checkpoint
// retention, and contributor indexes) to the hidden namespace in one
// git push --atomic. Each move creates the hidden ref with an absent-ref lease
// and deletes the branch ref with an exact-SHA lease, so a concurrent change to
// any enumerated ref rejects the whole push. It refuses while any issue holds
// an active claim, rejects ambiguous or malformed ledgers, and is a no-op once
// nothing remains to move. Issues first created after the snapshot are not
// fenced; run it with workers and coordinator dispatch paused.
func (s *GitStore) MigrateLedgerRefs(ctx context.Context, now time.Time, dryRun bool) (*MigrationReport, error) {
	pushURL, err := s.singleDestination(ctx, "ledger migration")
	if err != nil {
		return nil, err
	}
	refs, err := s.listRemoteRefs(ctx)
	if err != nil {
		return nil, err
	}
	report := &MigrationReport{Moves: []RefMove{}, DryRun: dryRun}
	var states []string
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		layout, ok := ledgerLayoutOf(ref)
		if !ok {
			continue
		}
		if err := validateLedgerRef(layout, ref, refs[ref]); err != nil {
			return nil, err
		}
		if strings.HasPrefix(ref, layout.StatePrefix()) {
			states = append(states, ref)
		}
		if layout == HiddenLayout {
			report.AlreadyHidden++
			continue
		}
		to, _ := layout.counterpartRef(ref)
		if _, dup := refs[to]; dup {
			return nil, fmt.Errorf("%w: both %s and %s exist; resolve the duplicate by hand before migrating", ErrLedgerNamespace, ref, to)
		}
		report.Moves = append(report.Moves, RefMove{From: ref, To: to, SHA: refs[ref]})
	}
	if err := s.refuseActiveClaims(ctx, refs, states, now); err != nil {
		return nil, err
	}
	if len(report.Moves) == 0 || dryRun {
		return report, nil
	}
	// The push needs every moved object locally; retention refs may name
	// checkpoints this clone never fetched.
	var missing []string
	for _, move := range report.Moves {
		if _, err := s.run(ctx, "", "cat-file", "-e", move.SHA); err != nil {
			missing = append(missing, move.SHA)
		}
	}
	if len(missing) > 0 {
		if _, err := s.run(ctx, "", append([]string{"fetch", "--no-write-fetch-head", s.Remote()}, missing...)...); err != nil {
			return nil, fmt.Errorf("fetch ledger objects before migration: %w", err)
		}
	}
	push := []string{"push", "--atomic", pushURL}
	for _, move := range report.Moves {
		push = append(push, "--force-with-lease="+move.From+":"+move.SHA, "--force-with-lease="+move.To+":")
	}
	for _, move := range report.Moves {
		push = append(push, move.SHA+":"+move.To, ":"+move.From)
	}
	if beforeMigrationPush != nil {
		beforeMigrationPush()
	}
	if _, err := s.run(ctx, "", push...); err != nil {
		return nil, fmt.Errorf("%w: atomic ledger migration rejected; no refs changed: %v", ErrConflict, err)
	}
	return report, nil
}

// listRemoteRefs reads every ref on the upstream remote.
func (s *GitStore) listRemoteRefs(ctx context.Context) (map[string]string, error) {
	out, err := s.run(ctx, "", "ls-remote", "--refs", s.Remote())
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid ls-remote result %q", line)
		}
		refs[fields[1]] = fields[0]
	}
	return refs, nil
}

// refuseActiveClaims decodes every state ref and fails on an unexpired active
// lease or an undecodable state.
func (s *GitStore) refuseActiveClaims(ctx context.Context, refs map[string]string, states []string, now time.Time) error {
	if len(states) == 0 {
		return nil
	}
	shas := make([]string, 0, len(states))
	for _, ref := range states {
		shas = append(shas, refs[ref])
	}
	if _, err := s.run(ctx, "", append([]string{"fetch", "--no-write-fetch-head", s.Remote()}, shas...)...); err != nil {
		return fmt.Errorf("fetch ledger state before migration: %w", err)
	}
	var active []string
	for _, ref := range states {
		body, err := s.run(ctx, "", "show", refs[ref]+":state.json")
		if err != nil {
			return fmt.Errorf("read %s: %w", ref, err)
		}
		var st struct {
			Lease struct {
				Status    string    `json:"status"`
				ExpiresAt time.Time `json:"expires_at"`
			} `json:"lease"`
		}
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			return fmt.Errorf("decode %s: %w", ref, err)
		}
		if st.Lease.Status == "active" && st.Lease.ExpiresAt.After(now) {
			active = append(active, ref)
		}
	}
	if len(active) > 0 {
		return fmt.Errorf("ledger migration refused: active claims in %s; wait for them to end or be released, pause workers, then rerun", strings.Join(active, ", "))
	}
	return nil
}

// ledgerLayoutOf reports which namespace's ledger families hold ref.
func ledgerLayoutOf(ref string) (RefLayout, bool) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		if _, ok := layout.counterpartRef(ref); ok {
			return layout, true
		}
	}
	return RefLayout{}, false
}

// validateLedgerRef checks the shape SwarmOS writes, so migration never
// carries an unknown ref into the hidden namespace.
func validateLedgerRef(layout RefLayout, ref, sha string) error {
	invalid := fmt.Errorf("unexpected ledger ref %s; remove or rename it before migrating", ref)
	if rest, ok := strings.CutPrefix(ref, layout.StatePrefix()); ok {
		if !validIssueName(rest) {
			return invalid
		}
		return nil
	}
	if rest, ok := strings.CutPrefix(ref, layout.CheckpointPrefix()); ok {
		issue, head, found := strings.Cut(rest, "/")
		if !found || !validIssueName(issue) || len(head) != 40 || strings.ToLower(head) != head {
			return invalid
		}
		if _, err := hex.DecodeString(head); err != nil {
			return invalid
		}
		if head != sha {
			return fmt.Errorf("%w: retention ref %s contains %s", ErrConflict, ref, sha)
		}
		return nil
	}
	actor, _ := strings.CutPrefix(ref, layout.ContributorPrefix())
	if actor == "" || actor != strings.ToLower(actor) || strings.ContainsAny(actor, " /\\:~^?*[]@") || strings.Contains(actor, "..") {
		return invalid
	}
	return nil
}

func validIssueName(name string) bool {
	digits, ok := strings.CutPrefix(name, "issue-")
	n, err := strconv.Atoi(digits)
	return ok && err == nil && n > 0 && strconv.Itoa(n) == digits
}
