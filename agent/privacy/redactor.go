package privacy

import (
	"context"
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

// WithheldResult is the text a tool result is replaced with when it could not
// be redacted. The result is withheld rather than passed through.
const WithheldResult = "tool result withheld: redaction failed"

// ToolRedactor is a types.ToolRedactor backed by a Vault. Tools receive real
// values; the model, the tree, and telemetry receive placeholders.
type ToolRedactor struct {
	Vault Vault
	// Skip exempts a tool from redaction, for example one that only returns
	// data the host already trusts the provider with. nil redacts every tool.
	Skip func(def types.ToolDef) bool
}

var _ types.ToolRedactor = (*ToolRedactor)(nil)

// NewToolRedactor returns a redactor over v.
func NewToolRedactor(v Vault) *ToolRedactor { return &ToolRedactor{Vault: v} }

// RestoreArgs implements types.ToolRedactor. Strings anywhere in args,
// including nested objects and arrays, are restored. args is not modified.
func (r *ToolRedactor) RestoreArgs(_ context.Context, def types.ToolDef, args map[string]any) map[string]any {
	if args == nil || r.skip(def) {
		return args
	}
	return restoreValue(r.Vault, args).(map[string]any)
}

func restoreValue(v Vault, value any) any {
	switch x := value.(type) {
	case string:
		return v.Restore(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = restoreValue(v, e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = restoreValue(v, e)
		}
		return out
	default:
		return value
	}
}

// TokenizeResult implements types.ToolRedactor. The text projection, text
// blocks, and JSON blocks are tokenized. Image and file bytes are passed
// through: detectors read text only. If any part cannot be tokenized the
// whole result is withheld.
func (r *ToolRedactor) TokenizeResult(ctx context.Context, def types.ToolDef, res types.ToolResult) types.ToolResult {
	if r.skip(def) {
		return res
	}
	out, err := tokenizeResult(ctx, r.Vault, res)
	if err != nil {
		return types.ToolResult{Text: WithheldResult, IsError: true}
	}
	return out
}

func (r *ToolRedactor) skip(def types.ToolDef) bool {
	return r == nil || r.Vault == nil || (r.Skip != nil && r.Skip(def))
}

func tokenizeResult(ctx context.Context, v Vault, res types.ToolResult) (types.ToolResult, error) {
	text, err := v.Tokenize(ctx, res.Text)
	if err != nil {
		return types.ToolResult{}, err
	}
	res.Text = text
	if len(res.Blocks) > 0 {
		blocks := make([]types.ToolResultBlock, len(res.Blocks))
		for i, b := range res.Blocks {
			if b, err = tokenizeBlock(ctx, v, b); err != nil {
				return types.ToolResult{}, err
			}
			blocks[i] = b
		}
		res.Blocks = blocks
	}
	return res, nil
}

func tokenizeBlock(ctx context.Context, v Vault, b types.ToolResultBlock) (types.ToolResultBlock, error) {
	switch b.Kind {
	case types.ToolResultBlockText:
		text, err := v.Tokenize(ctx, b.Text)
		if err != nil {
			return b, err
		}
		b.Text = text
	case types.ToolResultBlockJSON:
		raw, err := v.Tokenize(ctx, string(b.JSON))
		if err != nil {
			return b, err
		}
		if json.Valid([]byte(raw)) {
			b.JSON = json.RawMessage(raw)
		} else {
			// A span crossed JSON syntax. Send the tokenized text instead of
			// invalid JSON or the original values.
			b = types.ToolResultBlock{Kind: types.ToolResultBlockText, Text: raw}
		}
	}
	return b, nil
}
