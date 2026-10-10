package durablecodec

import (
	"encoding/gob"
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

func init() {
	gob.Register(types.StepResult{})
	gob.Register(types.SystemMessage{})
	gob.Register(types.UserMessage{})
	gob.Register(types.AssistantMessage{})
	// Every part kind, so any message or tool output a step records encodes.
	gob.Register(types.TextPart{})
	gob.Register(types.JSONPart{})
	gob.Register(types.ImagePart{})
	gob.Register(types.AudioPart{})
	gob.Register(types.VideoPart{})
	gob.Register(types.DocumentPart{})
	gob.Register(types.FilePart{})
	gob.Register(types.ToolResultPart{})
	gob.Register(types.ThinkingPart{})
	gob.Register(types.ToolCallPart{})
	gob.Register(types.ServerToolCallPart{})
	gob.Register(types.ServerToolResultPart{})
	gob.Register(types.CitationPart{})
	gob.Register(types.AudioOutPart{})
	gob.Register(types.ImageOutPart{})
	gob.Register(types.VideoOutPart{})
	gob.Register(types.RefusalPart{})
	// Run metadata can appear in recorded turns and appended input.
	gob.Register(types.ConfigPart{})
	gob.Register(types.FeedbackPart{})
	gob.Register(types.HandoffPart{})
	gob.Register(types.SteerPart{})
	gob.Register(types.TruncationPart{})
	gob.Register(types.RoutePart{})
	gob.Register(types.ApprovalPart{})
	gob.Register(types.GuardrailPart{})
	gob.Register(types.CompactionPart{})
	// Tool-call Arguments are map[string]any decoded from JSON; nested arrays and
	// objects arrive as []interface{} / map[string]interface{} inside interface
	// values and must be registered or gob.Encode fails on real tool schemas.
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
	gob.Register(json.Number(""))
}
