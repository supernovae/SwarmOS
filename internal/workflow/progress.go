package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

type ProgressEvent = progress.Event
type workProgress struct {
	session *progress.Session
	done    <-chan struct{}
}

func startWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent)) *workProgress {
	return &workProgress{session: progress.Start(ctx, issue, 0, emit)}
}
func newWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent), ticks <-chan time.Time) *workProgress {
	p := progress.New(ctx, issue, 0, emit, ticks, time.Now, nil)
	return &workProgress{session: p, done: p.Done()}
}
func (p *workProgress) next(stage string) { p.session.Next(stage) }
func (p *workProgress) finish(result *WorkResult, err error) {
	status := "completed"
	if err != nil {
		status = "failed"
		if errors.Is(err, context.Canceled) {
			status = "canceled"
		}
	} else if result != nil {
		if result.PausedForHuman {
			status = "paused"
		} else if result.HandedOffToPhase != "" {
			status = "handed-off"
		}
	}
	p.session.Finish(status)
}
