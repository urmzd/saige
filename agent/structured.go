package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ErrSchemaInvalid means a structured answer still failed to parse or
// validate after every repair attempt.
var ErrSchemaInvalid = errors.New("structured output did not match the schema")

// OutputSpec describes the typed answer Structured asks for.
type OutputSpec[T any] struct {
	// Schema constrains the answer. nil derives it from T with
	// types.SchemaFrom, which needs T to be a struct.
	Schema *types.ParameterSchema
	// Mode selects how the schema reaches the model. See OutputMode.
	Mode OutputMode
	// Repair is how many times an answer that fails to parse or validate is
	// sent back to the model with the error. 0 never re-asks.
	Repair int
	// Extract pulls the JSON out of a text answer. nil uses ExtractJSON,
	// which tolerates code fences, prose, and <think> blocks.
	Extract func(string) (string, error)
	// Validate checks the decoded value beyond the schema. Its error is
	// sent back to the model like a schema error.
	Validate func(T) error
	// OnDelta receives every delta of every attempt, in order.
	OnDelta func(types.Delta)
}

// StructuredResult reports how Structured reached its answer.
type StructuredResult struct {
	// JSON is the accepted answer.
	JSON json.RawMessage
	// Mode is the output mode used.
	Mode OutputMode
	// Attempts counts runs, including repairs.
	Attempts int
	// Switches lists the model changes the OutcomePolicy made.
	Switches []types.Switch
	// Transcripts holds one transcript per attempt.
	Transcripts []Transcript
}

// Structured runs a on input and returns the answer decoded into T.
//
// The answer is extracted from the final text, or taken from the
// final_answer tool in OutputTool mode, then checked against the schema and
// spec.Validate. A failure is sent back to the model with the error, up to
// spec.Repair times. When the attempts run out, the agent's OutcomePolicy
// sees types.OutcomeSchemaInvalid; a switch it accepts is recorded in the
// tree and the repair attempts start again on the new model. The returned
// error then wraps ErrSchemaInvalid.
//
// All attempts extend the agent's active branch, so the model sees its
// earlier answers and the errors. A schema passed here applies to these runs
// only and replaces any WithResponseSchema for them.
func Structured[T any](ctx context.Context, a *Agent, input []types.Message, spec OutputSpec[T]) (T, *StructuredResult, error) {
	var zero T
	schema, err := outputSchema[T](spec.Schema)
	if err != nil {
		return zero, nil, err
	}
	mode, err := a.resolveOutputMode(spec.Mode, schema)
	if err != nil {
		return zero, nil, err
	}
	extract := spec.Extract
	if extract == nil {
		extract = ExtractJSON
	}
	repair := max(spec.Repair, 0)
	decode := func(raw json.RawMessage) (T, error) {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return v, fmt.Errorf("decode: %w", err)
		}
		if spec.Validate != nil {
			if err := spec.Validate(v); err != nil {
				return v, err
			}
		}
		return v, nil
	}
	out := &runOutput{agent: a, schema: schema, mode: mode, scoped: true}
	if spec.Validate != nil {
		out.check = func(raw json.RawMessage) error {
			_, err := decode(raw)
			return err
		}
	}
	ctx = withRunOutput(ctx, out)

	res := &StructuredResult{Mode: mode}
	messages := input
	failures := 0
	for {
		res.Attempts++
		stream := a.Invoke(ctx, messages)
		transcript, err := Collect(stream, spec.OnDelta)
		res.Transcripts = append(res.Transcripts, transcript)
		var (
			raw   json.RawMessage
			cause error
		)
		switch {
		case err == nil:
			raw, cause = a.structuredAnswer(stream, schema, extract)
		case mode == OutputTool && errors.Is(err, ErrToolErrorLimit):
			// Repeated invalid final_answer calls stop the run at the
			// tool error limit. That is a schema failure, so it is
			// repaired like one.
			cause = err
		default:
			return zero, res, err
		}
		if cause == nil {
			v, err := decode(raw)
			if err == nil {
				res.JSON = raw
				return v, res, nil
			}
			cause = err
		}
		failures++
		if failures <= repair {
			messages = []types.Message{repairMessage(mode, cause)}
			continue
		}
		if len(res.Switches) < maxOutcomeSwitches {
			o := types.Outcome{Kind: types.OutcomeSchemaInvalid, Model: a.branchModel(), Attempts: res.Attempts, Err: cause}
			sw, err := a.observeOutcome(ctx, spec.OnDelta, a.cfg.Tree, a.cfg.Tree.Active(), a.cfg.Provider, o)
			if err != nil {
				return zero, res, err
			}
			if sw != nil {
				res.Switches = append(res.Switches, *sw)
				failures = 0
				messages = []types.Message{repairMessage(mode, cause)}
				continue
			}
		}
		return zero, res, fmt.Errorf("%w after %d attempts: %w", ErrSchemaInvalid, res.Attempts, cause)
	}
}

// outputSchema returns the schema to enforce: the explicit one, or one
// derived from a struct T.
func outputSchema[T any](schema *types.ParameterSchema) (*types.ParameterSchema, error) {
	if schema != nil {
		return schema, nil
	}
	t := reflect.TypeFor[T]()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: structured output: OutputSpec.Schema is required for %s", types.ErrInvalidModelConfig, t)
	}
	derived := types.SchemaFrom[T]()
	return &derived, nil
}

// structuredAnswer reads the answer of the run behind stream from the tree:
// the result of the tool call that ended it, or the text of its final
// assistant turn. The JSON is checked against schema.
func (a *Agent) structuredAnswer(stream *EventStream, schema *types.ParameterSchema, extract func(string) (string, error)) (json.RawMessage, error) {
	msgs, err := a.cfg.Tree.FlattenBranch(stream.branch)
	if err != nil {
		return nil, err
	}
	text, err := answerText(msgs, stream.StopToolCallID())
	if err != nil {
		return nil, err
	}
	extracted, err := extract(text)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal([]byte(extracted), &value); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoJSON, err)
	}
	if err := types.ValidateJSON(*schema, value); err != nil {
		return nil, err
	}
	return json.RawMessage(extracted), nil
}

// answerText finds a run's answer in its branch.
func answerText(msgs []types.Message, stopCallID string) (string, error) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if stopCallID != "" {
			for _, r := range toolResultsOf(msgs[i]) {
				if r.ToolCallID == stopCallID {
					return r.Text, nil
				}
			}
			continue
		}
		switch m := msgs[i].(type) {
		case types.AssistantMessage:
			if len(assistantToolCalls(&m)) > 0 {
				return "", errors.New("the run ended without a final answer")
			}
			var text strings.Builder
			for _, block := range m.Content {
				if tc, ok := block.(types.TextContent); ok {
					text.WriteString(tc.Text)
				}
			}
			return text.String(), nil
		case types.SystemMessage:
			// ConfigContent and similar metadata can follow the answer.
			continue
		default:
			return "", errors.New("the run ended without a final answer")
		}
	}
	return "", errors.New("the run ended without a final answer")
}

// repairMessage asks the model to correct an answer that failed.
func repairMessage(mode OutputMode, cause error) types.Message {
	how := "Reply again with only a JSON value that matches the required schema."
	if mode == OutputTool {
		how = fmt.Sprintf("Call the %s tool again with arguments that match its schema.", FinalAnswerToolName)
	}
	return types.NewUserMessage(fmt.Sprintf("Your previous answer was not accepted: %v\n%s", cause, how))
}

// branchModel returns the model the active branch is configured to use.
func (a *Agent) branchModel() string {
	msgs, err := a.cfg.Tree.FlattenBranch(a.cfg.Tree.Active())
	if err == nil {
		if rc, _ := a.prepareMessages(msgs); rc.model != "" {
			return rc.model
		}
	}
	return types.ProviderModel(a.cfg.Provider)
}

// DecodeOutput decodes a sub-agent's selected output as JSON into T. Pair it
// with SubAgentDef.ResponseSchema, whose default result policy returns the
// validated JSON answer.
func DecodeOutput[T any](r SubAgentResult) (T, error) {
	var v T
	if r.Error != "" {
		return v, errors.New(r.Error)
	}
	if err := json.Unmarshal([]byte(r.Output), &v); err != nil {
		return v, fmt.Errorf("decode subagent %q output: %w", r.Name, err)
	}
	return v, nil
}
