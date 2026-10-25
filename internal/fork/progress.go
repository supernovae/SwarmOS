package fork

import (
	"context"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

// ProgressEvent includes only local coordinator request status.
type ProgressEvent struct {
	Operation string
	RequestID int64
	Status    string
	Elapsed   time.Duration
}

type requestProgress struct {
	reporter  *progress.Reporter
	emit      func(ProgressEvent)
	operation string
	id        int64
}

func startRequestProgress(ctx context.Context, operation string, id int64, emit func(ProgressEvent), ticks <-chan time.Time, now func() time.Time) *requestProgress {
	p := &requestProgress{emit: emit, operation: operation, id: id}
	if emit == nil {
		return p
	}
	// Announce synchronously before dispatcher execution and the first poll.
	emit(ProgressEvent{Operation: operation, RequestID: id, Status: "posted"})
	p.reporter = progress.New(ctx, func(elapsed time.Duration) { p.send("waiting", elapsed) }, ticks, now)
	return p
}
func (p *requestProgress) send(status string, elapsed time.Duration) {
	p.emit(ProgressEvent{Operation: p.operation, RequestID: p.id, Status: status, Elapsed: elapsed})
}
func (p *requestProgress) stop() {
	if p.reporter != nil {
		p.reporter.Stop()
	}
}
func (p *requestProgress) finish(status string) {
	if p.reporter == nil {
		return
	}
	p.stop()
	p.reporter.Do(func(elapsed time.Duration) { p.send(status, elapsed) })
}
