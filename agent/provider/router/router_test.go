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

func (p provider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
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
			return deltas(types.PartDelta{Index: 0, Text: "second settings"}), nil
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
			return deltas(types.PartDelta{Index: 0, Text: "partial"}, types.ErrorDelta{Error: transient()}), nil
		}}},
		{ID: "second", Provider: provider{call: func() (<-chan types.Delta, error) {
			secondary.Add(1)
			return deltas(types.PartDelta{Index: 0, Text: "done"}), nil
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
	ch, err := s.Stream(context.Background(), types.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Stream(context.Background(), types.Request{}); !errors.Is(err, ErrSessionBusy) {
		t.Fatal(err)
	}
	close(release)
	for range ch {
	}
	if _, err = s.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}}); err == nil {
		t.Fatal("schema silently dropped")
	}
	if _, err = s.WithModel("missing").Stream(context.Background(), types.Request{}); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestEarlyStreamFailureAndPolicyValidation(t *testing.T) {
	r, _ := New(Config{Profiles: []Profile{
		{ID: "a", Provider: provider{call: func() (<-chan types.Delta, error) { return deltas(types.ErrorDelta{Error: transient()}), nil }}},
		{ID: "b", Provider: provider{call: func() (<-chan types.Delta, error) { return deltas(types.PartDelta{Index: 0, Text: "ok"}), nil }}},
	}})
	if got, err := consume(t, r.Session()); err != nil || got != "ok" {
		t.Fatal(got, err)
	}
	r.cfg.Policy = PolicyFunc(func(context.Context, Request) ([]string, error) { return []string{"a", "a"}, nil })
	if _, err := consume(t, r.Session()); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

// TestUsageBeforeOutputDoesNotCommit checks that usage reported before any
// output (as Anthropic does at message start) neither blocks failover nor
// reaches the caller from the failed profile, and is still forwarded when the
// profile serves or when the request ends with its error.
func TestUsageBeforeOutputDoesNotCommit(t *testing.T) {
	unavailable := &types.ProviderError{Kind: types.ErrorKindUnavailable, Err: errors.New("overloaded")}
	permanent := &types.ProviderError{Kind: types.ErrorKindPermanent, Err: errors.New("bad key")}
	tests := []struct {
		name       string
		a          []types.Delta
		wantText   string
		wantErr    bool
		wantBCalls int32
		wantPrompt int
	}{
		{
			name:       "usage then transient error fails over",
			a:          []types.Delta{types.UsageDelta{PromptTokens: 10}, types.ErrorDelta{Error: unavailable}},
			wantText:   "from b",
			wantBCalls: 1,
			wantPrompt: 3,
		},
		{
			name:       "usage then output is served",
			a:          []types.Delta{types.UsageDelta{PromptTokens: 10}, types.PartDelta{Index: 0, Text: "from a"}},
			wantText:   "from a",
			wantPrompt: 10,
		},
		{
			name:       "usage then permanent error reports usage",
			a:          []types.Delta{types.UsageDelta{PromptTokens: 10}, types.ErrorDelta{Error: permanent}},
			wantErr:    true,
			wantPrompt: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bCalls atomic.Int32
			r, err := New(Config{Profiles: []Profile{
				{ID: "a", Provider: provider{call: func() (<-chan types.Delta, error) { return deltas(tt.a...), nil }}},
				{ID: "b", Provider: provider{call: func() (<-chan types.Delta, error) {
					bCalls.Add(1)
					return deltas(types.UsageDelta{PromptTokens: 3}, types.PartDelta{Index: 0, Text: "from b"}), nil
				}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			ch, err := r.Session().Stream(context.Background(), types.Request{})
			if err != nil {
				t.Fatal(err)
			}
			var text string
			var prompt int
			var streamErr error
			for d := range ch {
				switch v := d.(type) {
				case types.PartDelta:
					text += v.Text
				case types.UsageDelta:
					prompt += v.PromptTokens
				case types.ErrorDelta:
					streamErr = v.Error
				}
			}
			if text != tt.wantText || (streamErr != nil) != tt.wantErr {
				t.Errorf("text = %q, err = %v; want %q, error %v", text, streamErr, tt.wantText, tt.wantErr)
			}
			if bCalls.Load() != tt.wantBCalls {
				t.Errorf("b calls = %d, want %d", bCalls.Load(), tt.wantBCalls)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("forwarded prompt tokens = %d, want %d", prompt, tt.wantPrompt)
			}
		})
	}
}
