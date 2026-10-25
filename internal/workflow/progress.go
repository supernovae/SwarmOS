package workflow

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/progress"
)

type ProgressEvent = progress.Event

// workProgress serializes callbacks from execution and the local status timer.
// It is independent of the authoritative lease-renewal heartbeat.
type workProgress struct {
	session               *progress.Session
	closed                bool
	shared                bool
	deadline              time.Time
	timer                 *time.Timer
	ctx                   context.Context
	phaseContext          context.Context
	now                   func() time.Time
	mu                    sync.Mutex
	emit                  func(ProgressEvent)
	issue                 int
	stage                 string
	started, stageStarted time.Time
	stop                  chan struct{}
	done                  <-chan struct{}
	stopTicker            func()
}

func startWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent)) *workProgress {
	return &workProgress{session: progress.Start(ctx, issue, 0, emit)}
}
func newWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent), ticks <-chan time.Time) *workProgress {
	p := progress.New(ctx, issue, 0, emit, ticks, time.Now, nil)
	return &workProgress{session: p, done: p.Done()}
}
func newSetupProgress(ctx context.Context, issue int, emit func(ProgressEvent), ticks <-chan time.Time) *workProgress {
	done := make(chan struct{})
	p := &workProgress{ctx: ctx, now: time.Now, emit: emit, issue: issue, started: time.Now(), stop: make(chan struct{}), done: done}
	if issue != 0 {
		p.next("preflight")
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				p.heartbeat(ctx)
			}
		}
	}()
	return p
}

func (p *workProgress) send(status string) {
	now := p.now()
	p.emit(ProgressEvent{IssueNumber: p.issue, Stage: p.stage, Status: status,
		Elapsed: now.Sub(p.started), StageElapsed: now.Sub(p.stageStarted)})
}

func (p *workProgress) next(stage string) {
	if p.session != nil {
		p.session.Next(stage)
		return
	}
	if p.emit == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx.Err() != nil {
		return
	}
	if p.stage != "" {
		p.send("completed")
	}
	p.stage, p.stageStarted = stage, p.now()
	p.send("started")
	p.resetDeadline()
}

func (p *workProgress) finish(result *WorkResult, err error) {
	if p.session != nil {
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
		return
	}
	if p.emit == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
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
	if p.stage != "" {
		p.send(status)
	}
	p.stage = ""
	p.mu.Unlock()
	if !p.shared {
		p.Close()
	}
}

// ProgressReporter shares serialized setup and execution status for one command.
type ProgressReporter = workProgress

// NewProgressReporter starts a command-scoped reporter. Close it before printing
// the command outcome. Execution reporting is enabled separately by OnProgress.
func NewProgressReporter(ctx context.Context, emit func(ProgressEvent)) *ProgressReporter {
	if emit == nil {
		return &workProgress{}
	}
	timer := time.NewTimer(15 * time.Second)
	p := newSetupProgress(ctx, 0, emit, timer.C)
	p.mu.Lock()
	p.stage = ""
	p.shared = true
	p.timer = timer
	p.mu.Unlock()
	return p
}

func (p *workProgress) resetDeadline() {
	if p.timer != nil && !p.closed {
		p.deadline = p.now().Add(15 * time.Second)
		p.timer.Reset(15 * time.Second)
	}
}

// Close joins the heartbeat goroutine and prevents further reporting.
func (p *workProgress) Close() {
	if p == nil || p.emit == nil {
		return
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.stop)
		if p.timer != nil {
			p.timer.Stop()
		}
		if p.stopTicker != nil {
			p.stopTicker()
		}
	}
	p.mu.Unlock()
	<-p.done
}

// ReportSetupPhase announces an operation before executing it synchronously.
// Reporting never changes the operation's error or claim authority.
func (p *workProgress) ReportSetupPhase(ctx context.Context, issue int, phase string, operation func() error) error {
	if p == nil || p.emit == nil {
		return operation()
	}
	p.mu.Lock()
	if p.closed || ctx.Err() != nil || p.ctx.Err() != nil {
		p.mu.Unlock()
		return operation()
	}
	stage, origin, previousIssue := p.stage, p.stageStarted, p.issue
	previousContext := p.phaseContext
	p.phaseContext = ctx
	p.stage, p.stageStarted, p.issue = phase, p.now(), issue
	p.send("started")
	p.resetDeadline()
	p.mu.Unlock()
	err := operation()
	p.mu.Lock()
	p.stage, p.stageStarted, p.issue = stage, origin, previousIssue
	p.phaseContext = previousContext
	p.resetDeadline()
	p.mu.Unlock()
	return err
}

func (p *workProgress) heartbeat(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() == nil && (p.phaseContext == nil || p.phaseContext.Err() == nil) && !p.closed && p.stage != "" && (p.deadline.IsZero() || !p.now().Before(p.deadline)) {
		p.send("running")
		if p.timer != nil {
			p.timer.Reset(15 * time.Second)
		}
	}
}
