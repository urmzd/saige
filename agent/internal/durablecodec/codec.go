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
	gob.Register(types.TextContent{})
	gob.Register(types.ToolUseContent{})
	gob.Register(types.ThinkingContent{})
	gob.Register(types.ToolResultContent{})
	gob.Register(types.FileContent{})
	gob.Register(types.ConfigContent{})
	gob.Register(types.FeedbackContent{})
	gob.Register(types.HandoffContent{})
	// Tool-call Arguments are map[string]any decoded from JSON; nested arrays and
	// objects arrive as []interface{} / map[string]interface{} inside interface
	// values and must be registered or gob.Encode fails on real tool schemas.
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
	gob.Register(json.Number(""))
}
