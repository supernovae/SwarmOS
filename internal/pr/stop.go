package pr

// ConflictError marks a PR stop that only a rebase onto its approved base can
// clear. Paused means a managed rebase is already stopped in the PR worktree;
// Manual means the conflict resolver could not finish it and a person must.
type ConflictError struct {
	PRNumber int
	Paused   bool
	Manual   bool
	Err      error
}

func (e *ConflictError) Error() string { return e.Err.Error() }
func (e *ConflictError) Unwrap() error { return e.Err }
