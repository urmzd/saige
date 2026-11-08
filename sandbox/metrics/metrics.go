// Package metrics defines the recording interface every sandbox component
// emits through. It depends on nothing else in the module: components take a
// Recorder and stay ignorant of transport, so Prometheus, OpenTelemetry, or
// an in-memory store can back it without any caller changing.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Label is one metric dimension.
type Label struct {
	Key   string
	Value string
}

// Recorder receives metric events. Implementations must be safe for
// concurrent use.
type Recorder interface {
	// Count adds delta to the counter identified by name and labels.
	Count(name string, delta float64, labels ...Label)
	// Gauge sets the gauge identified by name and labels to value.
	Gauge(name string, value float64, labels ...Label)
	// Observe records one sample (e.g. a duration in seconds) into the
	// distribution identified by name and labels.
	Observe(name string, value float64, labels ...Label)
}

// Nop returns a Recorder that discards everything. Use it when metrics are
// not wired up; callers never need nil checks.
func Nop() Recorder { return nopRecorder{} }

type nopRecorder struct{}

func (nopRecorder) Count(string, float64, ...Label)   {}
func (nopRecorder) Gauge(string, float64, ...Label)   {}
func (nopRecorder) Observe(string, float64, ...Label) {}

// Memory is an in-process Recorder for development and tests.
// Distributions are kept as count and sum.
type Memory struct {
	mu     sync.Mutex
	values map[string]float64
}

// NewMemory returns an empty in-memory recorder.
func NewMemory() *Memory {
	return &Memory{values: make(map[string]float64)}
}

func series(name string, labels []Label) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = fmt.Sprintf("%s=%q", l.Key, l.Value)
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Count implements Recorder.
func (m *Memory) Count(name string, delta float64, labels ...Label) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[series(name, labels)] += delta
}

// Gauge implements Recorder.
func (m *Memory) Gauge(name string, value float64, labels ...Label) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[series(name, labels)] = value
}

// Observe implements Recorder.
func (m *Memory) Observe(name string, value float64, labels ...Label) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[series(name+"_count", labels)]++
	m.values[series(name+"_sum", labels)] += value
}

// Snapshot returns a copy of every series and its current value, keyed in
// Prometheus text style (name{label="value"}).
func (m *Memory) Snapshot() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.values))
	for k, v := range m.values {
		out[k] = v
	}
	return out
}
