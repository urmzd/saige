// Package streamcheck holds the stream-integrity rules shared by the provider
// adapters: decoding streamed tool-call arguments, reporting a response that
// stopped at the output token limit, and classifying the error that ended a
// stream. Keeping them in one place stops the adapters from drifting apart on
// what counts as a failed or truncated response.
package streamcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ErrIncompleteStream reports a stream that ended without the provider's
// finish marker. It wraps io.ErrUnexpectedEOF, so the transport classifier
// treats it like a dropped connection.
var ErrIncompleteStream = fmt.Errorf("stream ended before the provider finished the response: %w", io.ErrUnexpectedEOF)

// DecodeArguments decodes the streamed JSON arguments of one tool call.
// Empty input and a JSON null both mean "no arguments" and return an empty,
// non-nil map. Anything that is not a complete JSON object is an error: a
// tool must never run with arguments the model did not finish writing.
func DecodeArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// ArgsFailure records the first tool call, or structured-output block, whose
// JSON did not decode. The adapter holds it until the stream ends, because
// the finish reason that says whether the output was cut short arrives after
// the block closes.
type ArgsFailure struct {
	ToolCallID string
	Name       string
	Err        error
}

// Set records f unless a failure is already held. It reports whether f was
// recorded.
func (f *ArgsFailure) Set(id, name string, err error) bool {
	if f.Err != nil {
		return false
	}
	f.ToolCallID, f.Name, f.Err = id, name, err
	return true
}

// Failed reports whether a failure is held.
func (f *ArgsFailure) Failed() bool { return f.Err != nil }

// Error builds the terminal error for a held failure. When finishReason says
// the output token limit stopped the response, the error is a truncation
// (errors.Is(err, types.ErrResponseTruncated)). Otherwise the model wrote
// malformed JSON and the error is permanent.
func (f *ArgsFailure) Error(provider, model, finishReason string, outputTokens, maxTokens int) *types.ProviderError {
	if types.IsTruncationFinishReason(finishReason) {
		return Truncated(provider, model, finishReason, outputTokens, maxTokens)
	}
	what := "structured output"
	if f.Name != "" {
		what = fmt.Sprintf("tool call %q", f.Name)
		if f.ToolCallID != "" {
			what = fmt.Sprintf("tool call %q (%s)", f.Name, f.ToolCallID)
		}
	}
	return &types.ProviderError{
		Provider: provider,
		Model:    model,
		Kind:     types.ErrorKindPermanent,
		Err:      fmt.Errorf("%s: invalid arguments JSON: %w", what, f.Err),
	}
}

// Truncated builds the error for a response the output token limit cut short.
// The finish reason is normalized to types.FinishReasonMaxTokens.
func Truncated(provider, model, finishReason string, outputTokens, maxTokens int) *types.ProviderError {
	return &types.ProviderError{
		Provider: provider,
		Model:    model,
		Kind:     types.ErrorKindTruncated,
		Err: &types.ResponseTruncatedError{
			FinishReason:    types.NormalizeFinishReason(finishReason),
			OutputTokens:    outputTokens,
			MaxOutputTokens: maxTokens,
		},
	}
}

// Refused reports a response the provider's safety system stopped, or the
// model refused, as finishReason says. It is a content-filter error, so
// retry does not resend it and a router may move it to another provider.
func Refused(provider, model, finishReason string) *types.ProviderError {
	return &types.ProviderError{
		Provider: provider,
		Model:    model,
		Kind:     types.ErrorKindContentFilter,
		Err:      fmt.Errorf("response stopped by the provider (finish reason %s)", finishReason),
	}
}

// StreamError wraps an error that is not an HTTP API error: a transport
// failure, an early EOF, or a malformed stream. Before any output reached the
// consumer, a recognized network failure is transient, so retry and failover
// can act on it. After output, the error is permanent: a retry would repeat
// text the consumer has already seen. Caller cancellation is never transient.
func StreamError(provider, model string, err error, beforeOutput bool) *types.ProviderError {
	kind := types.ErrorKindPermanent
	if k, ok := types.ClassifyErrorMessage(err.Error()); ok {
		kind = k
	} else if beforeOutput {
		if k, ok := types.ClassifyTransportError(err); ok {
			kind = k
		}
	}
	return &types.ProviderError{Provider: provider, Model: model, Kind: kind, Err: err}
}

// HTTPError builds the error for a non-2xx API response. The status sets the
// kind, the message refines it (a 400 that names the context window is
// ErrorKindContextLength), and Retry-After headers set RetryAfter.
func HTTPError(provider, model string, code int, header http.Header, message string, err error) *types.ProviderError {
	kind, delay := types.ClassifyHTTPError(code, header, message)
	return &types.ProviderError{
		Provider:   provider,
		Model:      model,
		Kind:       kind,
		Code:       code,
		Err:        err,
		RetryAfter: delay,
	}
}

// eventKinds maps the error type names that Anthropic and OpenAI put in a
// streamed error event to a kind. Both APIs send these after the HTTP status
// has already been 200, so the status code cannot classify them.
var eventKinds = map[string]types.ErrorKind{
	"overloaded_error":      types.ErrorKindUnavailable,
	"api_error":             types.ErrorKindUnavailable,
	"server_error":          types.ErrorKindUnavailable,
	"service_unavailable":   types.ErrorKindUnavailable,
	"timeout_error":         types.ErrorKindTransient,
	"rate_limit_error":      types.ErrorKindRateLimit,
	"rate_limit_exceeded":   types.ErrorKindRateLimit,
	"authentication_error":  types.ErrorKindAuth,
	"permission_error":      types.ErrorKindAuth,
	"invalid_api_key":       types.ErrorKindAuth,
	"invalid_request_error": types.ErrorKindInvalidRequest,
	"not_found_error":       types.ErrorKindInvalidRequest,
	"request_too_large":     types.ErrorKindInvalidRequest,
}

// EventError classifies an error event received inside a stream. payload is
// the event's JSON; it may be the full event ({"type":"error","error":{...}})
// or the inner error object. The message refines the type, so an
// invalid_request_error about the context window is ErrorKindContextLength.
func EventError(provider, model, payload string, err error) *types.ProviderError {
	kind := types.ErrorKindPermanent
	var body struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &body) == nil {
		names := []string{body.Type, fmt.Sprint(body.Code)}
		if body.Error != nil {
			names = []string{body.Error.Type, fmt.Sprint(body.Error.Code)}
		}
		for _, name := range names {
			if k, ok := eventKinds[name]; ok {
				kind = k
				break
			}
		}
	}
	if k, ok := types.ClassifyErrorMessage(payload); ok && !kind.Transient() {
		kind = k
	}
	if err == nil {
		err = errors.New(payload)
	}
	return &types.ProviderError{Provider: provider, Model: model, Kind: kind, Err: err}
}
