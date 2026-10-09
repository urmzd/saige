package fallback

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
)

func TestDefaultFallbackOn(t *testing.T) {
	kind := func(k types.ErrorKind) error { return &types.ProviderError{Kind: k, Err: errors.New("x")} }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"plain error", errors.New("boom"), true},
		{"transient", kind(types.ErrorKindTransient), true},
		{"rate limit", kind(types.ErrorKindRateLimit), true},
		{"unavailable", kind(types.ErrorKindUnavailable), true},
		{"auth", kind(types.ErrorKindAuth), true},
		{"context length", kind(types.ErrorKindContextLength), true},
		{"content filter", kind(types.ErrorKindContentFilter), true},
		{"invalid request", kind(types.ErrorKindInvalidRequest), false},
		{"local configuration", fmt.Errorf("%w: temperature", types.ErrInvalidModelConfig), false},
		{"canceled", fmt.Errorf("call: %w", context.Canceled), false},
		{"budget exceeded", types.ErrBudgetExceeded, false},
		{"budget busy", types.ErrBudgetBusy, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultFallbackOn(tc.err); got != tc.want {
				t.Fatalf("DefaultFallbackOn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultPolicyStopsOnTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name          string
		primary       func() *scriptProvider
		syncErr       error
		wantSecondary bool
	}{
		{"invalid request mid-stream", func() *scriptProvider {
			return &scriptProvider{deltas: []types.Delta{types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindInvalidRequest, Err: errors.New("bad field")}}}}
		}, nil, false},
		{"local configuration error", nil, fmt.Errorf("%w: top_k", types.ErrInvalidModelConfig), false},
		{"context length reaches a larger model", func() *scriptProvider {
			return &scriptProvider{deltas: []types.Delta{types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindContextLength, Err: errors.New("too long")}}}}
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var primary types.Provider
			if tc.primary != nil {
				primary = tc.primary()
			} else {
				primary = &errorProviderSimple{err: tc.syncErr}
			}
			secondary := &scriptProvider{deltas: []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "backup"}, types.TextEndDelta{}}}
			ch, err := New(primary, secondary).ChatStream(context.Background(), nil, nil)
			if err == nil {
				collect(ch)
			}
			if got := secondary.callCount() > 0; got != tc.wantSecondary {
				t.Fatalf("secondary called = %v, want %v", got, tc.wantSecondary)
			}
		})
	}
}

func TestSchemaSkipsMembersThatCannotEnforceIt(t *testing.T) {
	failing := &schemaScriptProvider{&scriptProvider{deltas: []types.Delta{
		types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindUnavailable, Err: errors.New("down")}},
	}}}
	plain := &scriptProvider{deltas: []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "free text"}, types.TextEndDelta{}}}
	schema := &types.ParameterSchema{Type: "object"}

	ch, err := New(failing, plain).ChatStreamWithSchema(context.Background(), nil, nil, schema)
	if err != nil {
		t.Fatal(err)
	}
	text, errs := collect(ch)
	var fe *types.FallbackError
	if text != "" || len(errs) != 1 || !errors.As(errs[0].Error, &fe) {
		t.Fatalf("text = %q errors = %v, want a FallbackError and no text", text, errs)
	}
	if plain.callCount() != 0 {
		t.Fatal("a member without schema support served a schema request")
	}

	_, err = New(plain, &scriptProvider{}).ChatStreamWithSchema(context.Background(), nil, nil, schema)
	if !errors.As(err, &fe) || !errors.Is(err, types.ErrInvalidModelConfig) || plain.callCount() != 0 {
		t.Fatalf("err = %v, want a FallbackError matching ErrInvalidModelConfig", err)
	}
}

func TestSchemaSkipsDecoratedMembersThatCannotEnforceIt(t *testing.T) {
	schema := &types.ParameterSchema{Type: "object"}
	text := []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "{}"}, types.TextEndDelta{}}
	for _, tc := range []struct {
		name     string
		members  func(plain, capable types.Provider) []types.Provider
		wantText string
		wantErr  bool
	}{
		{
			name: "retry wrapped plain then capable",
			members: func(plain, capable types.Provider) []types.Provider {
				return []types.Provider{retry.New(plain, retry.DefaultConfig()), retry.New(capable, retry.DefaultConfig())}
			},
			wantText: "{}",
		},
		{
			name: "plain wrapped twice then capable",
			members: func(plain, capable types.Provider) []types.Provider {
				return []types.Provider{retry.New(retry.New(plain, retry.DefaultConfig()), retry.DefaultConfig()), capable}
			},
			wantText: "{}",
		},
		{
			name: "outside decorator reporting the exported sentinel then capable",
			members: func(plain, capable types.Provider) []types.Provider {
				return []types.Provider{sentinelDecorator{plain}, capable}
			},
			wantText: "{}",
		},
		{
			name: "only retry wrapped plain",
			members: func(plain, _ types.Provider) []types.Provider {
				return []types.Provider{retry.New(plain, retry.DefaultConfig())}
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := &scriptProvider{deltas: text}
			capable := &schemaScriptProvider{&scriptProvider{deltas: text}}
			ch, err := New(tc.members(plain, capable)...).ChatStreamWithSchema(context.Background(), nil, nil, schema)
			if plain.callCount() != 0 {
				t.Fatal("a member without schema support served a schema request")
			}
			if tc.wantErr {
				var fe *types.FallbackError
				if !errors.As(err, &fe) || !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want a FallbackError matching ErrInvalidModelConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, errs := collect(ch)
			if got != tc.wantText || len(errs) != 0 || capable.callCount() != 1 {
				t.Fatalf("text = %q errors = %v capable calls = %d", got, errs, capable.callCount())
			}
		})
	}
}

// sentinelDecorator stands for a decorator outside this module tree, such
// as a tracing layer, that reports an unsupported schema through the exported
// sentinel.
type sentinelDecorator struct{ types.Provider }

func (sentinelDecorator) ChatStreamWithSchema(context.Context, []types.Message, []types.ToolDef, *types.ParameterSchema) (<-chan types.Delta, error) {
	return nil, &types.ProviderError{Kind: types.ErrorKindPermanent, Err: types.ErrSchemaUnsupported}
}

// optionsScript accepts request options and records that it received them.
type optionsScript struct {
	*scriptProvider
	got int
}

func (p *optionsScript) ChatStreamWithOptions(ctx context.Context, m []types.Message, tools []types.ToolDef, _ types.RequestOptions) (<-chan types.Delta, error) {
	p.got++
	return p.ChatStream(ctx, m, tools)
}

func TestOptionsSkipMembersThatCannotReceiveThem(t *testing.T) {
	choice := types.ToolChoice{Mode: types.ToolChoiceRequired}
	opts := types.RequestOptions{ToolChoice: &choice}
	plain := &scriptProvider{deltas: []types.Delta{types.TextContentDelta{Content: "dropped options"}}}
	accepting := &optionsScript{scriptProvider: &scriptProvider{deltas: []types.Delta{types.TextContentDelta{Content: "forced"}}}}
	for _, tc := range []struct {
		name    string
		members []types.Provider
		want    string
		wantErr bool
	}{
		{"skips a member without options", []types.Provider{plain, accepting}, "forced", false},
		{"no member accepts options", []types.Provider{plain}, "", true},
		{"through a retry decorator", []types.Provider{retry.New(plain, retry.DefaultConfig()), accepting}, "forced", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := New(tc.members...).ChatStreamWithOptions(context.Background(), nil, nil, opts)
			if err != nil {
				if !tc.wantErr || !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var text string
			for d := range ch {
				if v, ok := d.(types.TextContentDelta); ok {
					text += v.Content
				}
			}
			if text != tc.want {
				t.Fatalf("text = %q", text)
			}
		})
	}
	if plain.callCount() != 0 {
		t.Fatalf("a member without options was called %d times", plain.callCount())
	}
}
