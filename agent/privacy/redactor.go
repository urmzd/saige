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

// TokenizeResult implements types.ToolRedactor. Text parts, JSON parts and
// text-bearing documents and files (text, CSV or JSON with inline bytes)
// are tokenized. Other media is passed through: detectors read text only.
// If any part cannot be tokenized the whole result is withheld.
func (r *ToolRedactor) TokenizeResult(ctx context.Context, def types.ToolDef, res types.ToolResult) types.ToolResult {
	if r.skip(def) {
		return res
	}
	out, err := tokenizeResult(ctx, r.Vault, res)
	if err != nil {
		return types.ToolResult{Parts: []types.ToolOutputPart{types.Text(WithheldResult)}, IsError: true}
	}
	return out
}

func (r *ToolRedactor) skip(def types.ToolDef) bool {
	return r == nil || r.Vault == nil || (r.Skip != nil && r.Skip(def))
}

func tokenizeResult(ctx context.Context, v Vault, res types.ToolResult) (types.ToolResult, error) {
	if res.Parts == nil {
		return res, nil
	}
	parts := make([]types.ToolOutputPart, len(res.Parts))
	for i, p := range res.Parts {
		t, err := tokenizePart(ctx, v, p)
		if err != nil {
			return types.ToolResult{}, err
		}
		parts[i] = t
	}
	res.Parts = parts
	return res, nil
}

func tokenizePart(ctx context.Context, v Vault, p types.ToolOutputPart) (types.ToolOutputPart, error) {
	switch x := p.(type) {
	case types.TextPart:
		text, err := v.Tokenize(ctx, x.Text)
		if err != nil {
			return p, err
		}
		return types.Text(text), nil
	case types.JSONPart:
		raw, err := v.Tokenize(ctx, string(x.JSON))
		if err != nil {
			return p, err
		}
		if json.Valid([]byte(raw)) {
			return types.JSONPart{JSON: json.RawMessage(raw)}, nil
		}
		// A span crossed JSON syntax. Send the tokenized text instead of
		// invalid JSON or the original values.
		return types.Text(raw), nil
	case types.DocumentPart, types.FilePart:
		t, err := tokenizeMedia(ctx, v, x)
		if err != nil {
			return p, err
		}
		return t.(types.ToolOutputPart), nil
	}
	return p, nil
}
