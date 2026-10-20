package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/filelock"
)

// PublishingStore atomically advances both the authoritative state ref and a
// checkpoint branch. Only GitStore implements it.
type PublishingStore interface {
	CompareAndSwapAndPublish(context.Context, int, string, []byte, string, string) (string, error)
}

var ErrConflict = errors.New("coordination compare-and-swap conflict")
var ErrNotFound = errors.New("coordination state not found")

type Store interface {
	Load(context.Context, int) ([]byte, string, error)
	CompareAndSwap(context.Context, int, string, []byte) (string, error)
}
type FileStore struct{ root string }

func NewFileStore(root string) *FileStore {
	return &FileStore{root: filepath.Join(root, ".swarm", "coordination")}
}
func (s *FileStore) path(n int) string { return filepath.Join(s.root, fmt.Sprintf("issue-%d.json", n)) }
func (s *FileStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(s.path(n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return b, digest(b), nil
}
func (s *FileStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(s.path(n)+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err = filelock.LockExclusive(f); err != nil {
		return "", err
	}
	defer filelock.Unlock(f)
	current := ""
	if b, err := os.ReadFile(s.path(n)); err == nil {
		current = digest(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if current != old {
		return "", ErrConflict
	}
	tmp := s.path(n) + ".tmp"
	if err = os.WriteFile(tmp, next, 0644); err != nil {
		return "", err
	}
	if err = os.Rename(tmp, s.path(n)); err != nil {
		return "", err
	}
	return digest(next), nil
}

type GitStore struct{ root string }

func NewGitStore(root string) *GitStore { return &GitStore{root: root} }
func (s *GitStore) ref(n int) string    { return fmt.Sprintf("refs/heads/swarm-state/issue-%d", n) }
func (s *GitStore) run(ctx context.Context, input string, args ...string) (string, error) {
	fullArgs := args
	for _, key := range []string{"SWARM_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(key)); tok != "" {
			basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
			fullArgs = append([]string{"-c", "http.https://github.com/.extraheader=AUTHORIZATION: basic " + basic}, args...)
			break
		}
	}
	c := exec.CommandContext(ctx, "git", fullArgs...)
	c.Dir = s.root
	c.Stdin = strings.NewReader(input)
	b, e := c.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
func (s *GitStore) remoteSHA(ctx context.Context, ref string) (string, error) {
	out, err := s.run(ctx, "", "ls-remote", "--refs", "origin", ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", ErrNotFound
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != ref {
		return "", fmt.Errorf("invalid ls-remote result for %s", ref)
	}
	return fields[0], nil
}
func (s *GitStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	ref := s.ref(n)
	sha, err := s.remoteSHA(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	if _, err = s.run(ctx, "", "fetch", "--no-write-fetch-head", "origin", sha); err != nil {
		return nil, "", fmt.Errorf("fetch pinned state %s: %w", ref, err)
	}
	body, err := s.run(ctx, "", "show", sha+":state.json")
	if err != nil {
		return nil, "", err
	}
	return []byte(body), sha, nil
}
func (s *GitStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := s.run(ctx, "swarm coordination state\n", args...)
	if err != nil {
		return "", err
	}
	ref := s.ref(n)
	leaseArg := ref + ":" + old
	if old == "" {
		leaseArg = ref + ":"
	}
	if _, err = s.run(ctx, "", "push", "origin", "--force-with-lease="+leaseArg, commit+":"+ref); err != nil {
		actual, readErr := s.remoteSHA(ctx, ref)
		if readErr == nil && actual != old {
			return "", ErrConflict
		}
		if errors.Is(readErr, ErrNotFound) && old != "" {
			return "", ErrConflict
		}
		return "", fmt.Errorf("push coordination state: %w", err)
	}
	return commit, nil
}
func (s *GitStore) CompareAndSwapAndPublish(ctx context.Context, n int, old string, next []byte, branch, head string) (string, error) {
	if !strings.HasPrefix(branch, "swarm/issue-") || strings.Contains(branch, "..") || head == "" {
		return "", fmt.Errorf("invalid checkpoint branch or head")
	}
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := s.run(ctx, "swarm coordination checkpoint\n", args...)
	if err != nil {
		return "", err
	}
	stateRef := s.ref(n)
	branchRef := "refs/heads/" + branch
	oldBranch, branchErr := s.remoteSHA(ctx, branchRef)
	if errors.Is(branchErr, ErrNotFound) {
		oldBranch = ""
	} else if branchErr != nil {
		return "", branchErr
	}
	stateLease := stateRef + ":" + old
	if old == "" {
		stateLease = stateRef + ":"
	}
	branchLease := branchRef + ":" + oldBranch
	if oldBranch == "" {
		branchLease = branchRef + ":"
	}
	if _, err = s.run(ctx, "", "push", "--atomic", "origin", "--force-with-lease="+stateLease, "--force-with-lease="+branchLease, commit+":"+stateRef, head+":"+branchRef); err != nil {
		return "", fmt.Errorf("atomic checkpoint push: %w", err)
	}
	return commit, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
