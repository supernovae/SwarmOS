package sandbox

import "time"

// Lifecycle receives coordinator-measured phase durations for one role run.
// A nil duration was not measured by this execution path. It never changes
// how the run executes, what it imports or whether it succeeds.
type Lifecycle struct {
	Preflight, Setup, Snapshot *time.Duration
	// ObserveScratch runs after the harness exits and before the run's scratch
	// HOME is removed. It must only read; its outcome never affects the run.
	ObserveScratch func(scratchHome string)
	// ObserveSession runs after a host-HOME harness exits, before its control
	// directory is removed. It must only read the session named here; its
	// outcome never affects the run.
	ObserveSession func(HarnessSession)
}

// HarnessSession names one run's own session in the harness's own records.
// Home and CodexHome are where the harness keeps them; Control is the run's
// unique working directory. Records carries a session export fetched from the
// harness itself, when the harness keeps sessions outside plain files.
type HarnessSession struct {
	Driver, Home, CodexHome, Control string
	SessionIDs                       []string
	Records                          []byte
}

// observeSession never lets an observer failure reach the run.
func (l *Lifecycle) observeSession(session HarnessSession) {
	if l == nil || l.ObserveSession == nil {
		return
	}
	defer func() { _ = recover() }()
	l.ObserveSession(session)
}

func (l *Lifecycle) addPreflight(start time.Time) {
	if l != nil {
		l.Preflight = addDuration(l.Preflight, time.Since(start))
	}
}

func (l *Lifecycle) addSetup(start time.Time) {
	if l != nil {
		l.Setup = addDuration(l.Setup, time.Since(start))
	}
}

func (l *Lifecycle) addSnapshot(start time.Time) {
	if l != nil {
		l.Snapshot = addDuration(l.Snapshot, time.Since(start))
	}
}

func (l *Lifecycle) observe(scratch string) {
	if l != nil && l.ObserveScratch != nil {
		l.ObserveScratch(scratch)
	}
}

func addDuration(total *time.Duration, d time.Duration) *time.Duration {
	if total != nil {
		d += *total
	}
	return &d
}
