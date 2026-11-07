package workflow

import (
	"fmt"
)

// recoverCheckpointCompletion runs before workspace provisioning or bundle
// compilation, so a rejected receipt cannot overwrite preserved agent outputs.
func (r *workRun) recoverCheckpointCompletion() error {
	if r.forkMode || r.opts.PRNumber != 0 || r.e.LeaseManager.HasWorkerGateway() {
		return nil
	}
	c, err := r.e.LeaseManager.InspectCheckpointCompletion(r.ctx, r.opts.IssueNumber)
	if err != nil {
		return fmt.Errorf("pending implementation preserved; inspect completion recovery: %w", err)
	}
	if c == nil {
		return nil
	}
	if r.opts.Phase != "all" && r.opts.Phase != "implement" && r.opts.Phase != "test" {
		return fmt.Errorf("pending implementation preserved; resume with --phase implement before changing plans")
	}
	if r.opts.instructionsFor("developer") != "" {
		return fmt.Errorf("pending implementation preserved; resume without developer instructions to recover completion first")
	}
	st, err := r.e.LeaseManager.RecoverCheckpointCompletion(r.ctx, r.opts.IssueNumber, r.opts.Owner)
	if err != nil {
		return fmt.Errorf("pending implementation preserved; completion recovery rejected: %w", err)
	}
	r.st = st
	r.testerResume = true
	return nil
}
