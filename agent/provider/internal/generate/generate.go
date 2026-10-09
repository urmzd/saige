// Package generate runs a single-turn prompt through a provider and keeps the
// usage and finish reasons that a plain text result would drop.
package generate

import (
	"context"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Result is the text of a single-turn response with its merged usage.
type Result struct {
	Text  string
	Usage types.UsageDelta
}

// Run sends prompt as one user message with no tools and drains the stream so
// the producer never blocks. The first ErrorDelta wins. A finish reason that
// means the output limit cut the response short returns a
// *types.ResponseTruncatedError, which matches types.ErrResponseTruncated.
// The partial text and the usage are returned with any error, so the caller
// can still account for the tokens.
func Run(ctx context.Context, p types.Provider, prompt string) (Result, error) {
	ch, err := p.ChatStream(ctx, []types.Message{types.NewUserMessage(prompt)}, nil)
	if err != nil {
		return Result{}, err
	}
	var sb strings.Builder
	var res Result
	var genErr error
	for d := range ch {
		switch v := d.(type) {
		case types.TextContentDelta:
			sb.WriteString(v.Content)
		case types.UsageDelta:
			res.Usage = res.Usage.Merge(v)
		case types.ErrorDelta:
			if genErr == nil {
				genErr = v.Error
			}
		}
	}
	res.Text = sb.String()
	if genErr == nil {
		for _, reason := range res.Usage.FinishReasons {
			if types.IsTruncationFinishReason(reason) {
				genErr = &types.ResponseTruncatedError{FinishReason: types.FinishReasonMaxTokens, OutputTokens: res.Usage.CompletionTokens}
				break
			}
		}
	}
	return res, genErr
}

// Text is Run reduced to the text. On error it returns an empty string, as
// the Generate seam always has.
func Text(ctx context.Context, p types.Provider, prompt string) (string, error) {
	res, err := Run(ctx, p, prompt)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}
