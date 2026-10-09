package harness

import (
	"context"
	"fmt"
)

// Validator checks one model answer. A nil error accepts it; an error
// rejects it and its message is shown to the model in the repair prompt.
type Validator func(text string) error

// RepairOptions configures [Client.ChatWithRepair].
type RepairOptions struct {
	// MaxRepairs is the number of repair calls allowed after the first
	// answer fails validation. Zero means no repair: one call, validated.
	MaxRepairs int
	// Prompt builds the user message that asks for a fix, from the rejected
	// answer and the validation error. Nil uses [DefaultRepairPrompt].
	Prompt func(text string, err error) string
	// ChatOptions apply to every call of the loop.
	ChatOptions []ChatOption
}

// DefaultRepairPrompt asks the model to fix its previous answer and return
// only the corrected output.
func DefaultRepairPrompt(_ string, err error) string {
	return fmt.Sprintf("Your previous answer was rejected: %v\n\nReturn the complete corrected answer, raw, with no commentary.", err)
}

// RepairResult is the outcome of [Client.ChatWithRepair]. Result sums the
// usage of every call (see [ChatResult.Add]) and carries the last answer in
// Text. Repairs counts the repair calls made after the first. Valid reports
// whether the last answer passed validation; when it is false, Err holds the
// last validation error. Errors lists every validation error in order.
type RepairResult struct {
	Result  ChatResult
	Repairs int
	Valid   bool
	Err     error
	Errors  []string
}

// ApplyTo records the loop on a turn: RepairAttempts, and for an answer that
// never validated, Failed with a FailureReason starting with
// "validation failed" and ValidationError, which [ComputeReliability]
// counts as a validation failure.
func (r RepairResult) ApplyTo(turn *TurnResult) {
	turn.RepairAttempts = r.Repairs
	if r.Valid || r.Err == nil {
		return
	}
	msg := Truncate(r.Err.Error(), maxErrorBody)
	reason := "validation failed: " + msg
	turn.Failed = true
	turn.FailureReason = &reason
	turn.ValidationError = &msg
}

// ChatWithRepair sends messages, validates the answer, and while validation
// fails and repairs remain, appends the rejected answer and a repair request
// to the conversation and asks again. A chat error ends the loop and is
// returned with the result accumulated so far; a final answer that still
// fails validation is not an error, it is reported through Valid and Err.
// A nil validate accepts every answer.
func (c *Client) ChatWithRepair(ctx context.Context, messages []Message, validate Validator, opts RepairOptions) (RepairResult, error) {
	prompt := opts.Prompt
	if prompt == nil {
		prompt = DefaultRepairPrompt
	}
	convo := append([]Message(nil), messages...)
	var out RepairResult
	for attempt := 0; ; attempt++ {
		result, err := c.Chat(ctx, convo, opts.ChatOptions...)
		if err != nil {
			return out, err
		}
		if attempt == 0 {
			out.Result = result
		} else {
			out.Result = out.Result.Add(result)
		}
		out.Repairs = attempt
		var verr error
		if validate != nil {
			verr = validate(result.Text)
		}
		if verr == nil {
			out.Valid, out.Err = true, nil
			return out, nil
		}
		out.Err = verr
		out.Errors = append(out.Errors, verr.Error())
		if attempt >= opts.MaxRepairs {
			return out, nil
		}
		convo = append(convo,
			Message{Role: roleAssistant, Content: result.Text},
			Message{Role: roleUser, Content: prompt(result.Text, verr)},
		)
	}
}
