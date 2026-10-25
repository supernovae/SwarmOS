package pr

import (
	"errors"
	"fmt"
	"os"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
)

// restoreApprovedPlan materializes the authenticated plan in a fresh PR
// worktree. Existing local edits must be reviewed rather than overwritten.
func restoreApprovedPlan(root string, st *lease.IssueState) error {
	safe, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer safe.Close()
	active, err := safe.Sub(".swarm/active", true, 0755)
	if err != nil {
		return err
	}
	defer active.Close()
	for _, a := range st.Artifacts {
		name := ""
		switch a.Type {
		case contract.ArtifactDesign:
			name = "PLAN.md"
		case contract.ArtifactTestPlan:
			name = "TEST_PLAN.md"
		}
		if name == "" {
			continue
		}
		old, err := active.ReadFile(name, 8<<20)
		if err == nil {
			if string(old) != a.Body && string(old) != contract.CanonicalArtifactBody(a.Body) {
				return fmt.Errorf("%s differs from the approved plan; run pr work to review the change", name)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := active.WriteFile(name, []byte(contract.CanonicalArtifactBody(a.Body)), 0644); err != nil {
			return err
		}
	}
	return nil
}
