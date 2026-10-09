package otel

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// spyRecorder collects every span a spyTracer starts.
type spyRecorder struct {
	mu    sync.Mutex
	spans []*spySpan
}

func (r *spyRecorder) all() []*spySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*spySpan(nil), r.spans...)
}

// named returns the spans whose name is name, in start order.
func (r *spyRecorder) named(name string) []*spySpan {
	var out []*spySpan
	for _, s := range r.all() {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// spyTracer is a trace.Tracer that records spans and the parent each one was
// started under, without an SDK dependency.
type spyTracer struct {
	noop.Tracer
	rec *spyRecorder
}

func newSpyTracer() (spyTracer, *spyRecorder) {
	rec := &spyRecorder{}
	return spyTracer{rec: rec}, rec
}

func (t spyTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	s := &spySpan{name: name, kind: cfg.SpanKind(), attrs: map[attribute.Key]attribute.Value{}}
	if parent, ok := trace.SpanFromContext(ctx).(*spySpan); ok {
		s.parent = parent
	}
	for _, kv := range cfg.Attributes() {
		s.attrs[kv.Key] = kv.Value
	}
	t.rec.mu.Lock()
	t.rec.spans = append(t.rec.spans, s)
	t.rec.mu.Unlock()
	return trace.ContextWithSpan(ctx, s), s
}

// spySpan records what the code under test does to a span.
type spySpan struct {
	noop.Span
	mu         sync.Mutex
	name       string
	kind       trace.SpanKind
	parent     *spySpan
	attrs      map[attribute.Key]attribute.Value
	ended      int
	status     codes.Code
	statusDesc string
	errs       []error
	events     []spyEvent
}

func (s *spySpan) End(...trace.SpanEndOption) {
	s.mu.Lock()
	s.ended++
	s.mu.Unlock()
}

func (s *spySpan) SetStatus(code codes.Code, desc string) {
	s.mu.Lock()
	s.status, s.statusDesc = code, desc
	s.mu.Unlock()
}

func (s *spySpan) RecordError(err error, _ ...trace.EventOption) {
	s.mu.Lock()
	s.errs = append(s.errs, err)
	s.mu.Unlock()
}

func (s *spySpan) SetAttributes(kv ...attribute.KeyValue) {
	s.mu.Lock()
	for _, a := range kv {
		s.attrs[a.Key] = a.Value
	}
	s.mu.Unlock()
}

func (s *spySpan) IsRecording() bool { return true }

func (s *spySpan) endCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// attr returns the value set for key and whether it was set.
func (s *spySpan) attr(key string) (attribute.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.attrs[attribute.Key(key)]
	return v, ok
}

func (s *spySpan) str(key string) string {
	v, _ := s.attr(key)
	return v.AsString()
}

func (s *spySpan) int(key string) int64 {
	v, _ := s.attr(key)
	return v.AsInt64()
}

// spyEvent is one span event.
type spyEvent struct {
	name  string
	attrs map[attribute.Key]attribute.Value
}

func (s *spySpan) AddEvent(name string, opts ...trace.EventOption) {
	cfg := trace.NewEventConfig(opts...)
	e := spyEvent{name: name, attrs: map[attribute.Key]attribute.Value{}}
	for _, kv := range cfg.Attributes() {
		e.attrs[kv.Key] = kv.Value
	}
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}
