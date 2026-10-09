package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Progress is one progress notification for a running tool call.
type Progress struct {
	Server string
	// Tool is the remote tool name, before prefixing.
	Tool     string
	Progress float64
	// Total is zero when the server does not know it.
	Total   float64
	Message string
}

type progressKey struct{}

// WithProgress returns a context whose tool calls report progress to fn. Each
// call gets its own progress token, so concurrent calls never see each
// other's notifications. fn runs on the session's read loop and must not
// block.
func WithProgress(ctx context.Context, fn func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// progressHandler returns the handler for one call: the context's, else the
// spec's, else nil (and then no progress token is sent at all).
func (t *serverTool) progressHandler(ctx context.Context) func(Progress) {
	if fn, ok := ctx.Value(progressKey{}).(func(Progress)); ok && fn != nil {
		return fn
	}
	if fn := t.client.spec.OnProgress; fn != nil {
		return func(p Progress) { fn(ctx, p) }
	}
	return nil
}

// onProgress routes a notification to the call that owns its token. A token
// this client did not issue, or one whose call already returned, is ignored.
func (c *Client) onProgress(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	v, ok := c.progress.Load(req.Params.ProgressToken)
	if !ok {
		return
	}
	entry := v.(progressEntry)
	entry.fn(Progress{
		Server:   c.spec.Name,
		Tool:     entry.tool,
		Progress: req.Params.Progress,
		Total:    req.Params.Total,
		Message:  req.Params.Message,
	})
}

type progressEntry struct {
	tool string
	fn   func(Progress)
}
