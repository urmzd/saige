package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

// Batch API limits, from the API documentation.
const (
	batchMaxRequests = 50_000
	batchMaxBytes    = 200 << 20
	// batchTagKey is the metadata key that carries BatchSubmitOptions.Tag.
	batchTagKey = "saige_tag"
	// metaEndpoint names the handle field that records the batch endpoint.
	metaEndpoint = "endpoint"
	// finishContentFilter is the normalized finish reason of a refusal.
	finishContentFilter = "content_filter"
)

var (
	_ types.BatchProvider = (*Adapter)(nil)
	_ types.BatchFinder   = (*Adapter)(nil)
	_ types.BatchProvider = (*ResponsesAdapter)(nil)
	_ types.BatchFinder   = (*ResponsesAdapter)(nil)
)

// batchLine is one line of a batch input file.
type batchLine struct {
	CustomID string `json:"custom_id"`
	Method   string `json:"method"`
	URL      string `json:"url"`
	Body     any    `json:"body"`
}

// Submit implements types.BatchProvider with the Batch API on
// /v1/chat/completions. Each request is validated and encoded as
// ChatStreamWithOptions (or, with a schema, ChatStreamWithSchema) would
// encode it, without the stream options, so an option the model rejects
// fails here, before the input file is uploaded.
func (a *Adapter) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	lines := make([]batchLine, len(reqs))
	for i, r := range reqs {
		params, err := a.batchParams(r)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", r.CustomID, err)
		}
		lines[i] = batchLine{CustomID: r.CustomID, Method: http.MethodPost, URL: string(openai.BatchNewParamsEndpointV1ChatCompletions), Body: params}
	}
	return a.submitBatch(ctx, reqs, lines, openai.BatchNewParamsEndpointV1ChatCompletions, opts)
}

// batchParams builds the Chat Completions body of one request.
func (a *Adapter) batchParams(r types.BatchRequest) (openai.ChatCompletionNewParams, error) {
	var params openai.ChatCompletionNewParams
	c, err := a.withRequestOptions(r.Options.Raw())
	if err != nil {
		return params, err
	}
	if c, err = c.compileDials(r.Options, r.Tools, r.Schema != nil, types.SurfaceChat); err != nil {
		return params, err
	}
	if err := c.Validate(); err != nil {
		return params, err
	}
	if err := c.Capabilities().ValidateRequest(r.Tools, r.Schema != nil); err != nil {
		return params, err
	}
	if err := c.checkToolChoice(r.Tools); err != nil {
		return params, err
	}
	effortNone, err := c.checkChatTools(r.Tools)
	if err != nil {
		return params, err
	}
	params = openai.ChatCompletionNewParams{Model: c.model, Messages: toOpenAIMessages(r.Messages)}
	c.applyParams(&params)
	if err := c.applyPromptCache(&params); err != nil {
		return params, err
	}
	if tools := toOpenAITools(r.Tools); len(tools) > 0 {
		params.Tools = tools
		c.applyToolChoice(&params)
		if effortNone {
			params.ReasoningEffort = shared.ReasoningEffort("none")
		}
	}
	if r.Schema != nil {
		schemaMap, strict := responseSchema(*r.Schema)
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name: "response", Schema: schemaMap, Strict: openai.Bool(strict)}},
		}
	}
	return params, nil
}

// Submit implements types.BatchProvider with the Batch API on
// /v1/responses. Requests are validated as ChatStreamWithOptions validates
// them, including the controls the Responses API does not have.
func (r *ResponsesAdapter) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	lines := make([]batchLine, len(reqs))
	for i, q := range reqs {
		base, err := r.base.withRequestOptions(q.Options.Raw())
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", q.CustomID, err)
		}
		if base, err = base.compileDials(q.Options, q.Tools, q.Schema != nil, types.SurfaceResponses); err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", q.CustomID, err)
		}
		c := &ResponsesAdapter{base: *base}
		params, err := c.buildParams(q.Messages, q.Tools, q.Schema)
		if err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: %w", q.CustomID, err)
		}
		lines[i] = batchLine{CustomID: q.CustomID, Method: http.MethodPost, URL: string(openai.BatchNewParamsEndpointV1Responses), Body: params}
	}
	return r.base.submitBatch(ctx, reqs, lines, openai.BatchNewParamsEndpointV1Responses, opts)
}

// submitBatch uploads the input file and creates the batch.
func (a *Adapter) submitBatch(ctx context.Context, reqs []types.BatchRequest, lines []batchLine, endpoint openai.BatchNewParamsEndpoint, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	caps := a.Capabilities()
	if err := batch.CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	if len(reqs) > batchMaxRequests {
		return types.BatchHandle{}, caps.OptionError("batch", fmt.Sprintf("at most %d requests per batch", batchMaxRequests))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %s: encode: %w", l.CustomID, err)
		}
	}
	if buf.Len() > batchMaxBytes {
		return types.BatchHandle{}, caps.OptionError("batch", "a batch input file is limited to 200 MB")
	}
	model := string(a.model)
	file, err := a.client.Files.New(ctx, openai.FileNewParams{
		File:    openai.File(bytes.NewReader(buf.Bytes()), "batch.jsonl", "application/jsonl"),
		Purpose: openai.FilePurposeBatch,
	})
	if err != nil {
		// No batch exists until the create call, so a failed upload is
		// definite whatever its cause.
		return types.BatchHandle{}, fmt.Errorf("%w: upload batch input: %w", batch.ErrNotSubmitted, classifyOpenAIError(model, err, true))
	}
	params := openai.BatchNewParams{InputFileID: file.ID, Endpoint: endpoint, CompletionWindow: openai.BatchNewParamsCompletionWindow24h}
	if opts.Tag != "" {
		params.Metadata = shared.Metadata{batchTagKey: opts.Tag}
	}
	b, err := a.client.Batches.New(ctx, params)
	if err != nil {
		return types.BatchHandle{}, classifyOpenAIError(model, err, true)
	}
	return types.BatchHandle{Provider: providerName, Model: model, ID: b.ID,
		Meta: map[string]string{metaEndpoint: string(endpoint)}}, nil
}

// Status implements types.BatchProvider.
func (a *Adapter) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	b, err := a.client.Batches.Get(ctx, h.ID)
	if err != nil {
		return types.BatchStatus{}, a.batchError(err)
	}
	return openAIBatchStatus(b), nil
}

// Status implements types.BatchProvider.
func (r *ResponsesAdapter) Status(ctx context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	return r.base.Status(ctx, h)
}

func unix(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

func openAIBatchStatus(b *openai.Batch) types.BatchStatus {
	c := b.RequestCounts
	st := types.BatchStatus{VendorState: string(b.Status), CreatedAt: unix(b.CreatedAt), ExpiresAt: unix(b.ExpiresAt),
		Counts: types.BatchCounts{Total: int(c.Total), Succeeded: int(c.Completed), Errored: int(c.Failed)}}
	st.Counts.Processing = max(0, st.Counts.Total-st.Counts.Succeeded-st.Counts.Errored)
	switch b.Status {
	case openai.BatchStatusValidating:
		st.State = types.BatchPending
	case openai.BatchStatusInProgress, openai.BatchStatusFinalizing:
		st.State = types.BatchRunning
	case openai.BatchStatusCancelling:
		st.State = types.BatchCanceling
	case openai.BatchStatusCompleted:
		st.State, st.EndedAt = types.BatchEnded, unix(b.CompletedAt)
	case openai.BatchStatusExpired:
		st.State, st.EndedAt = types.BatchExpired, unix(b.ExpiredAt)
	case openai.BatchStatusCancelled:
		st.State, st.EndedAt = types.BatchCanceled, unix(b.CancelledAt)
	case openai.BatchStatusFailed:
		st.State, st.EndedAt = types.BatchFailed, unix(b.FailedAt)
		for _, e := range b.Errors.Data {
			if st.Error != "" {
				st.Error += "; "
			}
			st.Error += e.Code + ": " + e.Message
		}
	default:
		st.State = types.BatchRunning
	}
	if st.State.Terminal() {
		st.Counts.Processing = 0
	}
	return st
}

// Results implements types.BatchProvider. It reads the output file and then
// the error file; lines in each arrive in any order.
func (a *Adapter) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return a.batchResults(ctx, h)
}

// Results implements types.BatchProvider.
func (r *ResponsesAdapter) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return r.base.batchResults(ctx, h)
}

func (a *Adapter) batchResults(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		b, err := a.client.Batches.Get(ctx, h.ID)
		if err != nil {
			yield(types.BatchResult{}, a.batchError(err))
			return
		}
		responses := h.Meta[metaEndpoint] == string(openai.BatchNewParamsEndpointV1Responses) ||
			b.Endpoint == string(openai.BatchNewParamsEndpointV1Responses)
		expired := b.Status == openai.BatchStatusExpired
		canceled := b.Status == openai.BatchStatusCancelled
		for _, fileID := range []string{b.OutputFileID, b.ErrorFileID} {
			if fileID == "" {
				continue
			}
			if !a.readResultFile(ctx, fileID, responses, expired, canceled, yield) {
				return
			}
		}
	}
}

// readResultFile streams one output or error file. It returns false when
// the consumer stopped or an error ended the stream.
func (a *Adapter) readResultFile(ctx context.Context, fileID string, responses, expired, canceled bool, yield func(types.BatchResult, error) bool) bool {
	resp, err := a.client.Files.Content(ctx, fileID)
	if err != nil {
		yield(types.BatchResult{}, a.batchError(err))
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		res, err := a.decodeResultLine(line, responses, expired, canceled)
		if !yield(res, err) || err != nil {
			return false
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		yield(types.BatchResult{}, fmt.Errorf("openai: read batch results: %w", err))
		return false
	}
	return true
}

// outputLine is one line of an output or error file.
type outputLine struct {
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Adapter) decodeResultLine(line []byte, responses, expired, canceled bool) (types.BatchResult, error) {
	var l outputLine
	if err := json.Unmarshal(line, &l); err != nil {
		return types.BatchResult{}, fmt.Errorf("openai: decode batch result: %w", err)
	}
	res := types.BatchResult{CustomID: l.CustomID}
	model := string(a.model)
	switch {
	case l.Response != nil && l.Response.StatusCode == http.StatusOK:
		res.Outcome = types.BatchSucceeded
		var err error
		if responses {
			err = decodeResponsesBody(l.Response.Body, &res)
		} else {
			err = decodeChatBody(l.Response.Body, &res)
		}
		if err != nil {
			return types.BatchResult{}, fmt.Errorf("openai: decode batch result %s: %w", l.CustomID, err)
		}
		switch {
		case types.IsContentFilterFinishReason(res.FinishReason):
			res.Err = streamcheck.Refused(providerName, model, res.FinishReason)
		case types.IsTruncationFinishReason(res.FinishReason):
			res.Err = &types.ProviderError{Provider: providerName, Model: model, Kind: types.ErrorKindTruncated,
				Err: fmt.Errorf("%w: stopped at %s", types.ErrResponseTruncated, res.FinishReason)}
		}
	case l.Response != nil:
		var body struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(l.Response.Body, &body)
		res.Outcome = types.BatchErrored
		res.Err = &types.BatchRequestError{Outcome: res.Outcome, Code: body.Error.Code, Message: body.Error.Message,
			Err: streamcheck.HTTPError(providerName, model, l.Response.StatusCode, nil, body.Error.Message, errors.New(body.Error.Message))}
	default:
		res.Outcome = types.BatchErrored
		code, msg := "", ""
		if l.Error != nil {
			code, msg = l.Error.Code, l.Error.Message
		}
		switch {
		case code == "batch_expired" || (expired && code == ""):
			res.Outcome = types.BatchExpiredOutcome
		case code == "batch_cancelled" || (canceled && code == ""):
			res.Outcome = types.BatchCanceledOutcome
		}
		res.Err = &types.BatchRequestError{Outcome: res.Outcome, Code: code, Message: msg}
	}
	return res, nil
}

// chatBody is the part of a Chat Completions response a batch result needs.
type chatBody struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   *string `json:"content"`
			Refusal   *string `json:"refusal"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func decodeChatBody(raw json.RawMessage, res *types.BatchResult) error {
	var b chatBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	if len(b.Choices) > 0 {
		ch := b.Choices[0]
		res.FinishReason = ch.FinishReason
		if ch.Message.Content != nil && *ch.Message.Content != "" {
			res.Message.Content = append(res.Message.Content, types.TextContent{Text: *ch.Message.Content})
		}
		if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
			res.FinishReason = finishContentFilter
			res.Message.Content = append(res.Message.Content, types.TextContent{Text: *ch.Message.Refusal})
		}
		for _, tc := range ch.Message.ToolCalls {
			args, err := streamcheck.DecodeArguments(tc.Function.Arguments)
			tu := types.ToolUseContent{ID: tc.ID, Name: tc.Function.Name, Arguments: args}
			if err != nil {
				tu.ArgumentsError = err.Error()
			}
			res.Message.Content = append(res.Message.Content, tu)
		}
	}
	u := b.Usage
	res.Usage = types.UsageDelta{Cumulative: true, PromptTokens: u.PromptTokens, CachedPromptTokens: u.PromptTokensDetails.CachedTokens,
		CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, ResponseID: b.ID, ResponseModel: b.Model}
	if res.FinishReason != "" {
		res.Usage.FinishReasons = []string{res.FinishReason}
	}
	return nil
}

// responsesBody is the part of a Responses API response a batch result needs.
type responsesBody struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		TotalTokens        int `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func decodeResponsesBody(raw json.RawMessage, res *types.BatchResult) error {
	var b responsesBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	res.FinishReason = finishStop
	if b.IncompleteDetails != nil {
		switch b.IncompleteDetails.Reason {
		case incompleteOutputTokens:
			res.FinishReason = finishLength
		case finishContentFilter:
			res.FinishReason = finishContentFilter
		}
	}
	for _, item := range b.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				switch c.Type {
				case "output_text":
					res.Message.Content = append(res.Message.Content, types.TextContent{Text: c.Text})
				case "refusal":
					res.FinishReason = finishContentFilter
					res.Message.Content = append(res.Message.Content, types.TextContent{Text: c.Refusal})
				}
			}
		case itemFunctionCall:
			args, err := streamcheck.DecodeArguments(item.Arguments)
			tu := types.ToolUseContent{ID: item.CallID, Name: item.Name, Arguments: args}
			if err != nil {
				tu.ArgumentsError = err.Error()
			}
			res.Message.Content = append(res.Message.Content, tu)
		}
	}
	u := b.Usage
	res.Usage = types.UsageDelta{Cumulative: true, PromptTokens: u.InputTokens, CachedPromptTokens: u.InputTokensDetails.CachedTokens,
		CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens, ResponseID: b.ID, ResponseModel: b.Model,
		FinishReasons: []string{res.FinishReason}}
	return nil
}

// Cancel implements types.BatchProvider.
func (a *Adapter) Cancel(ctx context.Context, h types.BatchHandle) error {
	if _, err := a.client.Batches.Cancel(ctx, h.ID); err != nil {
		return a.batchError(err)
	}
	return nil
}

// Cancel implements types.BatchProvider.
func (r *ResponsesAdapter) Cancel(ctx context.Context, h types.BatchHandle) error {
	return r.base.Cancel(ctx, h)
}

// FindBatch implements types.BatchFinder by listing batches newest first
// and matching the tag in their metadata. The list endpoint has no
// metadata filter, so it pages back to q.Since.
func (a *Adapter) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	if q.Tag == "" {
		return types.BatchHandle{}, false, fmt.Errorf("%w: lookup needs a tag", types.ErrBatchAmbiguous)
	}
	pager := a.client.Batches.ListAutoPaging(ctx, openai.BatchListParams{Limit: openai.Int(100)})
	for pager.Next() {
		b := pager.Current()
		if unix(b.CreatedAt).Before(q.Since) {
			break
		}
		if b.Metadata[batchTagKey] == q.Tag {
			return types.BatchHandle{Provider: providerName, Model: string(a.model), ID: b.ID,
				Meta: map[string]string{metaEndpoint: b.Endpoint}}, true, nil
		}
	}
	if err := pager.Err(); err != nil {
		return types.BatchHandle{}, false, a.batchError(err)
	}
	return types.BatchHandle{}, false, nil
}

// FindBatch implements types.BatchFinder.
func (r *ResponsesAdapter) FindBatch(ctx context.Context, q types.BatchQuery) (types.BatchHandle, bool, error) {
	return r.base.FindBatch(ctx, q)
}

// batchError classifies an error from a batch endpoint. A 404 means the
// batch is unknown.
func (a *Adapter) batchError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %w", types.ErrBatchNotFound, err)
	}
	return classifyOpenAIError(string(a.model), err, true)
}
