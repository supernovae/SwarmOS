// Package progress provides serialized local status callbacks and joined shutdown.
package progress

import (
	"context"
	"sync"
	"time"
)

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

// New starts a reporter. Nil heartbeat disables reporting. Nil ticks uses the
// production ticker; callers may inject ticks and a clock for deterministic tests.
func New(ctx context.Context, heartbeat func(time.Duration), ticks <-chan time.Time, now func() time.Time) *Reporter {
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
