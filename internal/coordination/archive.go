package coordination

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
)

// ArchivedError distinguishes retained, relocated authority from a new issue.
type ArchivedError struct{ Issue int }

func (e *ArchivedError) Error() string {
	return fmt.Sprintf("issue #%d is archived; run swarm repo archive restore %d", e.Issue, e.Issue)
}

type ArchiveTombstone struct {
	Manifest    contract.ArchiveManifest `json:"manifest"`
	Digest      string                   `json:"digest"`
	ManifestTip string                   `json:"manifest_tip"`
}
type ArchiveIndex struct {
	Version    int                      `json:"version"`
	Generation string                   `json:"generation"`
	Entries    map[int]ArchiveTombstone `json:"entries"`
}

func parseArchiveIndex(data []byte) (*ArchiveIndex, error) {
	if len(data) > 64<<20 {
		return nil, fmt.Errorf("archive index too large")
	}
	if err := contract.ValidateArchiveJSON(data); err != nil {
		return nil, err
	}
	var keys struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, err
	}
	for key := range keys.Entries {
		n, err := strconv.Atoi(key)
		if err != nil || n <= 0 || strconv.Itoa(n) != key {
			return nil, fmt.Errorf("invalid archive index issue key")
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var idx ArchiveIndex
	if err := d.Decode(&idx); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing archive index data")
	}
	if idx.Version != 1 || idx.Entries == nil || idx.Generation == "" {
		return nil, fmt.Errorf("invalid archive index")
	}
	for n, t := range idx.Entries {
		_, digest, err := contract.MarshalArchiveManifest(&t.Manifest)
		if err != nil || n != t.Manifest.Issue || digest != t.Digest || validCheckpointSHA(t.ManifestTip) != nil {
			return nil, fmt.Errorf("invalid archive tombstone")
		}
	}
	return &idx, nil
}
func freshArchiveIndex() *ArchiveIndex {
	return &ArchiveIndex{Version: 1, Generation: "initial", Entries: map[int]ArchiveTombstone{}}
}

// archiveIndex never interprets an unreadable or invalid index as absence.
func (s *GitStore) archiveIndex(ctx context.Context, preview bool) (*ArchiveIndex, string, error) {
	tip, err := s.remoteSHA(ctx, s.layout.ArchiveRef())
	if errors.Is(err, ErrNotFound) {
		return freshArchiveIndex(), "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if !preview {
		if _, err = s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), tip); err != nil {
			return nil, "", err
		}
	}
	raw, err := s.run(ctx, "", "show", tip+":archive.json")
	if err != nil {
		return nil, "", fmt.Errorf("pinned archive index unavailable locally or invalid; fetch the index before preview: %w", err)
	}
	idx, err := parseArchiveIndex([]byte(raw))
	return idx, tip, err
}
func (s *GitStore) archiveAdmission(ctx context.Context, n int, preview bool) error {
	if err := s.checkLedger(ctx, s.ref(n)); err != nil {
		return err
	}
	idx, _, err := s.archiveIndex(ctx, preview)
	if err != nil {
		return err
	}
	if _, ok := idx.Entries[n]; ok {
		return &ArchivedError{n}
	}
	return nil
}
func (s *GitStore) archiveCommit(ctx context.Context, name string, data []byte, parent string) (string, error) {
	blob, err := s.run(ctx, string(data), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\t%s\n", blob, name), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return s.run(ctx, "swarm archive ledger\n", args...)
}
func (s *GitStore) indexCommit(ctx context.Context, idx *ArchiveIndex, parent string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	idx.Generation = hex.EncodeToString(nonce)
	data, err := json.Marshal(idx)
	if err != nil {
		return "", err
	}
	return s.archiveCommit(ctx, "archive.json", data, parent)
}

// initialArchiveFence forces a changed index update into the same transaction
// as state creation. Git omits unchanged updates, including their leases.
func (s *GitStore) initialArchiveFence(ctx context.Context, n int, old string) ([]string, error) {
	if err := s.checkLedger(ctx, s.ref(n)); err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	idx, tip, err := s.archiveIndex(ctx, false)
	if err != nil {
		return nil, err
	}
	if _, ok := idx.Entries[n]; ok {
		return nil, &ArchivedError{n}
	}
	if old != "" {
		return nil, nil
	}
	commit, err := s.indexCommit(ctx, idx, tip)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	return []string{"--atomic", "--force-with-lease=" + s.layout.ArchiveRef() + ":" + tip, commit + ":" + s.layout.ArchiveRef()}, nil
}

// ArchiveSnapshot pins exact names and objects without fetching or writing.
func (s *GitStore) ArchiveSnapshot(ctx context.Context, n int) ([]contract.ArchiveRef, string, error) {
	if n <= 0 {
		return nil, "", fmt.Errorf("invalid issue")
	}
	if err := s.archiveAdmission(ctx, n, true); err != nil {
		return nil, "", err
	}
	refs, err := s.listRemoteRefs(ctx)
	if err != nil {
		return nil, "", err
	}
	state := s.ref(n)
	tip, ok := refs[state]
	if !ok {
		return nil, "", ErrNotFound
	}
	var out []contract.ArchiveRef
	for ref, sha := range refs {
		if ref == state || strings.HasPrefix(ref, fmt.Sprintf("%sissue-%d/", s.layout.CheckpointPrefix(), n)) {
			if err := validateLedgerRef(s.layout, ref, sha); err != nil {
				return nil, "", err
			}
			other, _ := s.layout.counterpartRef(ref)
			if _, ok := refs[other]; ok {
				return nil, "", s.layout.ledgerConflict(ref, other, true)
			}
			dest := fmt.Sprintf("refs/archive/issue-%d/state", n)
			if ref != state {
				dest = fmt.Sprintf("refs/archive/issue-%d/checkpoints/%s", n, sha)
			}
			out = append(out, contract.ArchiveRef{Source: ref, Destination: dest, SHA: sha})
		}
		if strings.HasPrefix(ref, fmt.Sprintf("%sissue-%d/", s.layout.Counterpart().CheckpointPrefix(), n)) || ref == s.layout.Counterpart().StateRef(n) {
			return nil, "", ErrLedgerNamespace
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, tip, nil
}

func (s *GitStore) archiveScratch(ctx context.Context) (*GitStore, func(), error) {
	root, err := safefs.Open(s.root)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		root.Close()
		return nil, nil, err
	}
	name := ".swarm-archive-" + hex.EncodeToString(nonce)
	if err = root.Mkdir(name, 0700); err != nil {
		root.Close()
		return nil, nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	cleanup := func() { _ = root.RemoveAllChecked(name, info); _ = root.Close() }
	scratch := &GitStore{root: filepath.Join(s.root, name), layout: s.layout, remote: s.remote}
	if _, err = scratch.run(ctx, "", "init", "--bare", "."); err != nil {
		cleanup()
		return nil, nil, err
	}
	return scratch, cleanup, nil
}
func validateArchiveLocal(destination string) error {
	u, err := url.Parse(destination)
	if err != nil {
		return fmt.Errorf("invalid archive destination")
	}
	if u.Scheme != "file" {
		return nil
	}
	root, err := safefs.Open(string(filepath.Separator))
	if err != nil {
		return err
	}
	defer root.Close()
	sub, err := root.Sub(strings.TrimPrefix(u.Path, string(filepath.Separator)), false, 0)
	if err != nil {
		return fmt.Errorf("unsafe local archive directory: %w", err)
	}
	return sub.Close()
}
func (s *GitStore) archiveDestination(ctx context.Context, destination string) (string, error) {
	if err := contract.ValidateArchiveDestination(destination); err != nil {
		return "", err
	}
	if err := validateArchiveLocal(destination); err != nil {
		return "", err
	}
	effective, err := s.run(ctx, "", "ls-remote", "--get-url", destination)
	if err != nil {
		return "", err
	}
	if err = contract.ValidateArchiveDestination(effective); err != nil {
		return "", err
	}
	if err = validateArchiveLocal(effective); err != nil {
		return "", err
	}
	source, err := s.singleDestination(ctx, "archive")
	if err != nil {
		return "", err
	}
	// Canonicalize common local aliases before allowing any destination writes.
	canonical := func(v string) string {
		u, e := url.Parse(v)
		if e == nil && u.Scheme == "file" {
			v = u.Path
		}
		if !strings.Contains(v, ":") && !filepath.IsAbs(v) {
			v = filepath.Join(s.root, v)
		}
		if filepath.IsAbs(v) {
			if p, e := filepath.EvalSymlinks(v); e == nil {
				return filepath.Clean(p)
			}
		}
		return strings.TrimSuffix(v, "/")
	}
	if canonical(source) == canonical(destination) || canonical(source) == canonical(effective) {
		return "", fmt.Errorf("archive destination is the source repository")
	}
	return source, nil
}
func archiveManifestRef(n int) string { return fmt.Sprintf("refs/archive/issue-%d/manifest", n) }

// Archive copies and independently reads every object before the sole source
// mutation. The operator must provide an authoritative eligibility recheck.
func (s *GitStore) Archive(ctx context.Context, m *contract.ArchiveManifest, recheck func(context.Context) error) error {
	data, digest, err := contract.MarshalArchiveManifest(m)
	if err != nil {
		return err
	}
	m, err = contract.ParseArchiveManifest(data)
	if err != nil {
		return err
	}
	if recheck == nil {
		return fmt.Errorf("archive requires eligibility recheck")
	}
	if m.Namespace != s.layout.Name() {
		return fmt.Errorf("archive namespace mismatch")
	}
	source, err := s.archiveDestination(ctx, m.Destination)
	if err != nil {
		return err
	}
	refs, tip, err := s.ArchiveSnapshot(ctx, m.Issue)
	if err != nil {
		return err
	}
	if tip != m.StateTip || !reflect.DeepEqual(refs, m.Refs) {
		return ErrConflict
	}
	idx, indexTip, err := s.archiveIndex(ctx, false)
	if err != nil {
		return err
	}
	scratch, clean, err := s.archiveScratch(ctx)
	if err != nil {
		return err
	}
	defer clean()
	fetch := []string{"fetch", "--no-write-fetch-head", source}
	for _, r := range refs {
		fetch = append(fetch, r.SHA)
	}
	if _, err = scratch.run(ctx, "", fetch...); err != nil {
		return err
	}
	manifestTip, err := scratch.archiveCommit(ctx, "manifest.json", data, "")
	if err != nil {
		return err
	}
	// Existing manifests are accepted by bytes, not commit metadata (timestamps).
	existing, err := scratch.run(ctx, "", "ls-remote", "--refs", m.Destination, archiveManifestRef(m.Issue))
	if err != nil {
		return err
	}
	if fields := strings.Fields(existing); len(fields) > 0 {
		if len(fields) != 2 || fields[1] != archiveManifestRef(m.Issue) {
			return fmt.Errorf("invalid archive manifest ref")
		}
		if _, err = scratch.run(ctx, "", "fetch", "--no-write-fetch-head", m.Destination, fields[0]); err != nil {
			return err
		}
		body, e := scratch.run(ctx, "", "show", fields[0]+":manifest.json")
		if e != nil {
			return e
		}
		prior, e := contract.ParseArchiveManifest([]byte(body))
		if e != nil {
			return e
		}
		candidate := *m
		candidate.ArchivedAt = prior.ArchivedAt
		candidate.Actor = prior.Actor
		candidateBytes, _, e := contract.MarshalArchiveManifest(&candidate)
		if e != nil || string(candidateBytes) != body {
			return fmt.Errorf("archive manifest conflict; use a clean destination")
		}
		*m = *prior
		_, digest, err = contract.MarshalArchiveManifest(m)
		if err != nil {
			return err
		}
		manifestTip = fields[0]
	}
	push := []string{"push", "--atomic", m.Destination}
	for _, r := range append(append([]contract.ArchiveRef{}, refs...), contract.ArchiveRef{Destination: archiveManifestRef(m.Issue), SHA: manifestTip}) {
		out, e := scratch.run(ctx, "", "ls-remote", "--refs", m.Destination, r.Destination)
		if e != nil {
			return e
		}
		f := strings.Fields(out)
		if len(f) > 0 {
			if len(f) != 2 || f[1] != r.Destination || f[0] != r.SHA {
				return fmt.Errorf("archive ref conflict; use a clean destination")
			}
			continue
		}
		push = append(push, "--force-with-lease="+r.Destination+":", r.SHA+":"+r.Destination)
	}
	if len(push) > 3 {
		if err = scratch.push(ctx, push...); err != nil {
			return err
		}
	}
	tomb := ArchiveTombstone{Manifest: *m, Digest: digest, ManifestTip: manifestTip}
	if err = s.verifyArchive(ctx, tomb); err != nil {
		return err
	}
	if err = recheck(ctx); err != nil {
		return err
	}
	idx.Entries[m.Issue] = tomb
	// Index ancestry must be available in this object database.
	commit, err := s.indexCommit(ctx, idx, indexTip)
	if err != nil {
		return err
	}
	push = []string{"push", "--atomic", source, "--force-with-lease=" + s.layout.ArchiveRef() + ":" + indexTip, commit + ":" + s.layout.ArchiveRef()}
	for _, r := range refs {
		push = append(push, "--force-with-lease="+r.Source+":"+r.SHA, ":"+r.Source)
	}
	if err = s.push(ctx, push...); err != nil {
		current, readErr := s.listRemoteRefs(ctx)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		success := current[s.layout.ArchiveRef()] == commit
		for _, r := range refs {
			success = success && current[r.Source] == ""
		}
		if success {
			return nil
		}
		return fmt.Errorf("%w: archive source transaction: %w", ErrConflict, err)
	}
	return nil
}
func (s *GitStore) verifyArchive(ctx context.Context, t ArchiveTombstone) error {
	m := &t.Manifest
	scratch, clean, err := s.archiveScratch(ctx)
	if err != nil {
		return err
	}
	defer clean()
	refs := append(append([]contract.ArchiveRef{}, m.Refs...), contract.ArchiveRef{Destination: archiveManifestRef(m.Issue), SHA: t.ManifestTip})
	fetch := []string{"fetch", "--no-write-fetch-head", m.Destination}
	for _, r := range refs {
		out, e := scratch.run(ctx, "", "ls-remote", "--refs", m.Destination, r.Destination)
		if e != nil {
			return e
		}
		f := strings.Fields(out)
		if len(f) != 2 || f[0] != r.SHA || f[1] != r.Destination {
			return fmt.Errorf("archive read-back ref mismatch")
		}
		fetch = append(fetch, r.Destination)
	}
	if _, err = scratch.run(ctx, "", fetch...); err != nil {
		return err
	}
	for _, r := range refs {
		if _, err = scratch.run(ctx, "", "cat-file", "-e", r.SHA+"^{commit}"); err != nil {
			return err
		}
	}
	fsck := []string{"fsck", "--no-reflogs", "--no-dangling"}
	for _, r := range refs {
		fsck = append(fsck, r.SHA)
	}
	if _, err = scratch.run(ctx, "", fsck...); err != nil {
		return fmt.Errorf("archive object closure verification: %w", err)
	}

	body, err := scratch.run(ctx, "", "show", t.ManifestTip+":manifest.json")
	if err != nil {
		return err
	}
	parsed, err := contract.ParseArchiveManifest([]byte(body))
	if err != nil {
		return err
	}
	expected, digest, err := contract.MarshalArchiveManifest(m)
	if err != nil {
		return err
	}
	if string(expected) != body || digest != t.Digest || !reflect.DeepEqual(parsed, m) {
		return fmt.Errorf("archive manifest digest or identity mismatch")
	}
	return nil
}

func (s *GitStore) ArchiveTombstone(ctx context.Context, n int, preview bool) (*ArchiveTombstone, error) {
	idx, _, err := s.archiveIndex(ctx, preview)
	if err != nil {
		return nil, err
	}
	t, ok := idx.Entries[n]
	if !ok {
		return nil, ErrNotFound
	}
	return &t, nil
}
func (s *GitStore) RestoreArchive(ctx context.Context, n int, repository string, apply bool) (*ArchiveTombstone, error) {
	idx, tip, err := s.archiveIndex(ctx, !apply)
	if err != nil {
		return nil, err
	}
	t, ok := idx.Entries[n]
	if !ok {
		return nil, ErrNotFound
	}
	if t.Manifest.Repository != repository {
		return nil, fmt.Errorf("archive repository mismatch")
	}
	restoreRefs := append([]contract.ArchiveRef{}, t.Manifest.Refs...)
	original, _ := LayoutFor(t.Manifest.Namespace)
	if original != s.layout {
		for i := range restoreRefs {
			restoreRefs[i].Source, _ = original.counterpartRef(restoreRefs[i].Source)
		}
	}
	source, err := s.archiveDestination(ctx, t.Manifest.Destination)
	if err != nil {
		return nil, err
	}
	refs, err := s.listRemoteRefs(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range restoreRefs {
		other, _ := s.layout.counterpartRef(r.Source)
		if refs[r.Source] != "" || refs[other] != "" {
			return &t, fmt.Errorf("restore requires absent source refs")
		}
	}
	if !apply {
		return &t, nil
	}
	if err = s.verifyArchive(ctx, t); err != nil {
		return &t, err
	}
	fetch := []string{"fetch", "--no-write-fetch-head", t.Manifest.Destination}
	for _, r := range restoreRefs {
		fetch = append(fetch, r.SHA)
	}
	if _, err = s.run(ctx, "", fetch...); err != nil {
		return &t, err
	}
	delete(idx.Entries, n)
	commit, err := s.indexCommit(ctx, idx, tip)
	if err != nil {
		return &t, err
	}
	push := []string{"push", "--atomic", source, "--force-with-lease=" + s.layout.ArchiveRef() + ":" + tip, commit + ":" + s.layout.ArchiveRef()}
	for _, r := range restoreRefs {
		push = append(push, "--force-with-lease="+r.Source+":", r.SHA+":"+r.Source)
	}
	if err = s.push(ctx, push...); err != nil {
		current, readErr := s.listRemoteRefs(ctx)
		if readErr != nil {
			return &t, errors.Join(err, readErr)
		}
		completed := current[s.layout.ArchiveRef()] == commit
		for _, r := range restoreRefs {
			completed = completed && current[r.Source] == r.SHA
		}
		if completed {
			return &t, nil
		}
		return &t, fmt.Errorf("%w: restore transaction: %w", ErrConflict, err)
	}
	return &t, nil
}

// ArchiveContributorReferences checks every authoritative contributor index.
func (s *GitStore) ArchiveContributorReferences(ctx context.Context, n int) (bool, error) {
	refs, err := s.listRemoteRefs(ctx)
	if err != nil {
		return false, err
	}
	for ref, sha := range refs {
		if strings.HasPrefix(ref, s.layout.Counterpart().ContributorPrefix()) {
			return false, ErrLedgerNamespace
		}
		if !strings.HasPrefix(ref, s.layout.ContributorPrefix()) {
			continue
		}
		raw, e := s.run(ctx, "", "show", sha+":index.json")
		if e != nil {
			return false, e
		}
		var idx contributorIndex
		if e = json.Unmarshal([]byte(raw), &idx); e != nil || idx.Version != 1 || idx.Actor != strings.TrimPrefix(ref, s.layout.ContributorPrefix()) {
			return false, fmt.Errorf("invalid contributor index")
		}
		seen := map[int]bool{}
		for _, c := range idx.Claims {
			if c.IssueNumber <= 0 || c.ClaimID == "" || seen[c.IssueNumber] {
				return false, fmt.Errorf("invalid contributor index entry")
			}
			seen[c.IssueNumber] = true
			if c.IssueNumber == n {
				return true, nil
			}
		}
	}
	return false, nil
}

// CheckArchiveDestination validates routing without creating scratch stores.
func (s *GitStore) CheckArchiveDestination(ctx context.Context, destination string) error {
	_, err := s.archiveDestination(ctx, destination)
	return err
}

// CheckArchived admits operator reads without confusing archived authority with
// a missing issue. Store mutations independently repeat this check and fence.
func (s *GitStore) CheckArchived(ctx context.Context, n int) error {
	return s.archiveAdmission(ctx, n, false)
}
