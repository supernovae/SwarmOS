package agent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// Rejected-run recovery patches share the diagnostics storage rules: a private
// state directory outside the worktree, fixed bounds, and pruning by age and
// count. Saved text is available for maintainer inspection and is never
// applied automatically.
const (
	// RecoveryPatchMaxBytes bounds the diff body, matching timeout patches.
	RecoveryPatchMaxBytes = 4 << 20
	recoveryHeaderBytes   = 4 << 10
	recoveryMagic         = "# swarm-recovery v1\n"
)

var (
	recoveryFileName = regexp.MustCompile(`^rejected-[0-9a-f]{32}\.patch$`)
	recoveryHeadSHA  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	recoveryPath     = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// Test seam; production code never reassigns it.
var recoveryNewName = func() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return "rejected-" + hex.EncodeToString(random[:]) + ".patch"
}

const (
	unavailableOversized  diagnosticsUnavailable = "patch exceeds size limit"
	unavailableCredential diagnosticsUnavailable = "patch contains a credential"
	unavailableHeader     diagnosticsUnavailable = "invalid recovery header"
)

// RecoveryHeader binds a recovery patch to the run that produced it.
type RecoveryHeader struct {
	Issue    int
	PR       int
	Role     string
	HeadSHA  string   // worktree head the rejected run started from
	Rejected []string // paths the import policy refused
}

func (h RecoveryHeader) validate() error {
	if h.Issue <= 0 || h.PR < 0 || (h.Role != "developer" && h.Role != "tester") || !recoveryHeadSHA.MatchString(h.HeadSHA) || len(h.Rejected) == 0 {
		return unavailableHeader
	}
	for _, path := range h.Rejected {
		if !recoveryPath.MatchString(path) {
			return unavailableHeader
		}
	}
	return nil
}

// SaveRecoveryPatch stores patch privately and returns the saved path. Errors
// are fixed reasons that never carry paths, provider text, or patch content.
// A patch containing the coordinator credential is withheld, not redacted,
// since a redacted patch would apply wrong content.
func SaveRecoveryPatch(worktree string, h RecoveryHeader, patch []byte) (string, error) {
	if err := h.validate(); err != nil {
		return "", err
	}
	if len(patch) == 0 || len(patch) > RecoveryPatchMaxBytes {
		return "", unavailableOversized
	}
	redaction, err := diagnosticsRedaction()
	if err != nil {
		return "", unavailableRedaction
	}
	if redaction.Redact(string(patch)) != string(patch) {
		return "", unavailableCredential
	}
	var data bytes.Buffer
	data.WriteString(recoveryMagic)
	fmt.Fprintf(&data, "# issue %d\n# pr %d\n# role %s\n# head %s\n", h.Issue, h.PR, h.Role, h.HeadSHA)
	for _, path := range h.Rejected {
		fmt.Fprintf(&data, "# rejected %s\n", path)
	}
	data.Write(patch)
	dirPath, dir, err := openStateDir(worktree, "recovery")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	lock, err := dir.OpenLock(".lock")
	if err != nil {
		return "", unavailableWrite
	}
	defer lock.Close()
	if err := filelock.LockExclusive(lock); err != nil {
		return "", unavailableWrite
	}
	defer filelock.Unlock(lock)
	name := recoveryNewName()
	if !recoveryFileName.MatchString(name) {
		return "", unavailableWrite
	}
	if err := dir.WriteFile(name, data.Bytes(), 0o600); err != nil {
		return "", unavailableWrite
	}
	pruneStateFiles(dir, recoveryFileName, name, diagnosticsNow())
	return filepath.Join(dirPath, name), nil
}

// LoadRecoveryPatch reads a saved patch by its file name and returns its
// header and diff body.
func LoadRecoveryPatch(name string) (RecoveryHeader, []byte, error) {
	if !recoveryFileName.MatchString(name) {
		return RecoveryHeader{}, nil, fmt.Errorf("invalid recovery patch name %q; expected rejected-<32 hex>.patch", name)
	}
	_, dir, err := openStateDir("", "recovery")
	if err != nil {
		return RecoveryHeader{}, nil, fmt.Errorf("recovery patch directory: %w", err)
	}
	defer dir.Close()
	data, err := dir.ReadFile(name, RecoveryPatchMaxBytes+recoveryHeaderBytes)
	if err != nil {
		return RecoveryHeader{}, nil, fmt.Errorf("read recovery patch %s: missing, pruned, or over the size limit", name)
	}
	return parseRecoveryPatch(data)
}

func parseRecoveryPatch(data []byte) (RecoveryHeader, []byte, error) {
	invalid := func() (RecoveryHeader, []byte, error) {
		return RecoveryHeader{}, nil, fmt.Errorf("invalid recovery patch")
	}
	text, ok := bytes.CutPrefix(data, []byte(recoveryMagic))
	if !ok {
		return invalid()
	}
	var h RecoveryHeader
	seen := map[string]bool{}
	for bytes.HasPrefix(text, []byte("# ")) {
		line, rest, found := bytes.Cut(text, []byte("\n"))
		if !found {
			return invalid()
		}
		text = rest
		key, value, _ := strings.Cut(string(line[2:]), " ")
		if key != "rejected" && seen[key] {
			return invalid()
		}
		seen[key] = true
		var err error
		switch key {
		case "issue":
			h.Issue, err = strconv.Atoi(value)
		case "pr":
			h.PR, err = strconv.Atoi(value)
		case "role":
			h.Role = value
		case "head":
			h.HeadSHA = value
		case "rejected":
			h.Rejected = append(h.Rejected, value)
		default:
			return invalid()
		}
		if err != nil {
			return invalid()
		}
	}
	if !seen["issue"] || !seen["pr"] || !seen["role"] || !seen["head"] || h.validate() != nil || len(text) > RecoveryPatchMaxBytes || !bytes.HasPrefix(text, []byte("diff --git ")) {
		return invalid()
	}
	return h, text, nil
}

const planRetryRecordLimit = 16 << 20 // two bounded plan files, JSON escaping, and metadata

var planRetryID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func openPlanRetryDir(worktree, repository string, issue int, create bool) (*safefs.Root, error) {
	if repository == "" || issue <= 0 {
		return nil, fmt.Errorf("invalid planner retry record scope")
	}
	name := fmt.Sprintf("plan-retries/%x/%d", sha256.Sum256([]byte(repository)), issue)
	var excluded []string
	if cache, err := os.UserCacheDir(); err == nil {
		excluded = append(excluded, filepath.Join(cache, "swarm", string(sandbox.AgentBuildCache)))
	}
	_, dir, err := openPrivateStateDir(worktree, name, create, excluded...)
	return dir, err
}

// SavePlanRetryRecovery saves coordinator evidence outside the planner checkout.
func SavePlanRetryRecovery(worktree, repository string, issue int, id string, data []byte) error {
	if !planRetryID.MatchString(id) || len(data) > planRetryRecordLimit || !json.Valid(data) {
		return fmt.Errorf("invalid planner retry record")
	}
	dir, err := openPlanRetryDir(worktree, repository, issue, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.WriteFileDurable(id, data, 0600); err != nil {
		return unavailableWrite
	}
	return nil
}

func LoadPlanRetryRecovery(worktree, repository string, issue int, id string) ([]byte, error) {
	if !planRetryID.MatchString(id) {
		return nil, fmt.Errorf("invalid planner retry ID")
	}
	dir, err := openPlanRetryDir(worktree, repository, issue, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("missing private planner retry evidence: %w", os.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	data, err := dir.ReadFile(id, planRetryRecordLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("missing private planner retry evidence: %w", os.ErrNotExist)
	}
	if err != nil || !json.Valid(data) {
		return nil, fmt.Errorf("invalid or unreadable private planner retry evidence")
	}
	return data, nil
}

// HasPlanRetryRecovery reports whether there may be local evidence to match
// against authoritative state. Only the authoritative retry IDs are loaded.
func HasPlanRetryRecovery(worktree, repository string, issue int) (bool, error) {
	dir, err := openPlanRetryDir(worktree, repository, issue, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	f, err := dir.OpenDirectory(".")
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) != 0, nil
}

func RemovePlanRetryRecovery(worktree, repository string, issue int, id string) error {
	if !planRetryID.MatchString(id) {
		return fmt.Errorf("invalid planner retry ID")
	}
	dir, err := openPlanRetryDir(worktree, repository, issue, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Remove(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not retire private planner retry evidence")
	}
	return nil
}
