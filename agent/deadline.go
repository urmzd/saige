package agent

import (
	"sync"
	"time"
)

// pausableDeadline fires once after a total running time, not counting the
// intervals it spends paused. A delegation uses it so a child's time limit
// covers the child's work but not a human deciding on an approval. All
// methods are safe on a nil receiver, which means no deadline.
type pausableDeadline struct {
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
	fire      func()
	paused    int // nested pauses; the clock runs only at zero
	stopped   bool
}

// newPausableDeadline starts a running deadline of d that calls fire on expiry.
func newPausableDeadline(d time.Duration, fire func()) *pausableDeadline {
	p := &pausableDeadline{remaining: d, fire: fire}
	p.start()
	return p
}

func (p *pausableDeadline) start() {
	p.started = time.Now()
	p.timer = time.AfterFunc(p.remaining, p.fire)
}

// pause stops the clock. Pauses nest, so overlapping waits keep it stopped
// until the last one resumes.
func (p *pausableDeadline) pause() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused++
	if p.paused > 1 || p.stopped || p.timer == nil {
		return
	}
	if p.timer.Stop() {
		p.remaining -= time.Since(p.started)
	} else {
		p.remaining = 0 // already fired
	}
	p.timer = nil
}

// resume restarts the clock with the time left when it was paused.
func (p *pausableDeadline) resume() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused > 0 {
		p.paused--
	}
	if p.paused > 0 || p.stopped || p.timer != nil || p.remaining <= 0 {
		return
	}
	p.start()
}

// stop releases the timer without firing.
func (p *pausableDeadline) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}
