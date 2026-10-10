package tree

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// legacyDir holds stored data written by the release before typed parts
// (see its fixturegen directory).
const legacyDir = "../internal/testdata/legacy/"

func ptr[T any](v T) *T { return &v }

// v1Want is what each golden version 1 message reads as.
func v1Want() map[string]types.Message {
	png := types.MediaPNG
	return map[string]types.Message{
		"system": types.SystemMsg(
			types.Text("You are terse."),
			types.ConfigPart{Target: types.ModelTarget("gpt-x"), MaxIter: 3,
				Compact:    &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 10, KeepLast: 2},
				ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceAuto},
				Dials:      &types.Dials{Creativity: ptr(types.CreativityFocused), MaxOutput: ptr(int64(512))}, Reason: "fixture"},
			types.HandoffPart{To: "b", From: "a", Reason: "r", Message: "m", Context: "c"},
			types.RoutePart{Profile: "fast", Provider: "openai", Model: "gpt-x", Reason: "primary", Preset: "p", ConfigHash: "h",
				CatalogRevision: "rev", Options: &types.RequestOptions{Temperature: ptr(0.2), MaxOutputTokens: ptr(int64(100))}},
			types.ApprovalPart{Event: types.ApprovalEventGranted, Tool: "write", ToolCallID: "call_w",
				Grant: &types.Grant{ID: "g1", Scope: types.GrantScope("session")}, Approver: "ops", Reason: "ok"},
			types.GuardrailPart{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionBlock, Reason: "ssn"},
			types.CompactionPart{Strategy: "summary", Steps: []string{"summary"}, Trigger: types.CompactionTriggerRule, TokensBefore: 100,
				TokensAfter: 10, FromBranch: "main", Kept: []types.NodeID{"k"}, SummaryNode: "s"},
		),
		"tool_results": types.ToolResults(
			types.ToolOK("call_plain", types.Text("plain result")),
			types.ToolErr("call_err", "boom"),
			types.ToolOK("call_rich",
				types.Text("chart attached"),
				types.Image(types.Source{MediaType: png}),
				types.Image(types.Source{MediaType: png, URI: "https://example.com/chart.png", Filename: "chart.png"}),
				types.Document(types.Source{MediaType: types.MediaPDF, URI: "s3://bucket/report.pdf", Filename: "report.pdf"}),
				types.JSONPart{JSON: json.RawMessage(`{"rows":2}`)},
			),
			types.ToolOK("call_media_only", types.Text("see image"), types.Image(types.Source{MediaType: png})),
			types.ToolResultPart{CallID: "call_cited", Parts: []types.ToolOutputPart{types.Text("sourced")},
				Citations: []types.Citation{types.NewCitation(types.CitationWeb, "https://example.com", "Example")}, ToolVersion: "2"},
		),
		"user": types.UserMsg(
			types.Text("Describe these."),
			types.Image(types.Source{URI: "https://example.com/a.png", MediaType: png, Filename: "a.png"}),
			types.Document(types.Source{MediaType: types.MediaPDF, Filename: "inline.pdf"}),
			types.Video(types.Source{URI: "gs://bucket/clip.mp4", MediaType: types.MediaMP4}),
			types.SteerPart{ID: "sub-1"},
			types.GuardrailPart{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionRewrite},
			types.ConfigPart{MaxIter: 2},
			types.HandoffPart{To: "a"},
		),
		"user_tool_results": types.UserToolResults(types.ToolOK("call_human", types.Text("human answer"))),
		"assistant": types.AssistantMsg(
			types.ThinkingPart{Text: "plan", Signature: "sig-1"},
			types.Text("Looking."),
			types.ServerToolCallPart{ID: "srv_1", ToolKind: types.ServerToolWebSearch, Name: "web_search",
				Input: map[string]any{"query": "go", "max": json.Number("3")}},
			types.ServerToolResultPart{CallID: "srv_1", ToolKind: types.ServerToolWebSearch, Text: "results",
				Result:  json.RawMessage(`{"hits":[{"url":"https://go.dev"}]}`),
				Outputs: []types.Part{types.Image(types.Source{URI: "https://example.com/chart.png", MediaType: png})}},
			types.ServerToolCallPart{ID: "srv_2", ToolKind: types.ServerToolCodeExecution, Name: "code_execution", Input: map[string]any{"code": "1+1"}},
			types.ToolCallPart{ID: "call_rich", Name: "snapshot", Arguments: map[string]any{"n": json.Number("3"), "big": json.Number("12345678901234"),
				"f": json.Number("1.5"), "list": []any{json.Number("1"), map[string]any{"k": "v"}}}},
			types.ToolCallPart{ID: "call_bad", Name: "snapshot", ArgumentsError: "unexpected end of JSON input"},
			types.RoutePart{Provider: "anthropic", Model: "claude-x"},
			types.GuardrailPart{Guardrail: "tone", Phase: types.GuardrailPhaseOutput, Action: types.GuardrailActionPass},
			types.TruncationPart{Reason: "max_tokens"},
		),
	}
}

// compactRaw makes the JSON payloads of msg compact, since the golden file
// is indented and Postgres rewrites JSONB.
func compactRaw(t *testing.T, msg types.Message) types.Message {
	t.Helper()
	c := func(raw json.RawMessage) json.RawMessage {
		if raw == nil {
			return nil
		}
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	output := func(ps []types.ToolOutputPart) []types.ToolOutputPart {
		out := make([]types.ToolOutputPart, len(ps))
		for i, p := range ps {
			if j, ok := p.(types.JSONPart); ok {
				j.JSON = c(j.JSON)
				p = j
			}
			out[i] = p
		}
		return out
	}
	part := func(p types.Part) types.Part {
		switch v := p.(type) {
		case types.ToolResultPart:
			v.Parts = output(v.Parts)
			return v
		case types.ServerToolResultPart:
			v.Result = c(v.Result)
			return v
		}
		return p
	}
	switch m := msg.(type) {
	case types.SystemMessage:
		for i, p := range m.Parts {
			m.Parts[i] = part(p).(types.SystemPart)
		}
		return m
	case types.UserMessage:
		for i, p := range m.Parts {
			m.Parts[i] = part(p).(types.UserPart)
		}
		return m
	case types.AssistantMessage:
		for i, p := range m.Parts {
			m.Parts[i] = part(p).(types.AssistantPart)
		}
		return m
	}
	return msg
}

type storedMessage struct {
	Name    string          `json:"name"`
	Role    types.Role      `json:"role"`
	Message json.RawMessage `json:"message"`
}

func readV1Messages(t *testing.T) []storedMessage {
	t.Helper()
	raw, err := os.ReadFile(legacyDir + "tree/v1_messages.json")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []storedMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// TestV1MessagesGolden reads every content kind the previous release
// stored and checks the parts each one maps to.
func TestV1MessagesGolden(t *testing.T) {
	want := v1Want()
	msgs := readV1Messages(t)
	if len(msgs) != len(want) {
		t.Fatalf("golden has %d messages, want %d", len(msgs), len(want))
	}
	for _, m := range msgs {
		t.Run(m.Name, func(t *testing.T) {
			got, err := UnmarshalMessage(m.Role, m.Message)
			if err != nil {
				t.Fatal(err)
			}
			got = compactRaw(t, got)
			if !reflect.DeepEqual(got, want[m.Name]) {
				t.Fatalf("v1 %s =\n%#v\nwant\n%#v", m.Name, got, want[m.Name])
			}
			// A migrated message is version 2 and reads back the same.
			out, changed, err := MigrateMessage(m.Role, m.Message)
			if err != nil || !changed {
				t.Fatalf("migrate = %v, %v", changed, err)
			}
			if !strings.HasPrefix(string(out), `{"v":2,"parts":[`) {
				t.Fatalf("migrated = %s", out)
			}
			back, err := UnmarshalMessage(m.Role, out)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(compactRaw(t, back), want[m.Name]) {
				t.Fatalf("migrated %s =\n%#v\nwant\n%#v", m.Name, back, want[m.Name])
			}
			if again, changed, err := MigrateMessage(m.Role, out); err != nil || changed || !bytes.Equal(again, out) {
				t.Fatalf("migrating a version 2 message = %s, %v, %v", again, changed, err)
			}
		})
	}
}

// TestV1ElidedMedia checks that media whose bytes were never stored reads
// as elided, so an adapter rejects it instead of sending nothing.
func TestV1ElidedMedia(t *testing.T) {
	for _, m := range readV1Messages(t) {
		msg, err := UnmarshalMessage(m.Role, m.Message)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range types.PartsOf(msg) {
			var media []types.Part
			if r, ok := p.(types.ToolResultPart); ok {
				for _, o := range r.Parts {
					media = append(media, o)
				}
			} else {
				media = append(media, p)
			}
			for _, x := range media {
				src, ok := types.SourceOf(x)
				if !ok {
					continue
				}
				if src.Elided() != (src.URI == "") {
					t.Errorf("%s: %s elided = %v with URI %q", m.Name, x.Kind(), src.Elided(), src.URI)
				}
			}
		}
	}
}

// TestV1TreeGolden loads a tree document the previous release wrote, and
// checks that it rewrites as version 2 with nothing lost.
func TestV1TreeGolden(t *testing.T) {
	raw, err := os.ReadFile(legacyDir + "tree/v1_tree.json")
	if err != nil {
		t.Fatal(err)
	}
	tr := &Tree{}
	if err := json.Unmarshal(raw, tr); err != nil {
		t.Fatal(err)
	}
	if n := len(tr.nodes); n != 7 {
		t.Fatalf("nodes = %d, want 7", n)
	}
	if b := tr.Branches(); len(b) != 3 { // main, alt and the feedback branch
		t.Fatalf("branches = %v", b)
	}
	if cps := tr.Checkpoints(); len(cps) != 1 {
		t.Fatalf("checkpoints = %v", cps)
	}
	if fb := tr.Feedback(); len(fb) != 1 {
		t.Fatalf("feedback = %d nodes", len(fb))
	}
	want := v1Want()
	byText := map[string]string{"Describe these.": "user", "Looking.": "assistant", "You are terse.": "system"}
	seen := 0
	for _, n := range tr.nodes {
		name, ok := byText[firstText(n.Message)]
		if !ok {
			continue
		}
		seen++
		if got := compactRaw(t, n.Message); !reflect.DeepEqual(got, want[name]) {
			t.Fatalf("node %s = %#v, want %#v", name, got, want[name])
		}
	}
	if seen != 3 {
		t.Fatalf("matched %d golden nodes, want 3", seen)
	}

	out, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		V     int `json:"v"`
		Nodes []struct {
			Message json.RawMessage `json:"message"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.V != TreeFormatVersion {
		t.Fatalf("rewritten version = %d", doc.V)
	}
	for _, n := range doc.Nodes {
		if v, err := messageVersion(n.Message); err != nil || v != MessageFormatVersion {
			t.Fatalf("node message version = %d, %v: %s", v, err, n.Message)
		}
	}
	again := &Tree{}
	if err := json.Unmarshal(out, again); err != nil {
		t.Fatal(err)
	}
	for id, n := range tr.nodes {
		n.Message = compactRaw(t, n.Message)
		if !reflect.DeepEqual(again.nodes[id], n) {
			t.Fatalf("node %s changed across a rewrite:\n%#v\n%#v", id, again.nodes[id], n)
		}
	}
}

func firstText(m types.Message) string {
	for _, p := range types.PartsOf(m) {
		if t, ok := p.(types.TextPart); ok {
			return t.Text
		}
	}
	return ""
}

func TestMessageFormatVersion(t *testing.T) {
	for _, tt := range []struct {
		name, data string
		wantErr    error
	}{
		{"future version", `{"v":3,"parts":[]}`, ErrMessageFormatVersion},
		{"version zero", `{"v":0,"parts":[]}`, ErrMessageFormatVersion},
		{"wrong role", `{"v":2,"parts":[{"type":"tool_call","id":"c","name":"f"}]}`, types.ErrPartRole},
		{"unknown kind", `{"v":2,"parts":[{"type":"hologram"}]}`, types.ErrUnknownPartKind},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := UnmarshalMessage(types.RoleUser, json.RawMessage(tt.data)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
	// An empty object is an empty version 1 message.
	msg, err := UnmarshalMessage(types.RoleUser, json.RawMessage(`{}`))
	if err != nil || len(msg.(types.UserMessage).Parts) != 0 {
		t.Fatalf("empty message = %#v, %v", msg, err)
	}
}

// TestMarshalMessageNeverStoresBytes checks that inline media bytes are not
// written; the digest and other locators are.
func TestMarshalMessageNeverStoresBytes(t *testing.T) {
	src := types.Bytes(types.MediaPNG, []byte("secret-bytes")).With(types.Source{Ref: "saige-artifact://abc"})
	msg := types.UserMsg(types.Image(src), types.UserToolResults(types.ToolOK("c", types.Image(src))).Parts[0])
	raw, err := MarshalMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("c2VjcmV0")) || bytes.Contains(raw, []byte("secret")) {
		t.Fatalf("stored bytes: %s", raw)
	}
	got, err := UnmarshalMessage(types.RoleUser, raw)
	if err != nil {
		t.Fatal(err)
	}
	img := got.(types.UserMessage).Parts[0].(types.ImagePart)
	if img.Source.Digest != src.Digest || img.Source.Ref != src.Ref || len(img.Source.Inline) != 0 {
		t.Fatalf("source = %+v", img.Source)
	}
}
