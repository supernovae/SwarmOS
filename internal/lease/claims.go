package lease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/filelock"
)

type persistedClaim struct {
	IssueNumber int    `json:"issue_number"`
	ClaimID     string `json:"claim_id"`
}

// Control files live outside agent worktrees and are shared by this clone's
// linked worktrees. Independent clones still arbitrate through the remote CAS.
func claimControlPath(repoRoot string, issueNumber int) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("resolve claim repository identity: %w", err)
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return "", err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(common))
	return filepath.Join(configDir, "swarm", "claims", hex.EncodeToString(digest[:]), fmt.Sprintf("%d.json", issueNumber)), nil
}

func SaveClaimToken(repoRoot string, issueNumber int, claimID string) error {
	if claimID == "" {
		return fmt.Errorf("claim token is empty")
	}
	path, err := claimControlPath(repoRoot, issueNumber)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.Marshal(persistedClaim{IssueNumber: issueNumber, ClaimID: claimID})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".claim-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func LoadClaimToken(repoRoot string, issueNumber int) (string, error) {
	path, err := claimControlPath(repoRoot, issueNumber)
	if err != nil {
		return "", err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var token persistedClaim
	if err := json.Unmarshal(payload, &token); err != nil {
		return "", err
	}
	if token.IssueNumber != issueNumber || token.ClaimID == "" {
		return "", fmt.Errorf("invalid persisted claim token for issue #%d", issueNumber)
	}
	return token.ClaimID, nil
}

func LockClaim(repoRoot string, issueNumber int) (*os.File, error) {
	path, err := claimControlPath(repoRoot, issueNumber)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryLockExclusive(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("issue #%d is already executing on this machine: %w", issueNumber, err)
	}
	return file, nil
}

func UnlockClaim(file *os.File) {
	if file != nil {
		_ = filelock.Unlock(file)
		_ = file.Close()
	}
}
