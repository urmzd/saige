package ollama

import (
	"context"
	"reflect"

	"github.com/urmzd/saige/agent/types"
)

var _ types.OptionsProvider = (*Adapter)(nil)

// ChatStreamWithOptions implements types.OptionsProvider for the tool choice
// only, which replaces the configured choice for this call and is emulated
// as WithToolChoice describes: none withholds the tools, a named choice sends
// only that tool, and required is rejected. Sampling and reasoning options
// are set on the client (WithChatOptions, WithThink); any of them in opts
// fails before any network I/O with an error matching
// types.ErrInvalidModelConfig.
func (a *Adapter) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	choice := opts.ToolChoice
	opts.ToolChoice = nil
	if !reflect.ValueOf(opts).IsZero() {
		return nil, a.Capabilities().OptionError("options", "only tool_choice can be set per request; configure sampling on the client")
	}
	c := *a
	if choice != nil {
		c.toolChoice = choice
	}
	return c.ChatStream(ctx, messages, tools)
}
