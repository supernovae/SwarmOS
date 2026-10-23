package workflow

import (
	"context"
	"errors"
	"sync"
	"time"
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
	mu                    sync.Mutex
	emit                  func(ProgressEvent)
	issue                 int
	stage                 string
	started, stageStarted time.Time
	stop                  chan struct{}
	done                  chan struct{}
	stopTicker            func()
}

func startWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent)) *workProgress {
	if emit == nil {
		return &workProgress{}
	}
	ticker := time.NewTicker(15 * time.Second)
	p := newWorkProgress(ctx, issue, emit, ticker.C)
	p.stopTicker = ticker.Stop
	return p
}

func newWorkProgress(ctx context.Context, issue int, emit func(ProgressEvent), ticks <-chan time.Time) *workProgress {
	p := &workProgress{emit: emit, issue: issue, started: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	p.next("preflight")
	go func() {
		defer close(p.done)
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
				p.mu.Lock()
				// Do not announce running work after cancellation.
				if ctx.Err() == nil {
					p.send("running")
				}
				p.mu.Unlock()
			}
		}
	}()
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stage != "" {
		p.send("completed")
	}
	p.stage, p.stageStarted = stage, time.Now()
	p.send("started")
}

func (p *workProgress) finish(result *WorkResult, err error) {
	if p.emit == nil {
		return
	}
	close(p.stop)
	<-p.done // No timer callback may outlive ExecuteWork or its terminal event.
	if p.stopTicker != nil {
		p.stopTicker()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
}
