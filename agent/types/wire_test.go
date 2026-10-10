package types

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// wireRoundTripCases holds one fully populated value per Delta variant.
// Numbers inside maps are json.Number because the decoder keeps their text.
func wireRoundTripCases() []Delta {
	args := map[string]any{"q": "go", "n": json.Number("3"), "deep": map[string]any{"ok": true}}
	return []Delta{
		v1TextStart{},
		v1TextContent{Content: "hello"},
		v1TextEnd{},
		v1ThinkingStart{},
		v1ThinkingContent{Content: "hmm"},
		v1ThinkingEnd{Signature: "sig"},
		v1ToolCallStart{ID: "c1", Name: "search"},
		v1ToolCallArgument{ID: "c1", Content: `{"q":`},
		v1ToolCallEnd{ID: "c1", Arguments: args},
		ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		ToolExecDelta{ToolCallID: "c1", Inner: ToolExecDelta{ToolCallID: "c2", Inner: v1TextContent{Content: "child"}}},
		ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "ok", Error: "", Parts: []ToolOutputPart{
			Text("ok"),
			Image(Bytes(MediaPNG, []byte{1, 2, 3}).With(Source{URI: "file:///a.png", Filename: "a.png"}), ImageMeta{Width: 2}),
			JSONPart{JSON: json.RawMessage(`{"a":1}`)},
		}, Citations: []Citation{{Ordinal: 1, Kind: CitationTool, URI: "https://t", Start: -1, End: -1}}},
		MarkerDelta{ToolCallID: "c1", ToolName: "rm", Arguments: args, Markers: []Marker{
			{Kind: "human_approval", Message: "delete files", Meta: map[string]any{"risk": "high"}},
		}},
		HandoffDelta{From: "a", To: "b", Reason: "billing"},
		CitationDelta{Citation: Citation{Ordinal: 2, Kind: CitationWeb, URI: "https://x", Title: "X", Quote: "q",
			Start: -1, End: -1, Producer: "anthropic", Meta: map[string]any{"rank": json.Number("1")}}, ToolCallID: "c1"},
		DoneDelta{},
		FeedbackDelta{TargetNodeID: "n1", Rating: RatingNegative, Comment: "wrong"},
		UsageDelta{AccountingID: "acc", Cumulative: true, PromptTokens: 10, CachedPromptTokens: 2, CacheWriteTokens: 1,
			CompletionTokens: 5, TotalTokens: 15, Latency: 1234567891 * time.Nanosecond, ResponseModel: "m",
			ResponseID: "r", FinishReasons: []string{"stop"}, CacheHit: true},
		UsageDelta{PromptTokens: 30, CompletionTokens: 9, TotalTokens: 39, Requests: 2,
			PromptByModality:     map[Modality]int{ModalityText: 10, ModalityAudio: 20},
			CompletionByModality: map[Modality]int{ModalityAudio: 9}},
		RouteDelta{Profile: "fast", Provider: "openai", Model: "gpt", Experiment: "exp", Variant: "b", Reason: "fallback",
			Conversions: &ConversionReport{Offering: "openai/gpt@chat", Hash: "h", Decisions: []ConversionDecision{
				{Path: PartPath{Message: 1, Part: 0, Nested: -1}, Kind: KindAudio, MediaType: MediaWAV, Action: DecisionTranscribed, Via: "t@1"},
			}}},
		TruncatedDelta{NodeID: "n2", Reason: "interrupted"},
		QueuedDelta{SubmissionID: "s1", Mode: "queue", Position: 2},
		InjectedDelta{SubmissionID: "s1", Mode: "steer", NodeID: "n3"},
		InterruptedDelta{Reason: "user", SubmissionID: "s2"},
		v1ServerToolCall{ID: "st1", Kind: ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go"}},
		v1ServerToolResult{ID: "st1", Kind: ServerToolCodeExecution, Text: "42", Result: json.RawMessage(`{"stdout":"42"}`),
			IsError: true, Files: []wireFile{{URI: "file:///out.csv", MediaType: MediaCSV, Filename: "out.csv", Data: []byte("a,b")}}},
		PartialJSONDelta{JSON: json.RawMessage(`{"title":"dra"}`)},
		PartStart{Index: 0, Kind: KindText},
		PartStart{Index: 4, Kind: KindToolCall, ID: "c9", Name: "search"},
		PartStart{Index: 5, Kind: KindAudioOut, MediaType: MediaWAV},
		PartDelta{Index: 0, Text: "hi"},
		PartDelta{Index: 1, Thinking: "hmm"},
		PartDelta{Index: 1, Signature: "sig"},
		PartDelta{Index: 4, Args: `{"q":`},
		PartDelta{Index: 6, Refusal: "no"},
		PartDelta{Index: 5, Data: []byte("RIFF")},
		PartDelta{Index: 5, Transcript: "hello"},
		PartEnd{Index: 0},
		PartEnd{Index: 4, Part: ToolCallPart{ID: "c9", Name: "search", Arguments: args}},
		PartEnd{Index: 7, Part: ToolCallPart{ID: "c8", Name: "f", ArgumentsError: "bad"}},
		PartEnd{Index: 1, Part: ThinkingPart{Text: "hmm", Signature: "sig", Redacted: true}},
		PartEnd{Index: 2, Part: CitationPart{Citation: Citation{Ordinal: 1, Kind: CitationWeb, URI: "https://x", Start: -1, End: -1},
			Anchor: &Anchor{PartIndex: 0, Start: 0, End: 2}}},
		PartEnd{Index: 3, Part: ServerToolResultPart{CallID: "st1", ToolKind: ServerToolCodeExecution, Text: "42",
			Outputs: []Part{Document(URL("file:///out.csv", MediaCSV))}}},
		PartEnd{Index: 5, Part: AudioOutPart{Source: Bytes(MediaWAV, []byte("RIFF")), Transcript: "hello", VendorID: "aud_1"}},
		PartEnd{Index: 6, Part: RefusalPart{Text: "no", Category: "safety"}},
		ConversionDelta{Profile: "fast", Report: ConversionReport{Offering: "o", Decisions: []ConversionDecision{
			{Path: PartPath{Message: 0, Part: 1, Nested: -1}, Kind: KindImage, Action: DecisionRejected, Reason: "no vision"},
		}}},
		GuardrailDelta{Guardrail: "pii", Phase: GuardrailPhaseOutput, Action: GuardrailActionRewrite, Reason: "email", Text: "[REDACTED:EMAIL]"},
		GuardrailDelta{Guardrail: "policy", Phase: GuardrailPhaseInput, Action: GuardrailActionBlock, Reason: "off topic", Canceled: true},
		CompactionDelta{Branch: "compact-1", NodeID: "n9", Record: CompactionPart{
			Strategy: "chain(clear_tool_results,summary)", Steps: []string{"summary"}, Trigger: CompactionTriggerInputPressure,
			TokensBefore: 900, TokensAfter: 300, FromBranch: "main", Kept: []NodeID{"a"}, Selected: []NodeID{"b"},
			Cleared: []NodeID{"c"}, Summarized: []NodeID{"d"}, Dropped: []NodeID{"e"}, SummaryNode: "s",
		}},
	}
}

func TestWireRoundTrip(t *testing.T) {
	for _, d := range wireRoundTripCases() {
		t.Run(fmt.Sprintf("%T", d), func(t *testing.T) {
			b, err := MarshalDelta(d)
			if err != nil {
				t.Fatalf("MarshalDelta: %v", err)
			}
			got, err := UnmarshalDelta(b)
			if err != nil {
				t.Fatalf("UnmarshalDelta(%s): %v", b, err)
			}
			if !reflect.DeepEqual(got, d) {
				t.Errorf("round trip mismatch\n got: %#v\nwant: %#v\nwire: %s", got, d, b)
			}
		})
	}
}

// TestWireCoversEveryDelta fails when a new Delta variant is added to the
// package without a wire encoding and a round-trip case.
func TestWireCoversEveryDelta(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "isDelta" || fn.Recv == nil {
				continue
			}
			if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
				declared[id.Name] = true
			}
		}
	}
	covered := map[string]bool{"ErrorDelta": true} // covered by TestWireErrorDelta
	for _, d := range wireRoundTripCases() {
		covered[reflect.TypeOf(d).Name()] = true
	}
	for name := range declared {
		if !covered[name] {
			t.Errorf("%s has no wire round-trip case", name)
		}
	}
	if len(declared) < 20 {
		t.Fatalf("found only %d Delta types; the scan is broken", len(declared))
	}
}

func TestWireEnvelopeShape(t *testing.T) {
	env, err := NewDeltaEnvelope(v1TextContent{Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	env.Seq, env.RunID, env.Path = 42, "r1", []string{"call_9"}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"v":1,"seq":42,"run_id":"r1","path":["call_9"],"kind":"text.delta","data":{"content":"hi"}}`
	if string(b) != want {
		t.Errorf("envelope = %s\nwant      %s", b, want)
	}
	back, err := UnmarshalEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Seq != 42 || back.RunID != "r1" || !reflect.DeepEqual(back.Path, []string{"call_9"}) || !back.IsDelta() {
		t.Errorf("envelope metadata lost: %+v", back)
	}
}

func TestWireEnvelopeShapeV2(t *testing.T) {
	tests := []struct {
		d    Delta
		want string
	}{
		{PartStart{Index: 1, Kind: KindToolCall, ID: "call_9", Name: "search"},
			`{"v":2,"kind":"part.start","data":{"index":1,"kind":"tool_call","id":"call_9","name":"search"}}`},
		{PartDelta{Index: 0, Text: "hi"}, `{"v":2,"kind":"part.delta","data":{"index":0,"text":"hi"}}`},
		{PartEnd{Index: 1, Part: ToolCallPart{ID: "call_9", Name: "search", Arguments: map[string]any{"q": "go"}}},
			`{"v":2,"kind":"part.end","data":{"index":1,"part":{"type":"tool_call","id":"call_9","name":"search","arguments":{"q":"go"}}}}`},
		{PartEnd{Index: 0}, `{"v":2,"kind":"part.end","data":{"index":0}}`},
		{DoneDelta{}, `{"v":2,"kind":"done","data":{}}`},
	}
	for _, tt := range tests {
		b, err := MarshalDelta(tt.d)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("envelope = %s\nwant      %s", b, tt.want)
		}
	}
}

func TestWireInlineLimit(t *testing.T) {
	big := make([]byte, DefaultMaxInlineBytes+1)
	if _, err := MarshalDelta(PartDelta{Index: 0, Data: big}); !errors.Is(err, ErrWireInlineTooLarge) {
		t.Errorf("oversized chunk: err = %v", err)
	}
	end := PartEnd{Index: 0, Part: ImageOutPart{Source: Bytes(MediaPNG, big)}}
	if _, err := MarshalDelta(end); !errors.Is(err, ErrWireInlineTooLarge) {
		t.Errorf("oversized part: err = %v", err)
	}
	enc, err := NewEncoder(EncodeOptions{MaxInlineBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(end); err != nil {
		t.Errorf("unlimited encoder: %v", err)
	}
	small, _ := NewEncoder(EncodeOptions{MaxInlineBytes: 2})
	if _, err := small.Encode(PartDelta{Index: 0, Data: []byte("abc")}); !errors.Is(err, ErrWireInlineTooLarge) {
		t.Errorf("small limit: err = %v", err)
	}
	if _, err := NewEncoder(EncodeOptions{Version: 3}); !errors.Is(err, ErrWireVersion) {
		t.Errorf("version 3 encoder: err = %v", err)
	}
}

func TestWireNestedToolExecDelta(t *testing.T) {
	d := ToolExecDelta{ToolCallID: "outer", Inner: ToolExecDelta{ToolCallID: "inner",
		Inner: ErrorDelta{Error: fmt.Errorf("child: %w", ErrMaxIterations)}}}
	b, err := MarshalDelta(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalDelta(b)
	if err != nil {
		t.Fatal(err)
	}
	path, inner := FlattenDelta(got)
	if !reflect.DeepEqual(path, []string{"outer", "inner"}) {
		t.Errorf("path = %v", path)
	}
	ed, ok := inner.(ErrorDelta)
	if !ok {
		t.Fatalf("inner = %T", inner)
	}
	if ed.Error.Error() != "child: max iterations reached" || !errors.Is(ed.Error, ErrMaxIterations) {
		t.Errorf("inner error = %v", ed.Error)
	}
}

func TestWireErrorDelta(t *testing.T) {
	provider := &ProviderError{Provider: "anthropic", Model: "m", Kind: ErrorKindRateLimit, Code: 429,
		RetryAfter: 1500 * time.Millisecond, Err: errors.New("slow down")}
	tests := []struct {
		name      string
		err       error
		is        []error
		kind      ErrorKind
		retryable bool
		retry     time.Duration
	}{
		{name: "nil"},
		{name: "plain", err: errors.New("boom"), kind: ErrorKindPermanent},
		{name: "sentinel", err: fmt.Errorf("run: %w", ErrStreamCanceled), is: []error{ErrStreamCanceled}, kind: ErrorKindPermanent},
		{name: "cancel cause", err: fmt.Errorf("%w: %w", ErrStreamCanceled, context.Canceled),
			is: []error{ErrStreamCanceled, context.Canceled}, kind: ErrorKindPermanent},
		{name: "provider", err: provider, is: []error{ErrProviderFailed, ErrRateLimited},
			kind: ErrorKindRateLimit, retryable: true, retry: 1500 * time.Millisecond},
		{name: "wrapped provider", err: fmt.Errorf("turn 3: %w", provider), is: []error{ErrProviderFailed, ErrRateLimited},
			kind: ErrorKindRateLimit, retryable: true, retry: 1500 * time.Millisecond},
		{name: "context length", err: &ProviderError{Provider: "openai", Kind: ErrorKindContextLength, Code: 400,
			Err: errors.New("too long")}, is: []error{ErrContextLength}, kind: ErrorKindContextLength},
		{name: "suspended", err: ErrSuspended, is: []error{ErrSuspended}, kind: ErrorKindPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := MarshalDelta(ErrorDelta{Error: tt.err})
			if err != nil {
				t.Fatal(err)
			}
			d, err := UnmarshalDelta(b)
			if err != nil {
				t.Fatal(err)
			}
			got := d.(ErrorDelta).Error
			if tt.err == nil {
				if got != nil {
					t.Fatalf("nil error decoded as %v", got)
				}
				return
			}
			if got.Error() != tt.err.Error() {
				t.Errorf("message = %q, want %q", got.Error(), tt.err.Error())
			}
			for _, target := range tt.is {
				if !errors.Is(got, target) {
					t.Errorf("decoded error does not match %v", target)
				}
			}
			if KindOf(got) != tt.kind || IsTransient(got) != tt.retryable || RetryAfter(got) != tt.retry {
				t.Errorf("kind=%v transient=%v retry=%v", KindOf(got), IsTransient(got), RetryAfter(got))
			}
			if !strings.Contains(string(b), `"kind":"`+tt.kind.String()+`"`) {
				t.Errorf("wire form lacks kind: %s", b)
			}
		})
	}
}

func TestWireInterruptRoundTrip(t *testing.T) {
	created := time.Date(2026, 10, 8, 12, 0, 0, 123, time.UTC)
	in := Interrupt{
		ID: InterruptID("run", []string{"c1"}, "marker", "c2"), RunID: "run", Path: []string{"c1"},
		Kind: InterruptApproval, Payload: json.RawMessage(`{"tool":"rm"}`),
		Markers:   []Marker{{Kind: "human_approval", Message: "ok?", Meta: map[string]any{"n": json.Number("1")}}},
		CreatedAt: created, ExpiresAt: created.Add(time.Hour), Policy: InterruptPolicy{OnExpire: InterruptExpireEscalate},
	}
	b, err := MarshalInterrupt(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalInterrupt(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) || !got.ExpiresAt.Equal(in.ExpiresAt) {
		t.Errorf("times = %v, %v", got.CreatedAt, got.ExpiresAt)
	}
	got.CreatedAt, got.ExpiresAt = in.CreatedAt, in.ExpiresAt
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got %#v\nwant %#v", got, in)
	}
	env, _ := UnmarshalEnvelope(b)
	if env.IsDelta() || env.RunID != "run" {
		t.Errorf("envelope = %+v", env)
	}
	if _, err := env.Delta(); !errors.Is(err, ErrUnknownWireKind) {
		t.Errorf("decoding an interrupt as a delta: %v", err)
	}

	zero := Interrupt{ID: "i", Kind: InterruptClarification}
	b, _ = MarshalInterrupt(zero)
	if strings.Contains(string(b), "expires_at") {
		t.Errorf("zero expiry encoded: %s", b)
	}
	if back, _ := UnmarshalInterrupt(b); !back.ExpiresAt.IsZero() || !back.CreatedAt.IsZero() {
		t.Errorf("zero times decoded as %v, %v", back.CreatedAt, back.ExpiresAt)
	}
}

func TestWireInterruptReplyRoundTrip(t *testing.T) {
	tests := []InterruptReply{
		{ID: "i1", IdempotencyKey: "k", Decision: ApprovalDecision{Approved: true,
			ModifiedArgs: map[string]any{"path": "/tmp"}, Message: "fine"}},
		{ID: "i2", Answer: json.RawMessage(`"blue"`)},
	}
	for _, r := range tests {
		t.Run(r.ID, func(t *testing.T) {
			b, err := MarshalInterruptReply(r)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UnmarshalInterruptReply(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, r) {
				t.Errorf("got %#v\nwant %#v", got, r)
			}
		})
	}
}

func TestWireRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"future version", `{"v":3,"kind":"done"}`, ErrWireVersion},
		{"missing version", `{"kind":"done"}`, ErrWireVersion},
		{"unknown kind", `{"v":1,"kind":"telepathy"}`, ErrUnknownWireKind},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := UnmarshalDelta([]byte(tt.in)); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if _, err := UnmarshalDelta([]byte(`{"v":1,"kind":"tool.exec.delta","data":{"tool_call_id":"x"}}`)); err == nil {
		t.Error("tool.exec.delta without inner decoded")
	}
	if _, err := MarshalDelta(nil); !errors.Is(err, ErrUnknownWireKind) {
		t.Errorf("nil delta: %v", err)
	}
	if _, err := MarshalDelta(ToolExecDelta{ToolCallID: "x"}); err == nil {
		t.Error("ToolExecDelta with nil inner encoded")
	}
}

func TestFlattenDelta(t *testing.T) {
	path, inner := FlattenDelta(v1TextEnd{})
	if path != nil || inner != (v1TextEnd{}) {
		t.Errorf("flat delta: %v %v", path, inner)
	}
}
