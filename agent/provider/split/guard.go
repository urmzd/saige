package split

import "sync"

// guardState is the shared outcome history of one canary arm.
type guardState struct {
	g Guard

	mu       sync.Mutex
	outcomes []bool // true is a failure; a ring of at most Window entries
	next     int
	failures int
	isDown   bool
}

func newGuardState(g Guard) *guardState {
	if g.MinSamples <= 0 {
		g.MinSamples = 10
	}
	if g.Window <= 0 {
		g.Window = 100
	}
	g.MinSamples = min(g.MinSamples, g.Window)
	return &guardState{g: g}
}

// record adds one attempt and demotes the arm when its rate passes the limit.
func (s *guardState) record(failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outcomes) < s.g.Window {
		s.outcomes = append(s.outcomes, failed)
	} else {
		if s.outcomes[s.next] {
			s.failures--
		}
		s.outcomes[s.next] = failed
		s.next = (s.next + 1) % s.g.Window
	}
	if failed {
		s.failures++
	}
	if len(s.outcomes) >= s.g.MinSamples && float64(s.failures)/float64(len(s.outcomes)) > s.g.MaxErrorRate {
		s.isDown = true
	}
}

func (s *guardState) demoted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isDown
}

func (s *guardState) status() (rate float64, samples int, demoted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outcomes) > 0 {
		rate = float64(s.failures) / float64(len(s.outcomes))
	}
	return rate, len(s.outcomes), s.isDown
}

func (s *guardState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes, s.next, s.failures, s.isDown = nil, 0, 0, false
}
