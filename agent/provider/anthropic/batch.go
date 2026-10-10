package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Message Batches limits, from the API documentation.
const (
	batchMaxRequests = 100_000
	batchMaxBytes    = 256 << 20
)

var (
	_ types.BatchProvider = (*Adapter)(nil)
	_ types.BatchFinder   = (*Adapter)(nil)
)

// batchIDPattern is the custom_id alphabet the API accepts.
var batchIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// batchRequestBody is one entry of a create-batch body. Params is the same
// JSON a Messages API call sends, so a batch request carries exactly what a
// streaming call would, without the stream flag.
type batchRequestBody struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

// Submit implements types.BatchProvider with the Message Batches API. Each
// request is validated and encoded as Stream with options (or, with a
// schema, Stream with a schema) would encode it, so an option the model
// rejects fails here, before anything is sent. Batches have no metadata, so
// the tag is not sent; FindBatch matches on request IDs instead.
func (a *Adapter) Submit(ctx context.Context, reqs []types.BatchRequest, _ types.BatchSubmitOptions) (types.BatchHandle, error) {
	if err := batch.CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	caps := a.Capabilities()
	if len(reqs) > batchMaxRequests {
		return types.BatchHandle{}, caps.OptionError("batch", fmt.Sprintf("at most %d requests per batch", batchMaxRequests))
	}
	body := struct {
		Requests []batchRequestBody `json:"requests"`
	}{Requests: make([]batchRequestBody, len(reqs))}
	size := 0
	for i, r := range reqs {
		if !batchIDPattern.MatchString(r.CustomID) {
			return types.BatchHandle{}, caps.OptionError("custom_id", fmt.Sprintf("%q must match %s", r.CustomID, batchIDPattern))
		}
		params, err := a.batchParams(r)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", r.CustomID, err)
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: encode: %w", r.CustomID, err)
		}
		size += len(raw)
		body.Requests[i] = batchRequestBody{CustomID: r.CustomID, Params: raw}
	}
	if size > batchMaxBytes {
		return types.BatchHandle{}, caps.OptionError("batch", "a batch is limited to 256 MB")
	}
	var mb anthropic.MessageBatch
	if err := a.client.Post(ctx, "v1/messages/batches", body, &mb); err != nil {
		return types.BatchHandle{}, classifyAnthropicError(string(a.model), err, true)
	}
	return types.BatchHandle{Provider: a.Name(), Model: a.Model(), ID: mb.ID}, nil
}

// batchParams validates one request and builds its Messages parameters
// exactly as Stream does, parts included.
func (a *Adapter) batchParams(r types.BatchRequest) (anthropic.MessageNewParams, error) {
	c, params, _, err := a.buildParams(r.Messages, r.Tools, r.Schema, r.Options)
	if err != nil {
		return params, err
	}
	if c.maxTokens <= 0 {
		return params, c.Capabilities().OptionError("max_output_tokens", "a batch request needs max_tokens above zero")
	}
	return params, nil
}

// structuredToolName is the hidden tool that carries schema output.
const structuredToolName = "structured_output"

// Content block types of a complete message.
const (
	blockText             = "text"
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
	blockToolUse          = "tool_use"
)

// Status implements types.BatchProvider.
func (a *Adapter) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	mb, err := a.client.Messages.Batches.Get(ctx, h.ID, anthropic.MessageBatchGetParams{})
	if err != nil {
		return types.BatchStatus{}, a.batchError(err)
	}
	return batchStatus(mb), nil
}

func batchStatus(mb *anthropic.MessageBatch) types.BatchStatus {
	c := mb.RequestCounts
	st := types.BatchStatus{VendorState: string(mb.ProcessingStatus), CreatedAt: mb.CreatedAt,
		ExpiresAt: mb.ExpiresAt, EndedAt: mb.EndedAt,
		Counts: types.BatchCounts{Processing: int(c.Processing), Succeeded: int(c.Succeeded),
			Errored: int(c.Errored), Canceled: int(c.Canceled), Expired: int(c.Expired)}}
	st.Counts.Total = st.Counts.Processing + st.Counts.Succeeded + st.Counts.Errored + st.Counts.Canceled + st.Counts.Expired
	switch mb.ProcessingStatus {
	case anthropic.MessageBatchProcessingStatusCanceling:
		st.State = types.BatchCanceling
	case anthropic.MessageBatchProcessingStatusEnded:
		switch {
		case !mb.CancelInitiatedAt.IsZero():
			st.State = types.BatchCanceled
		case c.Expired > 0:
			st.State = types.BatchExpired
		default:
			st.State = types.BatchEnded
		}
	default:
		st.State = types.BatchRunning
	}
	return st
}

// Results implements types.BatchProvider. Results arrive in any order.
func (a *Adapter) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		stream := a.client.Messages.Batches.ResultsStreaming(ctx, h.ID, anthropic.MessageBatchResultsParams{})
		defer func() { _ = stream.Close() }()
		for stream.Next() {
			if !yield(a.batchResult(stream.Current()), nil) {
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(types.BatchResult{}, a.batchError(err))
		}
	}
}

// batchResult converts one results line.
func (a *Adapter) batchResult(r anthropic.MessageBatchIndividualResponse) types.BatchResult {
	out := types.BatchResult{CustomID: r.CustomID}
	switch r.Result.Type {
	case "succeeded":
		out.Outcome = types.BatchSucceeded
		m := r.Result.Message
		out.Message, out.Err = a.assistantFromMessage(m)
		out.FinishReason = string(m.StopReason)
		u := m.Usage
		prompt := int(u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens)
		out.Usage = types.UsageDelta{Cumulative: true, PromptTokens: prompt,
			CachedPromptTokens: int(u.CacheReadInputTokens), CacheWriteTokens: int(u.CacheCreationInputTokens),
			CompletionTokens: int(u.OutputTokens), TotalTokens: prompt + int(u.OutputTokens),
			ResponseID: m.ID, ResponseModel: string(m.Model), FinishReasons: []string{out.FinishReason}}
		switch {
		case out.Err != nil:
		case types.IsContentFilterFinishReason(out.FinishReason):
			out.Err = streamcheck.Refused("anthropic", string(a.model), out.FinishReason)
		case out.FinishReason == stopPauseTurn:
			out.Err = &types.ProviderError{Provider: "anthropic", Model: string(a.model), Kind: types.ErrorKindPermanent, Err: errPausedTurn}
		case types.IsTruncationFinishReason(out.FinishReason):
			out.Err = &types.ProviderError{Provider: "anthropic", Model: string(a.model), Kind: types.ErrorKindTruncated,
				Err: fmt.Errorf("%w: stopped at %s", types.ErrResponseTruncated, out.FinishReason)}
		}
	case "errored":
		out.Outcome = types.BatchErrored
		e := r.Result.Error.Error
		out.Err = &types.BatchRequestError{Outcome: out.Outcome, Code: e.Type, Message: e.Message,
			Err: &types.ProviderError{Provider: "anthropic", Model: string(a.model), Kind: errorKind(e.Type), Err: errors.New(e.Message)}}
	case "canceled":
		out.Outcome = types.BatchCanceledOutcome
		out.Err = &types.BatchRequestError{Outcome: out.Outcome}
	default:
		out.Outcome = types.BatchExpiredOutcome
		out.Err = &types.BatchRequestError{Outcome: out.Outcome}
	}
	return out
}

// errorKind maps an API error type to an ErrorKind.
func errorKind(t string) types.ErrorKind {
	switch t {
	case "invalid_request_error", "not_found_error", "request_too_large":
		return types.ErrorKindInvalidRequest
	case "authentication_error", "permission_error":
		return types.ErrorKindAuth
	case "rate_limit_error":
		return types.ErrorKindRateLimit
	case "overloaded_error", "api_error":
		return types.ErrorKindUnavailable
	}
	return types.ErrorKindPermanent
}

// assistantFromMessage converts a complete message to the parts the
// streaming path emits for it: one part per content block in block order,
// then the citations of the text blocks, anchored to them, then a refusal.
// The hidden schema tool becomes the text answer.
func (a *Adapter) assistantFromMessage(m anthropic.Message) (types.AssistantMessage, error) {
	var out types.AssistantMessage
	var cited []types.CitationPart
	calls := map[string]types.ServerToolKind{}
	for _, b := range m.Content {
		switch {
		case b.Type == blockText:
			for _, c := range b.Citations {
				if cit, ok := citationFrom(c.RawJSON()); ok {
					cited = append(cited, anchored(cit, len(out.Parts), len(b.Text)))
				}
			}
			out.Parts = append(out.Parts, types.TextPart{Text: b.Text})
		case b.Type == blockThinking:
			out.Parts = append(out.Parts, types.ThinkingPart{Text: b.Thinking, Signature: b.Signature})
		case b.Type == blockRedactedThinking:
			out.Parts = append(out.Parts, types.ThinkingPart{Redacted: true, Signature: b.Data})
		case b.Type == blockToolUse:
			if b.Name == structuredToolName {
				out.Parts = append(out.Parts, types.TextPart{Text: string(b.Input)})
				continue
			}
			args, err := streamcheck.DecodeArguments(string(b.Input))
			tu := types.ToolCallPart{ID: b.ID, Name: b.Name, Arguments: args}
			if err != nil {
				tu.ArgumentsError = err.Error()
			}
			out.Parts = append(out.Parts, tu)
		case b.Type == blockServerToolUse:
			input, _ := streamcheck.DecodeArguments(string(b.Input))
			calls[b.ID] = serverToolKind(b.Name)
			out.Parts = append(out.Parts, types.ServerToolCallPart{ID: b.ID, ToolKind: calls[b.ID], Name: b.Name, Input: input})
		case isServerToolResult(b.Type) && b.ToolUseID != "":
			out.Parts = append(out.Parts, a.serverToolResult(b.RawJSON(), serverResultKind(b.Type, b.ToolUseID, calls)))
		}
	}
	for _, c := range cited {
		out.Parts = append(out.Parts, c)
	}
	if string(m.StopReason) == stopRefusal {
		out.Parts = append(out.Parts, refusalPart(m.StopDetails))
	}
	return out, nil
}

// Cancel implements types.BatchProvider.
func (a *Adapter) Cancel(ctx context.Context, h types.BatchHandle) error {
	if _, err := a.client.Messages.Batches.Cancel(ctx, h.ID, anthropic.MessageBatchCancelParams{}); err != nil {
		return a.batchError(err)
	}
	return nil
}

// FindBatch implements types.BatchFinder. Message Batches carry no
// metadata, so batches created since q.Since with q.Count requests are
// candidates. An ended candidate is checked by reading its first result: our
// request IDs are unique to the job. A running candidate cannot be checked
// until it ends, so it makes the lookup ambiguous.
func (a *Adapter) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	pager := a.client.Messages.Batches.ListAutoPaging(ctx, anthropic.MessageBatchListParams{Limit: anthropic.Int(100)})
	ambiguous := false
	for pager.Next() {
		mb := pager.Current()
		if mb.CreatedAt.Before(q.Since) {
			break
		}
		st := batchStatus(&mb)
		if st.Counts.Total != q.Count {
			continue
		}
		if !st.State.HasResults() {
			ambiguous = true
			continue
		}
		h := types.BatchHandle{Provider: a.Name(), Model: a.Model(), ID: mb.ID}
		for res, err := range a.Results(ctx, h) {
			if err != nil {
				return types.BatchHandle{}, false, err
			}
			if slices.Contains(q.CustomIDs, res.CustomID) {
				return h, true, nil
			}
			break
		}
	}
	if err := pager.Err(); err != nil {
		return types.BatchHandle{}, false, a.batchError(err)
	}
	if ambiguous {
		return types.BatchHandle{}, false, fmt.Errorf("%w: a batch of %d requests created after %s is still running",
			types.ErrBatchAmbiguous, q.Count, q.Since.UTC().Format(time.RFC3339))
	}
	return types.BatchHandle{}, false, nil
}

// batchError classifies an error from a batch endpoint. A 404 means the
// batch is unknown.
func (a *Adapter) batchError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
		return fmt.Errorf("%w: %w", types.ErrBatchNotFound, err)
	}
	return classifyAnthropicError(string(a.model), err, true)
}
