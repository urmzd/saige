package eval

import (
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

// Annotation keys for the typed parts of a run. Observations recorded
// before parts existed have none of them, and the scorers that read them
// decline such observations instead of failing them.
const (
	// AnnotationParts holds the final turn's assistant parts, as a JSON
	// array in the shared part codec, without media bytes.
	AnnotationParts = "agent.parts" // []AssistantPart
	// AnnotationMedia holds a [MediaRecord] per media part of the final
	// turn.
	AnnotationMedia = "agent.media" // []MediaRecord
	// AnnotationConversions holds every executed conversion decision of
	// the run's provider calls.
	AnnotationConversions = "agent.conversions" // []types.ConversionDecision
	// AnnotationCitations holds the sources the run's tools cited, numbered
	// by the run's citation registry.
	AnnotationCitations = "agent.citations" // []types.Citation
)

// MediaRecord identifies one media part of a run without its bytes.
type MediaRecord struct {
	Kind      types.PartKind  `json:"kind"`
	MediaType types.MediaType `json:"media_type,omitempty"`
	Ref       string          `json:"ref,omitempty"`
	URI       string          `json:"uri,omitempty"`
	Digest    string          `json:"sha256,omitempty"`
	Size      int64           `json:"size,omitempty"`
}

// MediaRecords lists a record per media part of parts, in order.
func MediaRecords[P types.Part](parts []P) []MediaRecord {
	var out []MediaRecord
	for _, p := range parts {
		src, ok := types.SourceOf(p)
		if !ok {
			continue
		}
		out = append(out, MediaRecord{Kind: p.Kind(), MediaType: src.MediaType, Ref: src.Ref, URI: src.URI,
			Digest: src.Digest, Size: src.Size})
	}
	return out
}

// MarshalParts encodes parts as a JSON array in the shared part codec.
// Media bytes are never written.
func MarshalParts[P types.Part](parts []P) (json.RawMessage, error) {
	raws := make([]json.RawMessage, 0, len(parts))
	for i, p := range parts {
		raw, err := types.MarshalPart(p)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		raws = append(raws, raw)
	}
	return json.Marshal(raws)
}

// UnmarshalAssistantParts decodes a JSON array written by [MarshalParts]
// as assistant parts.
func UnmarshalAssistantParts(raw json.RawMessage) ([]types.AssistantPart, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, err
	}
	out := make([]types.AssistantPart, 0, len(raws))
	for i, r := range raws {
		p, err := types.UnmarshalRolePart[types.AssistantPart](r)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// PartsOf returns the final turn's assistant parts recorded on obs under
// [AnnotationParts]. ok is false when the observation has none recorded,
// such as one stored before parts existed.
func PartsOf(obs topeval.Observation) (parts []types.AssistantPart, ok bool, err error) {
	raw, found := obs.Annotations[AnnotationParts]
	if !found {
		return nil, false, nil
	}
	parts, err = UnmarshalAssistantParts(raw)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", AnnotationParts, err)
	}
	return parts, true, nil
}

// ConversionsOf returns the conversion decisions recorded on obs under
// [AnnotationConversions]. ok is false when none were recorded.
func ConversionsOf(obs topeval.Observation) ([]types.ConversionDecision, bool, error) {
	raw, found := obs.Annotations[AnnotationConversions]
	if !found {
		return nil, false, nil
	}
	var out []types.ConversionDecision
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, true, fmt.Errorf("%s: %w", AnnotationConversions, err)
	}
	return out, true, nil
}

// ToolCitationsOf returns the tool citations recorded on obs under
// [AnnotationCitations].
func ToolCitationsOf(obs topeval.Observation) ([]types.Citation, error) {
	raw, found := obs.Annotations[AnnotationCitations]
	if !found {
		return nil, nil
	}
	var out []types.Citation
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", AnnotationCitations, err)
	}
	return out, nil
}

// AnnotateParts records the final turn's parts on obs under
// [AnnotationParts] and their media under [AnnotationMedia]. An empty list
// is recorded too, so scorers can tell "no parts" from "not recorded".
func AnnotateParts(obs *topeval.Observation, parts []types.AssistantPart) error {
	raw, err := MarshalParts(parts)
	if err != nil {
		return fmt.Errorf("annotate %s: %w", AnnotationParts, err)
	}
	setAnnotation(obs, AnnotationParts, raw)
	if media := MediaRecords(parts); len(media) > 0 {
		raw, err := json.Marshal(media)
		if err != nil {
			return fmt.Errorf("annotate %s: %w", AnnotationMedia, err)
		}
		setAnnotation(obs, AnnotationMedia, raw)
	}
	return nil
}

func setAnnotation(obs *topeval.Observation, key string, raw json.RawMessage) {
	if obs.Annotations == nil {
		obs.Annotations = map[string]json.RawMessage{}
	}
	obs.Annotations[key] = raw
}

// partCollector assembles the parts of each top-level provider call. A
// call ends with its UsageDelta; parts after the last usage (a stream that
// ended without one, such as a failed call) belong to an unfinished call
// and win, keeping its complete parts and any text cut short.
type partCollector struct {
	asm  types.PartAssembler
	last []types.AssistantPart
	open bool
}

func (pc *partCollector) observe(delta types.Delta) {
	switch delta.(type) {
	case types.PartStart, types.PartDelta, types.PartEnd:
		pc.asm.Push(delta)
		pc.open = true
	case types.UsageDelta:
		pc.last, _, _ = pc.asm.Flush()
		pc.asm.Reset()
		pc.open = false
	}
}

func (pc *partCollector) parts() []types.AssistantPart {
	parts := pc.last
	if pc.open {
		parts, _, _ = pc.asm.Flush()
	}
	if parts == nil {
		return []types.AssistantPart{}
	}
	return parts
}
