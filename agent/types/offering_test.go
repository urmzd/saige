package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// offeringCases are capability declarations that exercise every field the
// projection carries.
func offeringCases() []ModelCapabilities {
	sampling := ModelCapabilities{Provider: "openai", Model: "chat", Family: "chat", MaxOutputTokens: 1000, ContextWindow: 128000,
		DefaultMaxOutputTokens: 512, Caps: map[Capability]bool{
			CapTemperature: true, CapTopP: true, CapMaxOutputTokens: true, CapSeed: true, CapStopSequences: true,
			CapToolChoice: true, CapFrequencyPenalty: true, CapPresencePenalty: true, CapTools: true, CapStreaming: true,
			CapStructuredOutput: true, CapAutomaticPromptCache: true,
		}, StructuredOutput: StructuredOutputNative,
		Pricing: Pricing{Currency: "USD", InputPerMTok: 1, OutputPerMTok: 2, CachedInputPerMTok: 0.1, BatchDiscount: 0.5,
			BatchCachedInputPerMTok: 0.05, AsOf: "2026-10-09"},
		Media: ContentSupport{NativeTypes: map[MediaType]bool{MediaPNG: true, MediaJPEG: true, MediaPDF: true}},
		Notes: []string{"a note"}}
	reasoning := ModelCapabilities{Provider: "anthropic", Model: "r", Family: "r", Caps: map[Capability]bool{
		CapTemperature: true, CapTopP: true, CapReasoningEffort: true, CapReasoningBudget: true, CapReasoningToggle: true,
		CapToolChoice: true, CapReasoning: true, CapWebSearch: true, CapServerTools: true,
	}, ReasoningEfforts: []string{"none", "low", "high"}, DefaultReasoningEffort: "low", MinReasoningBudget: 1024, MaxReasoningBudget: 32000,
		DynamicReasoningBudget: true, ZeroReasoningBudget: true, ReasoningDefaultEnabled: true,
		SamplingRequiresNoReasoning: []Capability{CapTemperature, CapTopP}, RejectsForcedToolChoice: true,
		ServerTools: []ServerToolKind{ServerToolWebSearch}}
	required := reasoning
	required.Model, required.Family, required.ReasoningRequired = "q", "q", true
	required.ReasoningEfforts = []string{"low", "high"}
	required.ZeroReasoningBudget = false
	chatTools := sampling
	chatTools.Model, chatTools.Family, chatTools.ChatCompletionsTools = "t", "t", ChatToolsNoReasoning
	chatTools.Caps = map[Capability]bool{CapReasoningEffort: true, CapTools: true}
	chatTools.ReasoningEfforts = []string{"none", "low"}
	responsesOnly := chatTools
	responsesOnly.Model, responsesOnly.Family, responsesOnly.ChatCompletionsTools = "o", "o", ChatToolsResponsesOnly
	return []ModelCapabilities{sampling, reasoning, required, chatTools, responsesOnly, {Provider: "x", Caps: map[Capability]bool{}}}
}

// comparable drops what the projection fills in on its own.
func comparableCaps(mc ModelCapabilities) ModelCapabilities {
	mc = mc.ForModel(mc.Model)
	mc.Offering = nil
	return mc
}

// TestOfferingProjectionRoundTrip proves the projection loses nothing a
// capability declaration states.
func TestOfferingProjectionRoundTrip(t *testing.T) {
	for _, mc := range offeringCases() {
		t.Run(mc.Model, func(t *testing.T) {
			got := OfferingFromCapabilities(mc).Capabilities()
			if got.Offering == nil {
				t.Fatal("projection does not carry its offering")
			}
			want := comparableCaps(mc)
			got = comparableCaps(got)
			got.Known, got.Model = want.Known, want.Model
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the declaration\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

// TestOfferingSpaceValidateMatchesValidateOptions checks that the
// parameter space an offering states rejects exactly what request
// validation rejects, with the same text.
func TestOfferingSpaceValidateMatchesValidateOptions(t *testing.T) {
	opts := []RequestOptions{
		{}, {Temperature: ptrF(0.5)}, {Temperature: ptrF(3)}, {TopP: ptrF(1.5)}, {TopK: ptrF(5)}, {FrequencyPenalty: ptrF(-3)},
		{Seed: ptrI(7)}, {MaxOutputTokens: ptrI(0)}, {MaxOutputTokens: ptrI(500)}, {MaxOutputTokens: ptrI(5000)},
		{StopSequences: []string{"x"}}, {ParallelTools: ptrB(true)}, {ToolChoice: &ToolChoice{Mode: ToolChoiceRequired}},
		{ToolChoice: &ToolChoice{Mode: ToolChoiceNone}}, {ReasoningEffort: ptrS("low")}, {ReasoningEffort: ptrS("max")},
		{ReasoningEffort: ptrS("none")}, {ReasoningEffort: ptrS("none"), Temperature: ptrF(1)},
		{ReasoningEffort: ptrS("high"), Temperature: ptrF(1)}, {ReasoningBudget: ptrI(2048)}, {ReasoningBudget: ptrI(100)},
		{ReasoningBudget: ptrI(-1)}, {ReasoningBudget: ptrI(0)}, {ReasoningBudget: ptrI(0), TopP: ptrF(0.5)},
		{ReasoningBudget: ptrI(4096), TopP: ptrF(0.5)}, {ReasoningEnabled: ptrB(false)},
		{ReasoningEnabled: ptrB(true), Temperature: ptrF(1)}, {ReasoningEnabled: ptrB(true), ReasoningEffort: ptrS("low")},
	}
	for _, mc := range offeringCases()[:3] {
		space := OfferingFromCapabilities(mc).Space()
		for i, o := range opts {
			want := mc.ValidateOptions(o)
			got := space.Validate(o, RequestShape{Provider: mc.Provider, Model: mc.Model})
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s/%d Validate(%+v)\n got: %v\nwant: %v", mc.Model, i, o, got, want)
			}
		}
	}
}

func TestChatToolsConstraintOnSurface(t *testing.T) {
	for _, mc := range offeringCases()[3:5] {
		space := OfferingFromCapabilities(mc).Space()
		chat := RequestShape{Tools: true, Surface: SurfaceOpenAIChat}
		responses := RequestShape{Tools: true, Surface: SurfaceOpenAIResponses}
		low := RequestOptions{ReasoningEffort: ptrS("low")}
		if err := space.Validate(low, chat); err == nil {
			t.Errorf("%s: chat accepted tools with effort low", mc.Model)
		}
		if err := space.Validate(low, responses); err != nil {
			t.Errorf("%s: the rule fired on responses: %v", mc.Model, err)
		}
		err := space.Validate(RequestOptions{ReasoningEffort: ptrS("none")}, chat)
		if mc.ChatCompletionsTools == ChatToolsNoReasoning && err != nil {
			t.Errorf("no_reasoning rejected effort none: %v", err)
		}
		if mc.ChatCompletionsTools == ChatToolsResponsesOnly && !errors.Is(err, ErrInvalidModelConfig) {
			t.Errorf("responses_only accepted tools on chat: %v", err)
		}
	}
}

func TestOfferingEffectiveDialMapMatchesProjection(t *testing.T) {
	for _, mc := range offeringCases() {
		o := OfferingFromCapabilities(mc)
		if got, want := o.EffectiveDialMap(), o.Capabilities().EffectiveDialMap(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: dial map from the space differs\n got: %+v\nwant: %+v", mc.Model, got, want)
		}
	}
}

func TestOfferingIntersect(t *testing.T) {
	a := Offering{ID: "a", Model: ModelInfo{Vendor: "v", ContextWindow: 100, Known: true}, Features: []Capability{CapTools, CapStreaming},
		Modalities: Modalities{
			In:         map[Modality]ModalityLimit{ModalityImage: {Media: []MediaType{MediaPNG, MediaJPEG}, MaxBytes: 10, Sources: []SourceKind{SourceInline, SourceURI}}},
			ToolResult: map[Modality]string{ModalityImage: ToolResultInline},
		},
		Tiers: map[ServiceTier]TierSpec{ServiceBatch: {Transport: TransportBatch, Discount: 0.5}}}
	b := Offering{ID: "b", Model: ModelInfo{Vendor: "w", ContextWindow: 50, Known: true}, Features: []Capability{CapTools},
		Modalities: Modalities{
			In: map[Modality]ModalityLimit{ModalityImage: {Media: []MediaType{MediaPNG}, MaxBytes: 5, Sources: []SourceKind{SourceInline}},
				ModalityAudio: {Media: []MediaType{MediaWAV}}},
			ToolResult: map[Modality]string{ModalityImage: ToolResultFollowUpUser},
		},
		Tiers: map[ServiceTier]TierSpec{ServiceBatch: {Transport: TransportBatch, Discount: 0.3}}}
	got := a.Intersect(b)
	img := got.Modalities.In[ModalityImage]
	if !slices.Equal(img.Media, []MediaType{MediaPNG}) || img.MaxBytes != 5 || !slices.Equal(img.Sources, []SourceKind{SourceInline}) {
		t.Errorf("image limit = %+v", img)
	}
	if _, ok := got.Modalities.In[ModalityAudio]; ok {
		t.Error("a modality one side lacks survived")
	}
	if got.Modalities.ToolResult[ModalityImage] != ToolResultFollowUpUser {
		t.Errorf("tool result lowering = %q, want the stricter", got.Modalities.ToolResult[ModalityImage])
	}
	if got.Model.ContextWindow != 50 || got.Model.Vendor != "v+w" || !slices.Equal(got.Features, []Capability{CapTools}) {
		t.Errorf("model or features = %+v %v", got.Model, got.Features)
	}
	if got.Tiers[ServiceBatch].Discount != 0.3 {
		t.Errorf("batch discount = %v, want the smaller", got.Tiers[ServiceBatch].Discount)
	}
	mc := a.Capabilities().Intersect(b.Capabilities())
	if mc.Offering == nil || mc.Offering.Modalities.In[ModalityImage].MaxBytes != 5 {
		t.Errorf("ModelCapabilities.Intersect does not intersect the offerings: %+v", mc.Offering)
	}
}

func TestTierPricing(t *testing.T) {
	o := Offering{Pricing: Pricing{Currency: "USD", InputPerMTok: 2, OutputPerMTok: 10, CachedInputPerMTok: 0.2, AsOf: "x"},
		Tiers: map[ServiceTier]TierSpec{ServiceBatch: {Transport: TransportBatch, Discount: 0.5, CachedInputPerMTok: 0.05},
			ServicePriority: {Pricing: &Pricing{InputPerMTok: 4, OutputPerMTok: 20}}}}
	b, ok := o.TierPricing(ServiceBatch)
	if !ok || b.InputPerMTok != 1 || b.OutputPerMTok != 5 || b.CachedInputPerMTok != 0.05 {
		t.Errorf("batch = %+v", b)
	}
	if p, _ := o.TierPricing(ServicePriority); p.InputPerMTok != 4 {
		t.Errorf("priority = %+v", p)
	}
	if _, ok := o.TierPricing(ServiceFlex); ok {
		t.Error("an undeclared tier priced")
	}
	if mc := o.Capabilities(); mc.Pricing.BatchDiscount != 0.5 || mc.Pricing.BatchCachedInputPerMTok != 0.05 {
		t.Errorf("projection lost the batch tier: %+v", mc.Pricing)
	}
}

func TestAcceptsValue(t *testing.T) {
	o := Offering{Model: ModelInfo{Vendor: "openai", Prefix: "gpt-6-luna"}, Endpoint: EndpointInfo{Name: "openai-chat"},
		Params: ParamSpace{Params: map[ParamName]ParamSpec{ParamPromptCacheRetention: {Type: ParamTypeEnum, Values: []string{"24h"}}}}}
	if err := o.AcceptsValue(ParamPromptCacheRetention, "24h"); err != nil {
		t.Fatal(err)
	}
	err := o.AcceptsValue(ParamPromptCacheRetention, "in_memory")
	if !errors.Is(err, ErrInvalidModelConfig) {
		t.Fatalf("in_memory accepted: %v", err)
	}
	if err := (Offering{}).AcceptsValue(ParamPromptCacheRetention, "in_memory"); err != nil {
		t.Fatalf("an undeclared parameter refused a value: %v", err)
	}
}

// targetProvider records the target it was switched to.
type targetProvider struct {
	testProvider
	got  Target
	fail bool
}

func (p *targetProvider) WithTarget(t Target) (Provider, error) {
	if p.fail {
		return nil, fmt.Errorf("%w %s", ErrUnknownTarget, t)
	}
	return &targetProvider{got: t}, nil
}

func TestTarget(t *testing.T) {
	if err := (Target{Model: "m", Preset: "p"}).Validate(); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("two fields validated: %v", err)
	}
	if s := PresetTarget("fast").String(); s != "preset:fast" {
		t.Errorf("String = %q", s)
	}
	p, err := ProviderWithTarget(&targetProvider{}, ProfileTarget("a/b"))
	if err != nil || p.(*targetProvider).got.Profile != "a/b" {
		t.Fatalf("switcher not called: %v %+v", err, p)
	}
	if _, err := ProviderWithTarget(&targetProvider{fail: true}, ProfileTarget("x")); !errors.Is(err, ErrUnknownTarget) {
		t.Errorf("error lost: %v", err)
	}
	if _, err := ProviderWithTarget(testProvider{}, PresetTarget("x")); !errors.Is(err, ErrUnknownTarget) {
		t.Errorf("a plain provider accepted a preset: %v", err)
	}
	if q, err := ProviderWithTarget(testProvider{}, Target{}); err != nil || q == nil {
		t.Errorf("zero target: %v", err)
	}
	if _, err := RetargetMembers([]Provider{testProvider{}}, ProfileTarget("x"), "fallback"); !errors.Is(err, ErrUnknownTarget) {
		t.Errorf("a chain accepted a profile: %v", err)
	}
}

func TestConfigPartReadsLegacyModel(t *testing.T) {
	var c ConfigPart
	if err := json.Unmarshal([]byte(`{"Model":"gpt-4","MaxIter":3}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Target != ModelTarget("gpt-4") || c.MaxIter != 3 {
		t.Fatalf("decoded %+v", c)
	}
	b, _ := json.Marshal(ConfigPart{Target: PresetTarget("p")})
	var back ConfigPart
	if err := json.Unmarshal(b, &back); err != nil || back.Target != PresetTarget("p") {
		t.Fatalf("round trip %s -> %+v (%v)", b, back, err)
	}
}
