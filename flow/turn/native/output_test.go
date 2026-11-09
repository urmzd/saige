package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// approvalSchema is the shape of a structured answer. additionalProperties
// and $comment are keywords saige's schema cannot carry: they are dropped on
// the way to the model, and the host validates the answer against them.
const approvalSchema = `{
	"type": "object",
	"properties": {
		"decision": {"type": "string", "enum": ["approve", "deny"], "description": "What was decided."},
		"reason": {"type": "string"},
		"references": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["decision", "reason", "references"],
	"additionalProperties": false,
	"$comment": "checked in full by the host"
}`

const approvalAnswer = `{"decision":"approve","reason":"within policy","references":["doc-1"]}`

// schemaRevision is an agent on provider whose answer is approvalSchema.
func schemaRevision(provider string) native.Revision {
	rev := offlineRevision(1, "You approve or deny.")
	rev.Spec.Provider, rev.Spec.Model = provider, "m"
	rev.Spec.Output = &native.Output{Schema: json.RawMessage(approvalSchema)}
	return rev
}

// structured answers every call with approvalAnswer and enforces a schema,
// as a model with native structured output does.
func structured() *agenttest.FunctionModel {
	return &agenttest.FunctionModel{Fn: func(context.Context, []saigetypes.Message, []saigetypes.ToolDef, agenttest.Request) (agenttest.Response, error) {
		return agenttest.Response{Text: approvalAnswer}, nil
	}}
}

// checkApprovalSchema fails unless got is approvalSchema as saige carries it.
func checkApprovalSchema(t *testing.T, got *saigetypes.ParameterSchema) {
	t.Helper()
	if got == nil {
		t.Fatal("the model was sent no response schema")
	}
	if got.Type != saigetypes.SchemaObject || !slices.Equal(got.Required, []string{"decision", "reason", "references"}) {
		t.Errorf("schema is type %q requiring %v, want an object requiring decision, reason and references", got.Type, got.Required)
	}
	if d := got.Properties["decision"]; d.Type != "string" || !slices.Equal(d.Enum, []string{"approve", "deny"}) || d.Description != "What was decided." {
		t.Errorf("decision is %+v, want a string enum of approve and deny with its description", d)
	}
	if r := got.Properties["references"]; r.Type != "array" || r.Items == nil || r.Items.Type != "string" {
		t.Errorf("references is %+v, want an array of strings", r)
	}
}

// An agent whose spec constrains its answer sends the schema to its model,
// and the answer is the model's JSON. Every layer between the turn and the
// model (a fallback chain, the retry decorator around a built-in provider)
// forwards that the model can enforce a schema, so none of them turns the
// request away.
func TestAnOutputSchemaReachesTheModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fallbacks bool
	}{
		{name: "one model"},
		{name: "the primary of a fallback chain", fallbacks: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary, backup := structured(), structured()
			m := newModels(map[string]saigetypes.Provider{"primary": primary, "backup": backup})
			rev := schemaRevision("primary")
			if tc.fallbacks {
				rev.Spec.Fallbacks = []native.ModelRef{{Provider: "backup", Model: "small-model"}}
			}
			r, err := native.New(rev, m.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, res := collect(t, r, native.Turn{Text: "may I deploy?"})
			if res.Text != approvalAnswer {
				t.Errorf("answer is %q, want the model's JSON %q", res.Text, approvalAnswer)
			}
			calls := primary.Requests()
			if len(calls) != 1 {
				t.Fatalf("the primary was called %d times, want once", len(calls))
			}
			checkApprovalSchema(t, calls[0].Request.Schema)
			if backup.CallCount() != 0 {
				t.Errorf("the fallback was called %d times; the primary answered", backup.CallCount())
			}
		})
	}
}

// The built-in OpenAI-compatible provider, which wraps its adapter in saige's
// retry decorator, sends the schema as the request's response_format.
func TestAnOutputSchemaReachesAnOpenAICompatibleServer(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	content, err := json.Marshal(approvalAnswer)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, `data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":%s},"finish_reason":null}]}

data: {"id":"c","object":"chat.completion.chunk","created":0,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`, content)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(native.EnvOpenAIBaseURL, srv.URL+"/v1")
	t.Setenv(native.EnvOpenAIKey, "sk-test")

	rev := schemaRevision(native.ProviderOpenAI)
	rev.Spec.Model = "gpt-4o-mini"
	r, err := native.New(rev)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, res := collect(t, r, native.Turn{Text: "may I deploy?"})
	if res.Text != approvalAnswer {
		t.Errorf("answer is %q, want the server's JSON", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(bodies))
	}
	format, _ := bodies[0]["response_format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("response_format is %v, want a json_schema", bodies[0]["response_format"])
	}
	spec, _ := format["json_schema"].(map[string]any)
	schema, _ := spec["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if schema["type"] != "object" || props["decision"] == nil || props["references"] == nil {
		t.Errorf("json_schema.schema is %v, want the spec's object", spec["schema"])
	}
}

// A model that cannot enforce a schema fails the turn with saige's
// ErrInvalidModelConfig before it is called. The schema is never dropped: an
// answer in prose where the host expects JSON is worse than no answer.
func TestAModelThatCannotEnforceAnOutputSchemaFailsTheTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		primary   func() saigetypes.Provider
		fallbacks bool
	}{
		{
			name:    "a provider without structured output",
			primary: func() saigetypes.Provider { return freeText() },
		},
		{
			name: "a known model that declares none",
			primary: func() saigetypes.Provider {
				return &agenttest.FunctionModel{Caps: &saigetypes.ModelCapabilities{
					Known: true,
					Caps:  map[saigetypes.Capability]bool{saigetypes.CapStreaming: true},
				}}
			},
		},
		{
			name:      "a fallback chain none of whose models has it",
			primary:   func() saigetypes.Provider { return freeText() },
			fallbacks: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary, backup := tc.primary(), freeText()
			m := newModels(map[string]saigetypes.Provider{"primary": primary, "backup": backup})
			rev := schemaRevision("primary")
			if tc.fallbacks {
				rev.Spec.Fallbacks = []native.ModelRef{{Provider: "backup", Model: "small-model"}}
			}
			r, err := native.New(rev, m.opts...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			res, err := r.Run(context.Background(), native.Turn{Text: "may I deploy?"}, nil)
			if !errors.Is(err, saigetypes.ErrInvalidModelConfig) {
				t.Fatalf("Run: %v, want an error matching ErrInvalidModelConfig", err)
			}
			if res.Text != "" || res.Stopped != nil {
				t.Errorf("a refused turn answered %q (stopped %v)", res.Text, res.Stopped)
			}
			for name, p := range map[string]saigetypes.Provider{"primary": primary, "backup": backup} {
				if n := callCount(p); n != 0 {
					t.Errorf("the %s model was called %d times; a schema it cannot enforce must not reach it", name, n)
				}
			}
		})
	}
}

func freeText() *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("free text")}}
}

func callCount(p saigetypes.Provider) int {
	switch v := p.(type) {
	case *agenttest.ScriptedProvider:
		return v.CallCount()
	case *agenttest.FunctionModel:
		return v.CallCount()
	}
	return 0
}

// A schema the runtime cannot send as written fails the build with
// ErrInvalid, before any model call, rather than running the agent free.
func TestAnInvalidOutputSchemaIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, schema string
	}{
		{name: "not JSON", schema: `{"type": "object",`},
		{name: "not an object", schema: `"object"`},
		{name: "an array at the top", schema: `{"type": "array", "items": {"type": "string"}}`},
		{name: "a string at the top", schema: `{"type": "string"}`},
		{name: "no type at the top", schema: `{"properties": {"decision": {"type": "string"}}}`},
		{name: "a list of types at the top", schema: `{"type": ["object", "null"]}`},
		{name: "a property that is not a schema", schema: `{"type": "object", "properties": {"decision": "string"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			model := structured()
			m := newModels(map[string]saigetypes.Provider{"primary": model})
			rev := schemaRevision("primary")
			rev.Spec.Output.Schema = json.RawMessage(tc.schema)
			_, err := native.New(rev, m.opts...)
			if !errors.Is(err, native.ErrInvalid) {
				t.Fatalf("New: %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "output schema") {
				t.Errorf("error %q does not say the output schema is at fault", err)
			}
			if model.CallCount() != 0 {
				t.Errorf("the model was called %d times", model.CallCount())
			}
		})
	}
}

// A spec that constrains nothing (no output, or an output with an empty or
// null schema) answers in free text, as every spec did before: no schema is
// sent, and a provider that cannot enforce one still runs it.
func TestASpecWithoutAnOutputSchemaAnswersInFreeText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output *native.Output
	}{
		{name: "no output"},
		{name: "an empty schema", output: &native.Output{}},
		{name: "a null schema", output: &native.Output{Schema: json.RawMessage("null")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			model := &agenttest.FunctionModel{Fn: func(context.Context, []saigetypes.Message, []saigetypes.ToolDef, agenttest.Request) (agenttest.Response, error) {
				return agenttest.Response{Text: "free text"}, nil
			}}
			plain := freeText()
			m := newModels(map[string]saigetypes.Provider{"structured": model, "plain": plain})
			for provider, check := range map[string]func(t *testing.T){
				"structured": func(t *testing.T) {
					calls := model.Requests()
					if len(calls) != 1 || calls[0].Request.Schema != nil {
						t.Errorf("the model saw %d calls, the first with schema %+v; want one call and no schema", len(calls), calls)
					}
				},
				"plain": func(t *testing.T) {
					if plain.CallCount() != 1 {
						t.Errorf("the scripted model was called %d times, want once", plain.CallCount())
					}
				},
			} {
				rev := offlineRevision(1, "You answer.")
				rev.Spec.Provider, rev.Spec.Output = provider, tc.output
				r, err := native.New(rev, m.opts...)
				if err != nil {
					t.Fatalf("%s: New: %v", provider, err)
				}
				_, res := collect(t, r, native.Turn{Text: "hi"})
				if res.Text != "free text" {
					t.Errorf("%s: answer is %q, want the model's text", provider, res.Text)
				}
				check(t)
			}
		})
	}
}

// The spec's JSON is what hosts write and digest: an output is
// {"output": {"schema": ...}}, and a spec without one has no output key, so
// the content of every spec written before it is unchanged.
func TestOutputKeepsItsJSONShape(t *testing.T) {
	t.Parallel()

	var spec turn.Spec
	in := `{"name":"approver","model":"m","provider":"p","output":{"schema":{"type":"object"}}}`
	if err := json.Unmarshal([]byte(in), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Output == nil || string(spec.Output.Schema) != `{"type":"object"}` {
		t.Fatalf("output decoded as %+v, want the schema as written", spec.Output)
	}
	out, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"output":{"schema":{"type":"object"}}`) {
		t.Errorf("spec encodes as %s, want output.schema", out)
	}
	bare, err := json.Marshal(turn.Spec{Name: "approver", Model: "m", Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "output") {
		t.Errorf("a spec without an output encodes as %s, want no output key", bare)
	}
}
