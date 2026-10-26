// Package progress reports coordinator-owned operation metadata.
package progress

import (
	"context"
	"sync"
	"time"
)

// Event contains coordinator-owned status only, never harness output,
// prompts, command arguments, claim tokens, or artifact contents.
type Event struct {
	IssueNumber  int
	PRNumber     int
	Check        string
	Stage        string
	Status       string // started, completed, running, paused, handed-off, failed, canceled, PASS, FAIL, skipped
	Elapsed      time.Duration
	StageElapsed time.Duration
}

// Session serializes observers independently of claim renewal. Observers must
// return promptly and must not perform coordinator operations.
type Session struct {
	stopTicker      func()
	delivery        sync.Mutex
	mu              sync.Mutex
	emit            func(Event)
	event           Event
	started, active time.Time
	phaseStarted    time.Time
	now             func() time.Time
	reset           func()
	stop            chan struct{}
	done            chan struct{}
	once            sync.Once
	closed          bool
}

func Start(ctx context.Context, issue, pr int, emit func(Event)) *Session {
	if emit == nil {
		return &Session{}
	}
	ticker := time.NewTicker(15 * time.Second)
	p := New(ctx, issue, pr, emit, ticker.C, time.Now, func() { ticker.Reset(15 * time.Second) })
	p.stopTicker = ticker.Stop
	return p
}

// New accepts injected ticks and a clock for deterministic tests.
func New(ctx context.Context, issue, pr int, emit func(Event), ticks <-chan time.Time, now func() time.Time, reset func()) *Session {
	if emit == nil {
		return &Session{}
	}
	p := &Session{emit: emit, event: Event{IssueNumber: issue, PRNumber: pr}, now: now, reset: reset, started: now(), stop: make(chan struct{}), done: make(chan struct{})}
	p.Next("preflight")
	go func() {
		defer close(p.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case tick, ok := <-ticks:
				if !ok {
					return
				}
				p.delivery.Lock()
				// A tick selected before a check transition must not become
				// an immediate heartbeat for the newly started check.
				p.mu.Lock()
				due := p.event.Check == "" || !tick.Before(p.active.Add(15*time.Second))
				p.mu.Unlock()
				if ctx.Err() == nil && due {
					p.send("running")
				}
				p.delivery.Unlock()
			}
		}
	}()
	return p
}

// send is called with delivery held; state is released before observer delivery.
func (p *Session) send(status string) {
	p.mu.Lock()
	e := p.event
	e.Status = status
	e.Elapsed = p.now().Sub(p.started)
	e.StageElapsed = p.now().Sub(p.active)
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		p.emit(e)
	}
}
func (p *Session) Next(stage string) {
	if p == nil || p.emit == nil {
		return
	}
	p.delivery.Lock()
	defer p.delivery.Unlock()
	if p.closed {
		return
	}
	if p.event.Stage != "" {
		p.send("completed")
	}
	p.mu.Lock()
	p.event.Stage = stage
	p.event.Check = ""
	p.active = p.now()
	p.phaseStarted = p.active
	p.mu.Unlock()
	if p.reset != nil {
		p.reset()
	}
	p.send("started")
}

// Observe adapts validator check lifecycle events to this operation's timer.
func (p *Session) Observe(e Event) {
	if p == nil || p.emit == nil {
		return
	}
	p.delivery.Lock()
	defer p.delivery.Unlock()
	if p.closed {
		return
	}
	if e.Status == "started" {
		p.mu.Lock()
		p.event.Check = e.Check
		p.active = p.now()
		p.mu.Unlock()
		if p.reset != nil {
			p.reset()
		}
		p.send("started")
	} else {
		p.send(e.Status)
		p.mu.Lock()
		p.event.Check = ""
		p.active = p.phaseStarted
		p.mu.Unlock()
		if p.reset != nil {
			p.reset()
		}
	}
}

// Snapshot may be inspected by an observer without acquiring its delivery lock.
func (p *Session) Snapshot() Event { p.mu.Lock(); defer p.mu.Unlock(); return p.event }
func (p *Session) Finish(status string) {
	if p == nil || p.emit == nil {
		return
	}
	p.once.Do(func() {
		close(p.stop)
		<-p.done
		if p.stopTicker != nil {
			p.stopTicker()
		}
		p.delivery.Lock()
		defer p.delivery.Unlock()
		p.send(status)
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	})
}

func (p *Session) Done() <-chan struct{} { return p.done }

const Interval = 15 * time.Second

type Reporter struct {
	mu         sync.Mutex
	stop, Done chan struct{}
	once       sync.Once
	stopTicker func()
	now        func() time.Time
	started    time.Time
	heartbeat  func(time.Duration)
}

// NewReporter starts a reporter. Nil heartbeat disables reporting. Nil ticks uses the
// production ticker; callers may inject ticks and a clock for deterministic tests.
func NewReporter(ctx context.Context, heartbeat func(time.Duration), ticks <-chan time.Time, now func() time.Time) *Reporter {
	p := &Reporter{heartbeat: heartbeat}
	if heartbeat == nil {
		return p
	}
	if now == nil {
		now = time.Now
	}
	p.now, p.started = now, now()
	p.stop, p.Done = make(chan struct{}), make(chan struct{})
	if ticks == nil {
		ticker := time.NewTicker(Interval)
		ticks, p.stopTicker = ticker.C, ticker.Stop
	}
	go func() {
		defer close(p.Done)
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
				p.Do(func(elapsed time.Duration) {
					if ctx.Err() == nil {
						heartbeat(elapsed)
					}
				})
			}
		}
	}()
	return p
}

// Do serializes an event with heartbeat callbacks.
func (p *Reporter) Do(emit func(time.Duration)) {
	if p.heartbeat == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	emit(p.now().Sub(p.started))
}

// Stop joins the worker before the caller emits a terminal event.
func (p *Reporter) Stop() {
	if p.heartbeat == nil {
		return
	}
	p.once.Do(func() {
		close(p.stop)
		<-p.Done
		if p.stopTicker != nil {
			p.stopTicker()
		}
	})
}
