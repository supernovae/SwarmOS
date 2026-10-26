package harness

import (
	"context"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

// DoctorRunner surrounds one synchronous diagnostic without changing its result.
// A nil runner executes the diagnostic silently.
type DoctorRunner func(category, name string, check func() DoctorCheck) DoctorCheck

func (run DoctorRunner) Run(category, name string, check func() DoctorCheck) DoctorCheck {
	if run == nil {
		return check()
	}
	return run(category, name, check)
}

// NewDoctorRunner reports each check using its own clock and heartbeat lifetime.
// Nil ticksFactory and now select the production timer and clock.
func NewDoctorRunner(ctx context.Context, observe func(progress.Event), ticksFactory func() <-chan time.Time, now func() time.Time) DoctorRunner {
	if observe == nil {
		return nil
	}
	return func(category, name string, check func() DoctorCheck) DoctorCheck {
		var ticks <-chan time.Time
		if ticksFactory != nil {
			ticks = ticksFactory()
		}
		emit := func(status string, elapsed time.Duration) {
			observe(progress.Event{Check: name, Status: status, StageElapsed: elapsed})
		}
		// Start the clock before announcing the check so observers that react to
		// "started" never race the reporter's start time.
		reporter := progress.NewReporter(ctx, func(elapsed time.Duration) { emit("running", elapsed) }, ticks, now)
		defer reporter.Stop()
		emit("started", 0)
		result := check()
		reporter.Stop()
		reporter.Do(func(elapsed time.Duration) { emit(result.Status, elapsed) })
		return result
	}
}
