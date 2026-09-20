package router

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

type provider struct {
	model string
	call  func() (<-chan types.Delta, error)
	caps  types.ModelCapabilities
}

func (p provider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	return p.call()
}
func (p provider) Model() string                         { return p.model }
func (p provider) Capabilities() types.ModelCapabilities { return p.caps }
func deltas(ds ...types.Delta) <-chan types.Delta {
	ch := make(chan types.Delta, len(ds))
	for _, d := range ds {
		ch <- d
	}
	close(ch)
	return ch
}
func transient() error {
	return &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("busy")}
}
func consume(t *testing.T, s *Session) (string, error) {
	t.Helper()
	return types.GenerateText(context.Background(), s, "hello")
}

func TestStickyIndependentProfilesAndSessions(t *testing.T) {
	var primary, secondary atomic.Int32
	r, err := New(Config{Profiles: []Profile{
		{ID: "first", Provider: provider{model: "same", call: func() (<-chan types.Delta, error) { primary.Add(1); return nil, transient() }}},
		{ID: "second", Provider: provider{model: "same", call: func() (<-chan types.Delta, error) {
			secondary.Add(1)
			return deltas(types.TextContentDelta{Content: "second settings"}), nil
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()
	for range 2 {
		got, err := consume(t, s)
		if err != nil || got != "second settings" {
			t.Fatal(got, err)
		}
	}
	if primary.Load() != 1 || secondary.Load() != 2 || s.Model() != "second" {
		t.Fatal("route was not sticky")
	}
	consume(t, r.Session())
	if primary.Load() != 2 {
		t.Fatal("sessions share affinity")
	}
}

func TestPartialOutputDoesNotRetryButNextCallMoves(t *testing.T) {
	var secondary atomic.Int32
	r, _ := New(Config{Profiles: []Profile{
		{ID: "first", Provider: provider{call: func() (<-chan types.Delta, error) {
			return deltas(types.TextContentDelta{Content: "partial"}, types.ErrorDelta{Error: transient()}), nil
		}}},
		{ID: "second", Provider: provider{call: func() (<-chan types.Delta, error) {
			secondary.Add(1)
			return deltas(types.TextContentDelta{Content: "done"}), nil
		}}},
	}})
	s := r.Session()
	_, err := consume(t, s)
	if err == nil || secondary.Load() != 0 {
		t.Fatal("partial output was retried")
	}
	got, err := consume(t, s)
	if err != nil || got != "done" {
		t.Fatal(got, err)
	}
}

func TestSessionBusyAndCapabilityGate(t *testing.T) {
	release := make(chan types.Delta)
	r, _ := New(Config{Profiles: []Profile{{ID: "a", Provider: provider{call: func() (<-chan types.Delta, error) { return release, nil }}}}})
	s := r.Session()
	ch, err := s.ChatStream(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ChatStream(context.Background(), nil, nil); !errors.Is(err, ErrSessionBusy) {
		t.Fatal(err)
	}
	close(release)
	for range ch {
	}
	if _, err = s.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"}); err == nil {
		t.Fatal("schema silently dropped")
	}
	if _, err = s.WithModel("missing").ChatStream(context.Background(), nil, nil); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestEarlyStreamFailureAndPolicyValidation(t *testing.T) {
	r, _ := New(Config{Profiles: []Profile{
		{ID: "a", Provider: provider{call: func() (<-chan types.Delta, error) { return deltas(types.ErrorDelta{Error: transient()}), nil }}},
		{ID: "b", Provider: provider{call: func() (<-chan types.Delta, error) { return deltas(types.TextContentDelta{Content: "ok"}), nil }}},
	}})
	if got, err := consume(t, r.Session()); err != nil || got != "ok" {
		t.Fatal(got, err)
	}
	r.cfg.Policy = PolicyFunc(func(context.Context, Request) ([]string, error) { return []string{"a", "a"}, nil })
	if _, err := consume(t, r.Session()); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
