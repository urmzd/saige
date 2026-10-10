package ollama

import (
	"context"
	"reflect"

	"github.com/urmzd/saige/agent/types"
)

var _ types.OptionsProvider = (*Adapter)(nil)

// chatStreamWithOptions serves Request.Options: the tool choice
// and dials. A schema in the same request is sent as the format. The tool choice replaces the configured choice for this call
// and is emulated as WithToolChoice describes: none withholds the tools, a
// named choice sends only that tool, and required is rejected. Dials compile
// to the think flag and sampling options for this call. Raw sampling and
// reasoning options are set on the client (WithChatOptions, WithThink); any
// of them in opts fails before any network I/O with an error matching
// types.ErrInvalidModelConfig.
func (a *Adapter) chatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema, opts types.RequestOptions) (<-chan types.Delta, error) {
	raw := opts.Raw()
	choice := raw.ToolChoice
	raw.ToolChoice = nil
	if !reflect.ValueOf(raw).IsZero() {
		return nil, a.Capabilities().OptionError("options", "only tool_choice and dials can be set per request; configure sampling on the client")
	}
	c := *a
	if choice != nil {
		c.toolChoice = choice
	}
	d, err := c.compileDials(opts, tools, schema != nil)
	if err != nil {
		return nil, err
	}
	return d.chatStream(ctx, messages, tools, schema)
}
