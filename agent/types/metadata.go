package types

import "encoding/json"

// Run metadata parts. The agent loop resolves or records each of these
// itself; they are stripped before a provider call (see IsMetadata).

// ConfigPart carries agent configuration. Persisted to the tree so
// that serialise/restore round-trips include the full agent config.
// Zero-valued fields mean "no change": only non-zero fields override.
type ConfigPart struct {
	// Target re-targets the provider from the next call on: a model, a
	// router profile or a preset (zero = no change). See ProviderWithTarget.
	Target     Target         `json:",omitzero"`
	MaxIter    int            // max loop iterations (0 = use previous/default)
	Compact    *CompactConfig // compaction strategy (nil = no change)
	CompactNow bool           // trigger immediate compaction this iteration
	// ToolChoice constrains tool use from the next turn on (nil = no change).
	// A required or named choice applies to one model turn and then reverts,
	// so a forced call cannot repeat in a loop. Auto and none stay in effect.
	ToolChoice *ToolChoice `json:",omitempty"`
	// Dials change the generation intents from the next call on, merged
	// field by field over earlier blocks (nil = no change). They take
	// effect at the next safe point; a reasoning change waits for the next
	// user turn while a tool loop with signed reasoning is open.
	Dials *Dials `json:",omitempty"`
	// Reason records why this block was written, for example the outcome
	// that made an OutcomePolicy switch models. It has no effect on the loop.
	Reason string `json:",omitempty"`
}

// UnmarshalJSON decodes a ConfigPart. A record written before targets
// existed carries a "Model" string, which is read as a model target.
func (c *ConfigPart) UnmarshalJSON(b []byte) error {
	type plain ConfigPart
	var v struct {
		plain
		Model string `json:"Model,omitempty"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*c = ConfigPart(v.plain)
	if c.Target.IsZero() && v.Model != "" {
		c.Target = ModelTarget(ModelID(v.Model))
	}
	return nil
}

// Kind implements Part.
func (ConfigPart) Kind() PartKind { return KindConfig }
func (ConfigPart) isPart()        {}
func (ConfigPart) isSystemPart()  {}
func (ConfigPart) isUserPart()    {}

// HandoffPart marks a transfer of control to another agent in the same
// handoff group. It is an agent-scoped overlay: it does not mutate the immutable
// root, but selects which agent is active for subsequent iterations. Like
// ConfigPart, it is stripped from the message stream before the LLM sees it:
// its effect is resolved by the agent loop, not sent as text.
type HandoffPart struct {
	To     string `json:"to"`               // target agent name in the group
	From   string `json:"from,omitempty"`   // agent that initiated the handoff
	Reason string `json:"reason,omitempty"` // optional rationale (telemetry / audit only)
	// Message is the handover note the previous owner wrote for the
	// recipient: what it did, what is left, and what to watch for.
	Message string `json:"message,omitempty"`
	// Context carries data the recipient needs that is not in its own view
	// of the conversation, such as identifiers or intermediate results.
	Context string `json:"context,omitempty"`
}

// Kind implements Part.
func (HandoffPart) Kind() PartKind { return KindHandoff }
func (HandoffPart) isPart()        {}
func (HandoffPart) isSystemPart()  {}
func (HandoffPart) isUserPart()    {} // allow human-forced handoffs too

// Rating represents a binary feedback signal.
type Rating int

// Feedback ratings.
const (
	RatingPositive Rating = 1
	RatingNegative Rating = -1
)

// FeedbackPart captures a user's quality rating and optional comment
// on a prior assistant response. Stored in the tree as metadata: stripped
// before messages reach the LLM.
type FeedbackPart struct {
	TargetNodeID string `json:"target_node_id"` // the node being rated (typically an AssistantMessage)
	Rating       Rating `json:"rating"`         // positive or negative
	Comment      string `json:"comment,omitempty"`
}

// Kind implements Part.
func (FeedbackPart) Kind() PartKind { return KindFeedback }
func (FeedbackPart) isPart()        {}
func (FeedbackPart) isUserPart()    {}

// SteerPart tags a user message that was injected into an active run
// without cancelling it. The tag is stripped before the provider call; the
// message's other content is sent as normal.
type SteerPart struct {
	ID string `json:"id"` // submission ID
}

// Kind implements Part.
func (SteerPart) Kind() PartKind { return KindSteer }
func (SteerPart) isPart()        {}
func (SteerPart) isUserPart()    {}

// TruncationPart marks an assistant turn that was committed before it
// finished. Only completed text is kept: open thinking, open tool calls and
// open media are dropped. It is stripped before the provider call.
type TruncationPart struct {
	Reason string `json:"reason"` // "interrupted", "max_tokens", ...
	// Dropped lists the kinds of the open parts that were not kept.
	Dropped []PartKind `json:"dropped,omitempty"`
}

// Kind implements Part.
func (TruncationPart) Kind() PartKind   { return KindTruncation }
func (TruncationPart) isPart()          {}
func (TruncationPart) isAssistantPart() {}

// RoutePart records which complete configuration produced the turn it is
// attached to, including any traffic split. It is stripped before the
// provider call.
type RoutePart struct {
	Profile         string          `json:"profile,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	Experiment      string          `json:"experiment,omitempty"`
	Variant         string          `json:"variant,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Preset          string          `json:"preset,omitempty"`
	ConfigHash      string          `json:"config_hash,omitempty"`
	CatalogRevision string          `json:"catalog_revision,omitempty"`
	Options         *RequestOptions `json:"-"`
	// Dials records how the turn's dials compiled, when it had any.
	Dials *DialReport `json:"dials,omitempty"`
	// Conversions records how the serving attempt's media was converted,
	// when any was.
	Conversions *ConversionReport `json:"conversions,omitempty"`
}

// Kind implements Part.
func (RoutePart) Kind() PartKind   { return KindRoute }
func (RoutePart) isPart()          {}
func (RoutePart) isSystemPart()    {}
func (RoutePart) isAssistantPart() {}

// RoutePartFrom records the configuration a RouteDelta names.
func RoutePartFrom(r RouteDelta) RoutePart {
	c := RoutePart{Profile: r.Profile, Provider: r.Provider, Model: r.Model, Experiment: r.Experiment,
		Variant: r.Variant, Reason: r.Reason, Preset: r.Preset, ConfigHash: r.ConfigHash, CatalogRevision: r.CatalogRevision}
	if r.Options != nil {
		o := r.Options.Clone()
		c.Options = &o
	}
	if r.Dials != nil {
		d := r.Dials.Clone()
		c.Dials = &d
	}
	if r.Conversions != nil {
		cr := r.Conversions.Clone()
		c.Conversions = &cr
	}
	return c
}

// MarshalJSON writes Options in their snake_case wire form.
func (c RoutePart) MarshalJSON() ([]byte, error) {
	type plain RoutePart
	return json.Marshal(struct {
		plain
		Options *wireOptions `json:"options,omitempty"`
	}{plain(c), toWireOptions(c.Options)})
}

// UnmarshalJSON reads RoutePart, accepting records written before the
// preset fields existed.
func (c *RoutePart) UnmarshalJSON(data []byte) error {
	type plain RoutePart
	var v struct {
		plain
		Options *wireOptions `json:"options,omitempty"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*c = RoutePart(v.plain)
	c.Options = v.Options.requestOptions()
	return nil
}
