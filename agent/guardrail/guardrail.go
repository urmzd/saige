// Package guardrail provides built-in guardrails for agent.InputGuardrail and
// agent.OutputGuardrail: sensitive-data detection with the privacy
// detectors, a length limit, a JSON schema check, and a model classifier.
//
// Guardrails check the user's message and the final answer. They do not see
// tool calls: a ToolGate decides those, and a ToolRedactor keeps personal
// data out of tool traffic.
package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/types"
)

// Detect returns a guardrail that finds sensitive spans with d. With redact
// false it blocks text in which d finds anything; with redact true it
// rewrites each span to [REDACTED:LABEL] and lets the run go on. A nil d uses
// privacy.DefaultDetector, which finds personal data such as emails, phone
// numbers and card numbers. Build a regular-expression guardrail with
// privacy.NewRegexDetector, or Regex.
//
// A rewrite cannot be reversed. To let tools receive the real values, use a
// privacy.Vault through a ToolRedactor or privacy.Provider instead.
func Detect(name string, d privacy.Detector, redact bool) agent.Guardrail {
	if d == nil {
		d = privacy.DefaultDetector()
	}
	return agent.NewGuardrail(name, func(ctx context.Context, in agent.GuardrailInput) (agent.GuardrailVerdict, error) {
		spans, err := d.Detect(ctx, in.Text)
		if err != nil {
			return agent.GuardrailVerdict{}, err
		}
		if len(spans) == 0 {
			return agent.Pass(), nil
		}
		labels := spanLabels(spans)
		if !redact {
			return agent.Block("found " + labels), nil
		}
		text, err := privacy.Redact(ctx, d, in.Text)
		if err != nil {
			return agent.GuardrailVerdict{}, err
		}
		return agent.Rewrite(text, "redacted "+labels), nil
	})
}

// PII is Detect with the default personal-data detector, named "pii".
func PII(redact bool) agent.Guardrail { return Detect("pii", nil, redact) }

// Regex is Detect over patterns compiled into one privacy.RegexDetector.
func Regex(name string, redact bool, patterns ...privacy.Pattern) (agent.Guardrail, error) {
	d, err := privacy.NewRegexDetector(patterns...)
	if err != nil {
		return nil, err
	}
	return Detect(name, d, redact), nil
}

func spanLabels(spans []privacy.Span) string {
	var labels []string
	for _, s := range spans {
		if !slices.Contains(labels, s.Label) {
			labels = append(labels, s.Label)
		}
	}
	return strings.Join(labels, ", ")
}

// MaxLength returns a guardrail, named "max_length", that blocks text longer
// than n characters.
func MaxLength(n int) agent.Guardrail {
	return agent.NewGuardrail("max_length", func(_ context.Context, in agent.GuardrailInput) (agent.GuardrailVerdict, error) {
		if count := utf8.RuneCountInString(in.Text); count > n {
			return agent.Block(fmt.Sprintf("%d characters exceed the limit of %d", count, n)), nil
		}
		return agent.Pass(), nil
	})
}

// JSONSchema returns a guardrail, named "json_schema", that blocks text that
// is not JSON matching schema. A JSON value inside a fenced block or
// surrounding prose is found the way agent.ExtractJSON finds it.
func JSONSchema(schema types.ParameterSchema) agent.Guardrail {
	return Structured("json_schema", func(_ context.Context, value any) error {
		return types.ValidateJSON(schema, value)
	})
}

// Structured returns a guardrail that decodes the text as JSON and blocks it
// when it does not decode or check rejects the value. The block reason is
// the error.
func Structured(name string, check func(context.Context, any) error) agent.Guardrail {
	return agent.NewGuardrail(name, func(ctx context.Context, in agent.GuardrailInput) (agent.GuardrailVerdict, error) {
		raw, err := agent.ExtractJSON(in.Text)
		if err != nil {
			return agent.Block(err.Error()), nil
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return agent.Block("invalid JSON: " + err.Error()), nil
		}
		if err := check(ctx, value); err != nil {
			return agent.Block(err.Error()), nil
		}
		return agent.Pass(), nil
	})
}
