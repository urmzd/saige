package convert

import (
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func transcribePolicy(c types.Converter, cache types.ConversionCache) types.ConversionPolicy {
	return types.ConversionPolicy{Dial: per(types.ModalityAudio, types.ActTranscribe), Converters: []types.Converter{c}, Cache: cache}
}

// Convert the view, never the record.
func TestApplyConvertsTheViewNotTheRecord(t *testing.T) {
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "the transcript"}
	audio := wav("clip")
	msgs := []types.Message{types.UserMsg(types.Text("what was said?"), audio),
		types.ToolResults(types.ToolOK("c1", types.Text("tool text"), types.AudioPart{Source: types.Bytes(types.MediaWAV, []byte("nested"))}))}
	pl, err := PlanConversions(visionChat(), msgs, transcribePolicy(c, nil))
	if err != nil {
		t.Fatal(err)
	}
	view, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, NewMemoryCache(0))
	if err != nil {
		t.Fatal(err)
	}
	user := view[0].(types.UserMessage).Parts
	if len(user) != 2 || user[1].(types.TextPart).Text != "the transcript" {
		t.Fatalf("view = %#v, want the audio replaced by its transcript", user)
	}
	nested := view[1].(types.SystemMessage).Parts[0].(types.ToolResultPart).Parts
	if len(nested) != 2 || nested[1].(types.TextPart).Text != "the transcript" {
		t.Fatalf("tool result = %#v, want the nested audio replaced", nested)
	}
	if _, ok := msgs[0].(types.UserMessage).Parts[1].(types.AudioPart); !ok {
		t.Fatal("the record lost its audio part")
	}
	if _, ok := msgs[1].(types.SystemMessage).Parts[0].(types.ToolResultPart).Parts[1].(types.AudioPart); !ok {
		t.Fatal("the record lost its nested audio part")
	}
	if len(rep.Decisions) != 2 || rep.Decisions[0].Action != types.DecisionTranscribed || rep.Decisions[1].Path.Nested != 1 ||
		rep.Hash != rep.ComputeHash() || len(rep.Decisions[0].Produced) != 1 {
		t.Errorf("report = %+v", rep)
	}
}

// History is sent again every turn and may fail over between members: the
// conversion runs once.
func TestApplyMemoizesAcrossTurnsAndMembers(t *testing.T) {
	c := &fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "t"}
	cache := NewMemoryCache(0)
	msgs := []types.Message{types.UserMsg(wav("clip"))}
	for _, target := range []types.Offering{visionChat(), visionChat(), textOnly()} {
		pl, err := PlanConversions(target, msgs, transcribePolicy(c, cache))
		if err != nil {
			t.Fatal(err)
		}
		_, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, cache)
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, types.AssistantMsg(types.Text("a")), types.UserMsg(types.Text("next")))
		_ = rep
	}
	if c.calls.Load() != 1 {
		t.Fatalf("converter ran %d times, want once", c.calls.Load())
	}

	// Another scope is another tenant: no hit.
	pol := transcribePolicy(c, cache)
	pol.Scope = "tenant-b"
	pl, _ := PlanConversions(visionChat(), msgs[:1], pol)
	if _, rep, _ := pl.Apply(t.Context(), msgs[:1], Runtime{}, cache); rep.Decisions[0].Cached || c.calls.Load() != 2 {
		t.Errorf("a different scope was served from the cache (calls %d)", c.calls.Load())
	}
}

func TestApplyFallsBackToTheNextPermittedAction(t *testing.T) {
	broken := &fake{action: types.ActExtract, media: types.ModalityDocument, err: errBoom}
	msgs := []types.Message{types.UserMsg(types.DocumentPart{Source: types.Bytes("text/x-weird", []byte("doc"))})}
	pol := types.ConversionPolicy{Dial: per(types.ModalityDocument, types.ActExtract, types.ActOmit), Converters: []types.Converter{broken}}
	pl, err := PlanConversions(visionChat(), msgs, pol)
	if err != nil {
		t.Fatal(err)
	}
	view, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	notice := view[0].(types.UserMessage).Parts[0].(types.TextPart).Text
	if !strings.Contains(notice, "omitted") || !strings.Contains(rep.Decisions[0].Reason, "boom") || rep.Decisions[0].Action != types.DecisionOmitted {
		t.Fatalf("notice %q, decision %+v", notice, rep.Decisions[0])
	}

	pol.Dial = per(types.ModalityDocument, types.ActExtract)
	pl, _ = PlanConversions(visionChat(), msgs, pol)
	if _, _, err := pl.Apply(t.Context(), msgs, Runtime{}, nil); !errors.Is(err, types.ErrModalityUnsupported) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want a rejection naming the failure", err)
	}
}

// A conversion is a durable step: a replay returns the recorded parts and
// restores the recorded charge, without calling the converter's model.
func TestApplyReplayDoesNotRebill(t *testing.T) {
	pricing := types.Pricing{InputPerMTok: 1, OutputPerMTok: 1}
	c := pricedFake{&fake{action: types.ActTranscribe, media: types.ModalityAudio, text: "t", cost: 500, pricing: pricing}}
	msgs := []types.Message{types.UserMsg(wav("clip"))}
	j := newJournal()
	run := func(cache types.ConversionCache) (*types.Budget, []types.BudgetReceipt, types.ConversionReport) {
		b := types.NewBudget(types.BudgetPolicy{Limit: 10_000_000_000, PerCallCost: 1000})
		main, err := b.ReserveWith("main", pricing, types.ConversionEstimate{Cost: 500})
		if err != nil {
			t.Fatal(err)
		}
		if main.Cost != 1500 {
			t.Fatalf("reservation = %v, want the per-call bound plus the estimate", main.Cost)
		}
		var receipts []types.BudgetReceipt
		rt := Runtime{Steps: j, Budget: b, Reservation: "main", OnReceipt: func(r types.BudgetReceipt) { receipts = append(receipts, r) }}
		pl, err := PlanConversions(visionChat(), msgs, transcribePolicy(c, cache))
		if err != nil {
			t.Fatal(err)
		}
		_, rep, err := pl.Apply(t.Context(), msgs, rt, cache)
		if err != nil {
			t.Fatal(err)
		}
		return b, receipts, rep
	}
	b1, r1, rep := run(NewMemoryCache(0))
	if c.calls.Load() != 1 || len(r1) != 1 || r1[0].Cost != 120 {
		t.Fatalf("first run: calls %d, receipts %+v; want one charge of 120 micro-units (100 in + 20 out at $1 per million)", c.calls.Load(), r1)
	}
	if b1.Spent() != 120 || rep.Decisions[0].Cost == nil || *rep.Decisions[0].Cost != 120 {
		t.Fatalf("spent %v, decision cost %v", b1.Spent(), rep.Decisions[0].Cost)
	}
	if len(j.runs) != 1 || !strings.HasPrefix(j.runs[0], StepPrefix) || !strings.HasSuffix(j.runs[0], ":fake-transcribe@1") {
		t.Fatalf("steps = %v, want one convert:<digest>:<converter> step", j.runs)
	}

	// A crash and a replay: a fresh process, budget and cache.
	b2, r2, _ := run(NewMemoryCache(0))
	if c.calls.Load() != 1 {
		t.Fatalf("replay called the converter again (%d calls)", c.calls.Load())
	}
	if len(r2) != 1 || r2[0].ID != r1[0].ID || b2.Spent() != 120 {
		t.Fatalf("replay: receipts %+v, spent %v; want the recorded charge restored once", r2, b2.Spent())
	}
}

func TestDocumentsExtractsAPDF(t *testing.T) {
	doc := pdf(onePagePDF("Invoice 4417 total 92 dollars"))
	doc.Source.Filename = "invoice.pdf"
	msgs := []types.Message{types.UserMsg(types.Text("total?"), doc)}
	pol := types.ConversionPolicy{Dial: per(types.ModalityDocument, types.ActExtract), Converters: []types.Converter{Documents()}}
	pl, err := PlanConversions(textOnly(), msgs, pol)
	if err != nil {
		t.Fatal(err)
	}
	view, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, NewMemoryCache(0))
	if err != nil {
		t.Fatal(err)
	}
	text := view[0].(types.UserMessage).Parts[1].(types.TextPart).Text
	if !strings.Contains(text, "Invoice 4417") || rep.Decisions[0].Via != "documents@1" || rep.Decisions[0].Action != types.DecisionExtracted {
		t.Fatalf("text %q, decision %+v", text, rep.Decisions[0])
	}
}

func TestDocumentsRejectsAnEmptyExtraction(t *testing.T) {
	msgs := []types.Message{types.UserMsg(pdf([]byte("not a pdf")))}
	pol := types.ConversionPolicy{Dial: per(types.ModalityDocument, types.ActExtract), Converters: []types.Converter{Documents()}}
	pl, _ := PlanConversions(textOnly(), msgs, pol)
	if _, _, err := pl.Apply(t.Context(), msgs, Runtime{}, nil); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want a rejection", err)
	}
}

func TestTranscodeFitsOnlyTargetsThatTakePNG(t *testing.T) {
	gif := types.Image(types.Bytes(types.MediaGIF, tinyGIF()))
	o := visionChat()
	pol := types.ConversionPolicy{Dial: per(types.ModalityImage, types.ActConvert), Converters: []types.Converter{Transcode()}}
	pl, err := PlanConversions(o, []types.Message{types.UserMsg(gif)}, pol)
	if err != nil {
		t.Fatal(err)
	}
	view, _, err := pl.Apply(t.Context(), []types.Message{types.UserMsg(gif)}, Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	img := view[0].(types.UserMessage).Parts[0].(types.ImagePart)
	if img.Source.MediaType != types.MediaPNG || img.Width != 1 || img.Source.Digest == "" {
		t.Fatalf("image = %+v, want a 1x1 PNG", img)
	}
	if _, err := PlanConversions(textOnly(), []types.Message{types.UserMsg(gif)}, pol); err == nil {
		t.Fatal("transcode was planned for a target that takes no images")
	}
}

// tinyGIF is a 1x1 GIF.
func tinyGIF() []byte {
	return []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
}

func TestBudgetCarve(t *testing.T) {
	b := types.NewBudget(types.BudgetPolicy{Limit: 10_000, PerCallCost: 1000, MaxRequests: 2})
	priced := types.Pricing{InputPerMTok: 1}
	if _, err := b.ReserveWith("main", priced, types.ConversionEstimate{Cost: 300}); err != nil {
		t.Fatal(err)
	}
	child, err := b.Carve("main", "conv", priced, 300, 0)
	if err != nil || child.Cost != 300 || child.Requests != 1 {
		t.Fatalf("carve = %+v, %v", child, err)
	}
	if _, err := b.Carve("main", "conv2", priced, 10, 0); !errors.Is(err, types.ErrBudgetExceeded) {
		t.Fatalf("a third request under MaxRequests 2 was admitted: %v", err)
	}
	if _, err := b.Carve("main", "free", types.Pricing{}, 10, 0); !errors.Is(err, types.ErrUnpriced) {
		t.Fatalf("an unpriced converter was admitted under a cost limit: %v", err)
	}
}

// lowerView plans and applies msgs on the Chat-like offering.
func lowerView(t *testing.T, msgs []types.Message) ([]types.Message, types.ConversionReport) {
	t.Helper()
	pl, err := PlanConversions(visionChat(), msgs, types.ConversionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !pl.Rewrites() || pl.Converts() {
		t.Fatalf("plan rewrites=%v converts=%v, want a lowering only", pl.Rewrites(), pl.Converts())
	}
	view, rep, err := pl.Apply(t.Context(), msgs, Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view, rep
}

// describeView renders a view as one line per message, for comparisons.
func describeView(msgs []types.Message) []string {
	var out []string
	for _, m := range msgs {
		var b strings.Builder
		b.WriteString(string(m.Role()) + ":")
		for _, p := range types.PartsOf(m) {
			switch v := p.(type) {
			case types.TextPart:
				b.WriteString(" text(" + v.Text + ")")
			case types.ToolCallPart:
				b.WriteString(" call(" + v.ID + ")")
			case types.ToolResultPart:
				b.WriteString(" result(" + v.CallID + ":")
				for _, np := range v.Parts {
					if t, ok := np.(types.TextPart); ok {
						b.WriteString(" " + t.Text)
					} else {
						b.WriteString(" " + string(np.Kind()))
					}
				}
				b.WriteString(")")
			default:
				src, _ := types.SourceOf(p)
				b.WriteString(" " + string(p.Kind()) + "(" + string(src.Inline) + ")")
			}
		}
		out = append(out, b.String())
	}
	return out
}

func sameView(t *testing.T, got []types.Message, want ...string) {
	t.Helper()
	if g := describeView(got); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Fatalf("view:\n%s\nwant:\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
}

// A tool result image on a surface whose tool results carry text only is
// moved to a user message after the tool results; the record keeps it.
func TestApplyLowersAToolResultImage(t *testing.T) {
	img := pngPart("red").(types.ToolOutputPart)
	msgs := []types.Message{
		types.UserMsg(types.Text("what color?")),
		types.AssistantMsg(types.ToolCallPart{ID: "c1", Name: "snapshot"}),
		types.ToolResults(types.ToolOK("c1", types.Text("snapshot taken"), img)),
	}
	view, rep := lowerView(t, msgs)
	sameView(t, view,
		"user: text(what color?)",
		"assistant: call(c1)",
		"system: result(c1: snapshot taken [image image/png attached in the next message])",
		"user: text([output of tool call c1 (snapshot)]) image(red)")
	if _, ok := msgs[2].(types.SystemMessage).Parts[0].(types.ToolResultPart).Parts[1].(types.ImagePart); !ok || len(msgs) != 3 {
		t.Fatal("the record changed")
	}
	if len(rep.Decisions) != 1 || rep.Decisions[0].Action != types.DecisionLowered || rep.Decisions[0].Path.Nested != 1 {
		t.Errorf("report = %+v", rep)
	}
}

// Every lowered part of one turn goes into one follow-up message, in order,
// labelled by its call, after all of the turn's tool results; text parts
// stay in their tool results.
func TestApplyLowersOneFollowUpPerTurn(t *testing.T) {
	part := func(s string) types.ToolOutputPart { return pngPart(s).(types.ToolOutputPart) }
	msgs := []types.Message{
		types.UserMsg(types.Text("compare")),
		types.AssistantMsg(types.ToolCallPart{ID: "c1", Name: "a"}, types.ToolCallPart{ID: "c2", Name: "b"}, types.ToolCallPart{ID: "c3", Name: "c"}),
		types.ToolResults(
			types.ToolOK("c1", part("one"), types.Text("between"), part("two")),
			types.ToolOK("c2", types.Text("text only")),
		),
		types.ToolResults(types.ToolOK("c3", part("three"))),
		types.AssistantMsg(types.Text("next turn"), types.ToolCallPart{ID: "c4", Name: "a"}),
		types.UserMsg(types.ToolOK("c4", part("four")), types.Text("and say why")),
	}
	view, _ := lowerView(t, msgs)
	sameView(t, view,
		"user: text(compare)",
		"assistant: call(c1) call(c2) call(c3)",
		"system: result(c1: [image image/png attached in the next message] between [image image/png attached in the next message]) result(c2: text only)",
		"system: result(c3: [image image/png attached in the next message])",
		"user: text([output of tool call c1 (a)]) image(one) image(two) text([output of tool call c3 (c)]) image(three)",
		"assistant: text(next turn) call(c4)",
		"user: result(c4: [image image/png attached in the next message])",
		"user: text([output of tool call c4 (a)]) image(four)",
		"user: text(and say why)")
}

// An offering that takes media inline in tool results is not rewritten.
func TestApplyLeavesInlineToolResultMedia(t *testing.T) {
	o := visionChat()
	o.Modalities.ToolResult = map[types.Modality]string{types.ModalityImage: types.ToolResultInline}
	msgs := []types.Message{types.AssistantMsg(types.ToolCallPart{ID: "c1"}),
		types.ToolResults(types.ToolOK("c1", pngPart("x").(types.ToolOutputPart)))}
	pl, err := PlanConversions(o, msgs, types.ConversionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if pl.Rewrites() || pl.Decisions[0].Action != types.DecisionNative {
		t.Fatalf("plan = %+v, want the image native", pl.Decisions)
	}
}
