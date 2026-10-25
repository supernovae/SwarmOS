package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

// ProgressEvent contains coordinator-owned status only, never harness output,
// prompts, command arguments, claim tokens, or artifact contents.
type ProgressEvent struct {
	IssueNumber  int
	Stage        string
	Status       string // started, completed, running, paused, handed-off, failed, canceled
	Elapsed      time.Duration
	StageElapsed time.Duration
}

// workProgress serializes callbacks from execution and the local status timer.
// It is independent of the authoritative lease-renewal heartbeat.
type workProgress struct {
	emit                  func(ProgressEvent)
	issue                 int
	stage                 string
	started, stageStarted time.Time
	reporter              *progress.Reporter
	done                  chan struct{}
}

func startWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent)) *workProgress {
	return newWorkProgress(ctx, issue, emit, nil)
}

func newWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent), ticks <-chan time.Time) *workProgress {
	p := &workProgress{emit: emit, issue: issue, started: time.Now()}
	if emit == nil {
		return p
	}
	// Emit the initial event before starting the timer.
	p.stage, p.stageStarted = "preflight", time.Now()
	p.send("started")
	p.reporter = progress.New(ctx, func(time.Duration) { p.send("running") }, ticks, nil)
	p.done = p.reporter.Done
	return p
}

func (p *workProgress) send(status string) {
	now := time.Now()
	p.emit(ProgressEvent{IssueNumber: p.issue, Stage: p.stage, Status: status,
		Elapsed: now.Sub(p.started), StageElapsed: now.Sub(p.stageStarted)})
}

func (p *workProgress) next(stage string) {
	if p.emit == nil {
		return
	}
	p.reporter.Do(func(time.Duration) {
		if p.stage != "" {
			p.send("completed")
		}
		p.stage, p.stageStarted = stage, time.Now()
		p.send("started")
	})
}

func (p *workProgress) finish(result *WorkResult, err error) {
	if p.emit == nil {
		return
	}
	p.reporter.Stop()
	p.reporter.Do(func(time.Duration) {
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
		p.send(status)
	})
}
