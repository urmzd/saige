package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func labels(t *testing.T, d Detector, text string) []string {
	t.Helper()
	spans, err := d.Detect(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range spans {
		out = append(out, s.Label+"="+text[s.Start:s.End])
	}
	return out
}

func TestDefaultDetector(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"email", "mail ada@example.com now", []string{"EMAIL=ada@example.com"}},
		{"valid card", "card 4111 1111 1111 1111 ok", []string{"CREDIT_CARD=4111 1111 1111 1111"}},
		{"card failing luhn is ignored", "order 4111111111111112", nil},
		{"ssn", "ssn 123-45-6789", []string{"SSN=123-45-6789"}},
		{"invalid ssn area", "ssn 666-45-6789", nil},
		{"iban", "pay GB82 WEST 1234 5698 7654 32", []string{"IBAN=GB82 WEST 1234 5698 7654 32"}},
		{"iban bad checksum", "pay GB00WEST12345698765432", nil},
		{"ipv4", "host 10.0.0.255", []string{"IP_ADDRESS=10.0.0.255"}},
		{"ipv4 out of range", "version 1.2.3.400", nil},
		{"phone", "call (555) 123-4567", []string{"PHONE=(555) 123-4567"}},
		{"phone with country code", "call +1 555-123-4567", []string{"PHONE=+1 555-123-4567"}},
		{"aws key", "key AKIAABCDEFGHIJKLMNOP", []string{"SECRET=AKIAABCDEFGHIJKLMNOP"}},
		{"several in one pass", "a@b.io and 10.1.1.1", []string{"EMAIL=a@b.io", "IP_ADDRESS=10.1.1.1"}},
		{"plain text", "nothing to see here 12345", nil},
		{"phone before house number", "Phone: 555-123-4567 1234 Main St", []string{"PHONE=555-123-4567"}},
		{"rejected card then email", "ref 4111111111111112 mail ada@example.com", []string{"EMAIL=ada@example.com"}},
		{"phone then card", "call 555-123-4567 1234 or pay 4111 1111 1111 1111", []string{"PHONE=555-123-4567", "CREDIT_CARD=4111 1111 1111 1111"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := labels(t, DefaultDetector(), tc.text)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewRegexDetectorRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    []Pattern
	}{
		{"none", nil},
		{"no label", []Pattern{{Expr: "x"}}},
		{"lower-case label", []Pattern{{Label: "name", Expr: "x"}}},
		{"bad expr", []Pattern{{Label: "X", Expr: "("}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegexDetector(tc.p...); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestChainResolvesOverlaps(t *testing.T) {
	ner := DetectorFunc(func(_ context.Context, text string) ([]Span, error) {
		i := strings.Index(text, "Ada Lovelace")
		if i < 0 {
			return nil, nil
		}
		// Overlaps nothing; a second span overlaps the email and is shorter.
		return []Span{{Start: i, End: i + 12, Label: "PERSON"}, {Start: strings.Index(text, "ada@"), End: strings.Index(text, "ada@") + 3, Label: "NAME"}}, nil
	})
	got := labels(t, Chain(DefaultDetector(), ner), "Ada Lovelace ada@example.com")
	want := "PERSON=Ada Lovelace|EMAIL=ada@example.com"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestVaultRoundTrip(t *testing.T) {
	ctx := context.Background()
	v := NewVault(nil)
	in := "write to ada@example.com, cc bob@example.com, again ada@example.com"
	tok, err := v.Tokenize(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := "write to <<EMAIL_1>>, cc <<EMAIL_2>>, again <<EMAIL_1>>"
	if tok != want {
		t.Fatalf("Tokenize = %q, want %q", tok, want)
	}
	if got := v.Restore(tok); got != in {
		t.Fatalf("Restore = %q", got)
	}
	if got := v.Restore("<<EMAIL_9>> stays"); got != "<<EMAIL_9>> stays" {
		t.Fatalf("unknown placeholder changed: %q", got)
	}

	snap, err := v.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadVault(nil, snap)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Restore(tok); got != in {
		t.Fatalf("restored from snapshot = %q", got)
	}
	// Numbering continues after a reload instead of reusing a placeholder.
	next, _ := loaded.Tokenize(ctx, "carol@example.com")
	if next != "<<EMAIL_3>>" {
		t.Fatalf("after reload = %q", next)
	}
	snap2, _ := v.Snapshot()
	if string(snap) != string(snap2) {
		t.Fatal("snapshot is not deterministic")
	}
}

func TestLoadVaultRejects(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"not json", "{"},
		{"future version", `{"v":2}`},
		{"bad token", `{"v":1,"entries":[{"token":"EMAIL_1","label":"EMAIL","value":"x"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadVault(nil, []byte(tc.data)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestVaultConcurrent(t *testing.T) {
	v := NewVault(nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := v.Tokenize(context.Background(), "ada@example.com")
			if err != nil || tok != "<<EMAIL_1>>" {
				t.Errorf("Tokenize = %q, %v", tok, err)
			}
			_ = v.Restore(tok)
		}()
	}
	wg.Wait()
	if v.Len() != 1 {
		t.Fatalf("Len = %d", v.Len())
	}
}

func TestDetectorErrorsFailClosed(t *testing.T) {
	boom := DetectorFunc(func(context.Context, string) ([]Span, error) { return nil, errors.New("boom") })
	bad := DetectorFunc(func(context.Context, string) ([]Span, error) { return []Span{{Start: 0, End: 99, Label: "X"}}, nil })
	for _, d := range []Detector{boom, bad} {
		if _, err := NewVault(d).Tokenize(context.Background(), "abc"); err == nil {
			t.Fatal("Tokenize: want error")
		}
		if _, err := Redact(context.Background(), d, "abc"); err == nil {
			t.Fatal("Redact: want error")
		}
	}
}

func TestRedact(t *testing.T) {
	got, err := Redact(context.Background(), nil, "mail ada@example.com")
	if err != nil || got != "mail [REDACTED:EMAIL]" {
		t.Fatalf("Redact = %q, %v", got, err)
	}
}

func TestStreamRestorer(t *testing.T) {
	v := NewVault(nil)
	tok, _ := v.Tokenize(context.Background(), "ada@example.com")
	if tok != "<<EMAIL_1>>" {
		t.Fatal(tok)
	}
	for _, tc := range []struct {
		name  string
		parts []string
		want  []string // emitted per Write, then the Flush output last
	}{
		{"whole", []string{"hi <<EMAIL_1>>!"}, []string{"hi ada@example.com!", ""}},
		{"split in placeholder", []string{"hi <<EMA", "IL_1>> ok"}, []string{"hi ", "ada@example.com ok", ""}},
		{"split between brackets", []string{"hi <", "<EMAIL_1>", ">"}, []string{"hi ", "", "ada@example.com", ""}},
		{"not a placeholder", []string{"a << b"}, []string{"a << b", ""}},
		{"pending at end is flushed", []string{"x <<EMAIL_"}, []string{"x ", "<<EMAIL_"}},
		{"unknown placeholder", []string{"<<PHONE_", "7>>"}, []string{"", "<<PHONE_7>>", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewStreamRestorer(v.Restore)
			var got []string
			for _, p := range tc.parts {
				got = append(got, r.Write(p))
			}
			got = append(got, r.Flush())
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("long opener released", func(t *testing.T) {
		r := NewStreamRestorer(v.Restore)
		long := "<<" + strings.Repeat("A", maxPlaceholderLen)
		if got := r.Write(long); got != long {
			t.Fatalf("held %d bytes", len(long)-len(got))
		}
	})
}

func TestToolRedactor(t *testing.T) {
	ctx := context.Background()
	v := NewVault(nil)
	r := NewToolRedactor(v)
	def := types.ToolDef{Name: "lookup"}

	res := r.TokenizeResult(ctx, def, types.ToolResult{Parts: []types.ToolOutputPart{
		types.Text("owner ada@example.com"),
		types.Text("ip 10.0.0.1"),
		types.JSONPart{JSON: json.RawMessage(`{"email":"ada@example.com"}`)},
		types.Image(types.Bytes(types.MediaPNG, []byte("png"))),
	}})
	if len(res.Parts) != 4 || res.Parts[0].(types.TextPart).Text != "owner <<EMAIL_1>>" ||
		res.Parts[1].(types.TextPart).Text != "ip <<IP_ADDRESS_1>>" ||
		string(res.Parts[2].(types.JSONPart).JSON) != `{"email":"<<EMAIL_1>>"}` ||
		string(res.Parts[3].(types.ImagePart).Source.Inline) != "png" {
		t.Fatalf("TokenizeResult = %+v", res)
	}

	args := map[string]any{"to": "<<EMAIL_1>>", "nested": map[string]any{"list": []any{"<<IP_ADDRESS_1>>", 3.0}}}
	got := r.RestoreArgs(ctx, def, args)
	if got["to"] != "ada@example.com" || got["nested"].(map[string]any)["list"].([]any)[0] != "10.0.0.1" {
		t.Fatalf("RestoreArgs = %v", got)
	}
	if args["to"] != "<<EMAIL_1>>" {
		t.Fatal("RestoreArgs modified its input")
	}

	r.Skip = func(d types.ToolDef) bool { return d.Name == "trusted" }
	if out := r.TokenizeResult(ctx, types.ToolDef{Name: "trusted"}, types.ToolResult{Parts: []types.ToolOutputPart{types.Text("bob@example.com")}}); out.Text() != "bob@example.com" {
		t.Fatalf("skipped tool was redacted: %q", out.Text())
	}

	failing := NewToolRedactor(NewVault(DetectorFunc(func(context.Context, string) ([]Span, error) { return nil, errors.New("down") })))
	if out := failing.TokenizeResult(ctx, def, types.ToolResult{Parts: []types.ToolOutputPart{types.Text("ada@example.com")}}); out.Text() != WithheldResult || !out.IsError {
		t.Fatalf("failed redaction passed through: %+v", out)
	}
}

// fakeProvider records the messages it received and replies with a scripted stream.
type fakeProvider struct {
	mu     sync.Mutex
	got    []types.Message
	script []types.Delta
}

func (f *fakeProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs := req.Messages
	f.mu.Lock()
	f.got = msgs
	f.mu.Unlock()
	ch := make(chan types.Delta, len(f.script))
	for _, d := range f.script {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func collect(ch <-chan types.Delta) []types.Delta {
	var out []types.Delta
	for d := range ch {
		out = append(out, d)
	}
	return out
}

func TestProviderDecorator(t *testing.T) {
	inner := &fakeProvider{script: []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartDelta{Index: 0, Text: "Sending to <<EMA"},
		types.PartDelta{Index: 0, Text: "IL_1>> now"},
		types.PartEnd{Index: 0},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "send"},
		types.PartDelta{Index: 1, Args: `{"to":"<<EMAIL`},
		types.PartDelta{Index: 1, Args: `_1>>"}`},
		types.PartEnd{Index: 1, Part: types.ToolCallPart{ID: "c1", Name: "send", Arguments: map[string]any{"to": "<<EMAIL_1>>"}}},
		types.PartDelta{Index: 2, Thinking: "<<EMAIL_1>>"},
		types.PartDelta{Index: 3, Text: "tail <<EMAIL_"},
		types.DoneDelta{},
	}}
	v := NewVault(nil)
	p := NewProvider(inner, v)
	msgs := []types.Message{
		types.UserMsg(types.Text("email ada@example.com please")),
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ToolCallPart{ID: "c0", Name: "lookup", Arguments: map[string]any{"q": "ada@example.com"}},
			types.ThinkingPart{Text: "ada@example.com", Signature: "sig"},
		}},
		types.ToolResults(types.ToolResultPart{CallID: "c0", Parts: []types.ToolOutputPart{types.Text("found ada@example.com")}}),
	}
	ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	out := collect(ch)

	sent := inner.got
	if txt := sent[0].(types.UserMessage).Parts[0].(types.TextPart).Text; txt != "email <<EMAIL_1>> please" {
		t.Fatalf("user text sent = %q", txt)
	}
	am := sent[1].(types.AssistantMessage)
	if am.Parts[0].(types.ToolCallPart).Arguments["q"] != "<<EMAIL_1>>" {
		t.Fatalf("tool args sent = %v", am.Parts[0])
	}
	if am.Parts[1].(types.ThinkingPart).Text != "ada@example.com" {
		t.Fatal("thinking must not be rewritten")
	}
	if tr := sent[2].(types.SystemMessage).Parts[0].(types.ToolResultPart).Text(); tr != "found <<EMAIL_1>>" {
		t.Fatalf("tool result sent = %q", tr)
	}
	if msgs[0].(types.UserMessage).Parts[0].(types.TextPart).Text != "email ada@example.com please" {
		t.Fatal("input messages were modified")
	}

	var text, args strings.Builder
	var end types.ToolCallPart
	var thinking string
	for _, d := range out {
		switch x := d.(type) {
		case types.PartDelta:
			text.WriteString(x.Text)
			args.WriteString(x.Args)
			if x.Thinking != "" {
				thinking = x.Thinking
			}
		case types.PartEnd:
			if tc, ok := x.Part.(types.ToolCallPart); ok {
				end = tc
			}
		}
	}
	if text.String() != "Sending to ada@example.com nowtail <<EMAIL_" {
		t.Fatalf("text = %q", text.String())
	}
	if args.String() != `{"to":"ada@example.com"}` || end.Arguments["to"] != "ada@example.com" {
		t.Fatalf("args = %q, end = %v", args.String(), end.Arguments)
	}
	if thinking != "<<EMAIL_1>>" {
		t.Fatalf("thinking = %q", thinking)
	}
	if _, ok := out[len(out)-1].(types.DoneDelta); !ok {
		t.Fatalf("last delta = %T, want DoneDelta after the flush", out[len(out)-1])
	}
}

func TestProviderRejectsUnsupportedControls(t *testing.T) {
	p := NewProvider(&fakeProvider{}, NewVault(nil))
	if _, err := p.Stream(context.Background(), types.Request{Options: &types.RequestOptions{}}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("options err = %v", err)
	}
	if _, err := p.Stream(context.Background(), types.Request{Schema: &types.ParameterSchema{Type: "object"}}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("schema err = %v", err)
	}
	if p.Unwrap() == nil || !strings.HasPrefix(p.Name(), "privacy(") {
		t.Fatal("decorator identity")
	}
}

func TestProviderFailsClosedOnDetectorError(t *testing.T) {
	inner := &fakeProvider{}
	p := NewProvider(inner, NewVault(DetectorFunc(func(context.Context, string) ([]Span, error) { return nil, errors.New("down") })))
	if _, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("x"))}}); err == nil {
		t.Fatal("want error")
	}
	if inner.got != nil {
		t.Fatal("request was sent")
	}
}
