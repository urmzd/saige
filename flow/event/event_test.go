package event_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/event"
)

var update = flag.Bool("update", false, "rewrite testdata from the samples")

var at = time.Date(2026, 10, 28, 14, 3, 11, 0, time.UTC)

// sample returns an envelope around b, run scoped for run and control types.
func sample(b event.Body) event.Envelope {
	if f := event.Family(b.Type()); f != "run" && f != "control" {
		return event.Envelope{
			ID: "ev-1", Source: "slack://T01", Type: b.Type(), Subject: "C02", Channel: "slack://T01/C02",
			Time: at, Data: b,
		}
	}
	return event.Envelope{
		ID: "ev-1", Source: "fiber://run/r1", Type: b.Type(), Channel: "slack://T01/C02",
		Time: at, Hops: 1, Tenant: "acme", Run: "r1", Seq: 7, Agent: "helper", Version: "1.2.0",
		Digest: "sha256:abc", Data: b,
	}
}

func samples() []event.Body {
	q := event.Question{
		ID: "q1", Kind: event.QuestionApproval, Agent: "helper", Prompt: "May I deploy?",
		Options: []string{"yes", "no"}, Tool: &event.QuestionTool{Name: "deploy", CallID: "c1", Arguments: json.RawMessage(`{"env":"prod"}`)},
		Approvers: []string{"role:oncall"}, ExpiresAt: at.Add(time.Hour), At: at,
		Default: &event.Answer{QuestionID: "q1", Decision: event.DecisionDeny, Expired: true},
	}
	cost := &event.Cost{Micros: 1_250, Currency: "USD", AsOf: "2026-07-24"}
	usage := event.Usage{ModelCalls: 2, Requests: 2, PromptTokens: 900, CachedPromptTokens: 400, CompletionTokens: 120, Cost: cost}
	bodies := []event.Body{
		event.Feedback{RunID: "r1", Actor: event.Actor{ID: "ana"}, Rating: "down", Reasons: []string{"incorrect"}, Comment: "Wrong line", Surface: "slack"},
		event.Accepted{Attempt: 1},
		event.Started{Attempt: 1},
		event.Parked{Attempt: 1, Reason: "waiting on a person", Question: &q},
		event.Resumed{Attempt: 2, Answer: &event.Answer{QuestionID: "q1", Decision: event.DecisionApprove, Note: "go", By: "ana", At: at}},
		event.Completed{Attempt: 2, Text: "Deployed.", Usage: &event.RunUsage{Own: usage, Total: usage, Context: &event.ContextUsage{Model: "m", Window: 200000, Tokens: 900}}},
		event.Failed{Attempt: 1, Error: "provider unavailable", Terminal: true},
		event.Stopped{Attempt: 1, Reason: "wrong repo", By: "ana", Text: "I was", Usage: &event.RunUsage{Own: usage, Total: usage}},
		event.ChildParked{RunID: "r2", Agent: "deployer", Question: q},
		event.Text{Text: "Hello"},
		event.Thinking{Text: "Consider the plan"},
		event.ToolCall{ID: "c1", Name: "search", Phase: event.PhaseExecuting, Arguments: json.RawMessage(`{"q":"fiber"}`)},
		event.ToolDone{ID: "c1", Name: "search", Result: "3 hits", Arguments: json.RawMessage(`{"q":"fiber"}`)},
		event.Citation{Label: "[1]", Text: "quoted", Source: event.Provenance{DocumentTitle: "Handbook", SourceURI: "https://example.com/h"}, Index: 0},
		event.ToolMarker{ID: "c2", Name: "deploy", Markers: []event.Marker{{Kind: "human_approval", Message: "Deploys need sign-off"}},
			Interrupt: &event.MarkerInterrupt{ID: "i1", Kind: "approval", ExpiresAt: at.Add(time.Hour)}, Parent: []string{"c1"}},
		event.Action{Name: "jira_transition", Target: "jira://acme/PROJ-7", Summary: "moved to Done", Address: "jira://acme/PROJ-7", Data: json.RawMessage(`{"status":"Done"}`)},
		event.UsageCall{AccountingID: "r1/a1/1", Agent: "helper", Model: "m", Requests: 1, PromptTokens: 900, CompletionTokens: 120, Cost: cost, ContextWindow: 200000},
		event.UsageChild{AccountingID: "run:r2", RunID: "r2", Agent: "deployer", Depth: 1, Usage: usage},
		event.Compaction{Trigger: "input_pressure", Strategy: "summary", TokensBefore: 9000, TokensAfter: 2000, Summarized: 30, Kept: 4},
		event.StopRequest{RunID: "r1", Reason: "wrong repo", By: "ana", At: at},
		event.CompactRequest{RunID: "r1", By: "ana", At: at},
	}
	return append(bodies, ingressSamples()...)
}

// Every registered type decodes into its own body, renders, and round trips
// through its testdata file unchanged. A type with no sample, no testdata or
// no renderer fails here.
func TestCatalogComplete(t *testing.T) {
	byType := map[string]event.Body{}
	for _, b := range samples() {
		byType[b.Type()] = b
	}
	types := event.Types()
	if !slices.IsSorted(types) {
		t.Fatalf("Types() is not sorted: %v", types)
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			e, ok := event.Lookup(typ)
			if !ok || e.Type != typ {
				t.Fatalf("Lookup(%q) = %+v, %v", typ, e, ok)
			}
			if !event.ValidType(typ) {
				t.Fatalf("%q is outside the grammar", typ)
			}
			if e.New == nil || e.Render == nil {
				t.Fatalf("%q has no New or no Render", typ)
			}
			if got := e.New().Type(); got != typ {
				t.Fatalf("New().Type() = %q, want %q", got, typ)
			}
			b, ok := byType[typ]
			if !ok {
				t.Fatalf("no sample for %q", typ)
			}
			path := filepath.Join("testdata", typ+".json")
			if *update {
				out, err := json.MarshalIndent(sample(b), "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read testdata: %v (run with -update)", err)
			}
			var env event.Envelope
			if err := json.Unmarshal(file, &env); err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			if reflect.TypeOf(env.Data) != reflect.TypeOf(b) {
				t.Fatalf("decoded %T, want %T", env.Data, b)
			}
			got, _ := json.Marshal(env)
			want, _ := json.Marshal(sample(b))
			if !bytes.Equal(got, want) {
				t.Fatalf("decoded %s, want %s", got, want)
			}
			if err := env.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			out, err := json.MarshalIndent(env, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(out, '\n'), file) {
				t.Fatalf("round trip changed %s:\n%s", path, out)
			}
			r1, err := e.Render(env)
			if err != nil || r1 == "" {
				t.Fatalf("Render = %q, %v", r1, err)
			}
			if r2, _ := e.Render(env); r2 != r1 {
				t.Fatalf("Render is not stable: %q then %q", r1, r2)
			}
		})
	}
	for typ := range byType {
		if _, ok := event.Lookup(typ); !ok {
			t.Errorf("sample %q is not registered", typ)
		}
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, f := range files {
		typ := filepath.Base(f[:len(f)-len(".json")])
		if _, ok := event.Lookup(typ); !ok {
			t.Errorf("testdata %s names no registered type", f)
		}
	}
}

func TestUnknownKeepsBytes(t *testing.T) {
	in := []byte(`{"id":"e","source":"x://a","type":"future.thing.v9","channel":"x://a/b","time":"2026-10-28T14:03:11Z","hops":0,"data":{"z":1,"a":[true,null],"n":1.50}}`)
	var env event.Envelope
	if err := json.Unmarshal(in, &env); err != nil {
		t.Fatalf("an unknown type is not an error: %v", err)
	}
	u, ok := env.Data.(event.Unknown)
	if !ok {
		t.Fatalf("Data = %T, want Unknown", env.Data)
	}
	if u.Type() != "future.thing.v9" || string(u.Raw) != `{"z":1,"a":[true,null],"n":1.50}` {
		t.Fatalf("Unknown = %q %s", u.Type(), u.Raw)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("marshal changed the bytes:\n got %s\nwant %s", out, in)
	}
}

func TestDecodeRejectsMalformedKnown(t *testing.T) {
	for _, data := range []string{`{"text":5}`, `[1]`, `"text"`, `{"text":`} {
		if _, err := event.Decode(event.TypeText, json.RawMessage(data)); err == nil {
			t.Errorf("Decode(%s) of a known type succeeded", data)
		}
	}
	in := []byte(`{"id":"e","source":"s","type":"run.delta.text.v1","channel":"c","time":"2026-10-28T14:03:11Z","hops":0,"data":{"text":5}}`)
	var env event.Envelope
	if err := json.Unmarshal(in, &env); err == nil {
		t.Fatal("an envelope with a broken known body decoded")
	}
	// An unknown type with the same broken-looking data is passed through.
	if b, err := event.Decode("nobody.knows.v1", json.RawMessage(`{"text":5}`)); err != nil {
		t.Fatalf("unknown type: %v", err)
	} else if _, ok := b.(event.Unknown); !ok {
		t.Fatalf("unknown type decoded to %T", b)
	}
	// Empty data is the zero body.
	if b, err := event.Decode(event.TypeText, nil); err != nil || !reflect.DeepEqual(b, event.Body(event.Text{})) {
		t.Fatalf("Decode(nil) = %#v, %v", b, err)
	}
}

func TestValidate(t *testing.T) {
	good := sample(event.Text{Text: "hi"})
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, mut := range map[string]func(*event.Envelope){
		"no id":         func(e *event.Envelope) { e.ID = "" },
		"no source":     func(e *event.Envelope) { e.Source = "" },
		"bad type":      func(e *event.Envelope) { e.Type = "Run.Text" },
		"no channel":    func(e *event.Envelope) { e.Channel = "" },
		"no time":       func(e *event.Envelope) { e.Time = time.Time{} },
		"negative hops": func(e *event.Envelope) { e.Hops = -1 },
		"no data":       func(e *event.Envelope) { e.Data = nil },
		"wrong body":    func(e *event.Envelope) { e.Data = event.Thinking{} },
		"no run":        func(e *event.Envelope) { e.Run = "" },
		"unknown typed": func(e *event.Envelope) { e.Type = "run.future.v1"; e.Data = event.Unknown{TypeName: "x.v1"} },
	} {
		e := good
		mut(&e)
		if err := e.Validate(); !errors.Is(err, event.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestPrivateIsNeverSerialized(t *testing.T) {
	e := sample(event.Text{Text: "hi"})
	e.Private = event.Private{"installation": "secret-123"}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("secret-123")) || bytes.Contains(out, []byte("installation")) {
		t.Fatalf("private data serialized: %s", out)
	}
}

// A run's lineage survives the wire: the fields a subscriber follows a
// response by are read back as they were written, and a root run's event,
// which names no parent, writes no parent keys.
func TestLineageRoundTrips(t *testing.T) {
	called := sample(event.Completed{Attempt: 1, Text: "done"})
	called.Root, called.ParentRun, called.ParentToolCall = "r0", "r1", "htu_r1_2"
	root := sample(event.Started{Attempt: 1})
	root.Root = root.Run
	for _, e := range []event.Envelope{called, root} {
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var back event.Envelope
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		if back.Root != e.Root || back.ParentRun != e.ParentRun || back.ParentToolCall != e.ParentToolCall {
			t.Errorf("%s: lineage (%q, %q, %q) read back as (%q, %q, %q)", out,
				e.Root, e.ParentRun, e.ParentToolCall, back.Root, back.ParentRun, back.ParentToolCall)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"parent_run"`)) || bytes.Contains(out, []byte(`"parent_tool_call"`)) {
		t.Errorf("a root run's event wrote parent keys: %s", out)
	}
}

func TestPrefixIsSegmentAware(t *testing.T) {
	for _, tc := range []struct {
		typ, prefix string
		want        bool
	}{
		{"run.delta.text.v1", "run.delta", true},
		{"run.delta.text.v1", "run", true},
		{"run.delta.text.v1", "run.delta.text.v1", true},
		{"run.delta.text.v1", "run.delta.text", true},
		{"run.deltas.v1", "run.delta", false},
		{"run.delta.text.v1", "run.del", false},
		{"run.delta.text.v1", "", false},
		{"run.delta.text.v1", "run.delta.text.v", false},
		{"runner.started.v1", "run", false},
	} {
		if got := event.HasPrefix(tc.typ, tc.prefix); got != tc.want {
			t.Errorf("HasPrefix(%q, %q) = %v, want %v", tc.typ, tc.prefix, got, tc.want)
		}
	}
	if f := event.Family("run.delta.text.v1"); f != "run" {
		t.Errorf("Family = %q", f)
	}
	if v, ok := event.Version("run.delta.text.v12"); !ok || v != 12 {
		t.Errorf("Version = %d, %v", v, ok)
	}
	if b := event.Base("run.completed.v1"); b != "run.completed" {
		t.Errorf("Base = %q", b)
	}
	for _, bad := range []string{"", "run", "run.v0", "run.v01", "Run.x.v1", "run..v1", "run.x.v", "run.x.1"} {
		if event.ValidType(bad) {
			t.Errorf("ValidType(%q) = true", bad)
		}
	}
}

func TestOptInNeedsExplicitPrefix(t *testing.T) {
	for _, tc := range []struct {
		prefix, typ string
		want        bool
	}{
		{"run", event.TypeUsageCall, false},
		{"run", event.TypeUsageChild, false},
		{"run", event.TypeCompacted, false},
		{"run.usage", event.TypeUsageCall, true},
		{"run.usage", event.TypeUsageChild, true},
		{"run.usage.child", event.TypeUsageChild, true},
		{"run.usage.v1", event.TypeUsageCall, true},
		{"run.context", event.TypeCompacted, true},
		{"run", event.TypeText, true},
		{"run.delta", event.TypeText, true},
		{"control", event.TypeStop, true},
	} {
		if got := event.Selects(tc.prefix, tc.typ); got != tc.want {
			t.Errorf("Selects(%q, %q) = %v, want %v", tc.prefix, tc.typ, got, tc.want)
		}
	}
	for _, typ := range []string{event.TypeUsageCall, event.TypeUsageChild, event.TypeCompacted} {
		if e, _ := event.Lookup(typ); !e.OptIn {
			t.Errorf("%s is not opt-in", typ)
		}
	}
}

// An envelope that decodes re-encodes to bytes that decode to the same
// encoding again.
func FuzzEnvelopeUnmarshal(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte(`{"type":"future.thing.v1","data":[1,2]}`))
	f.Add([]byte(`{"type":"run.delta.text.v1","data":null}`))
	f.Add([]byte(`{"type":"run.delta.text.v1"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		var e1 event.Envelope
		if err := json.Unmarshal(in, &e1); err != nil {
			return
		}
		_ = e1.Validate()
		m1, err := json.Marshal(e1)
		if err != nil {
			t.Fatalf("marshal of a decoded envelope: %v", err)
		}
		var e2 event.Envelope
		if err := json.Unmarshal(m1, &e2); err != nil {
			t.Fatalf("re-decode %s: %v", m1, err)
		}
		m2, err := json.Marshal(e2)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(m1, m2) {
			t.Fatalf("not stable:\n%s\n%s", m1, m2)
		}
	})
}
