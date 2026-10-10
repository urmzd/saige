package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
)

// maxEvalCases bounds one eval call, so a client cannot hand the server an
// unbounded scoring job.
const maxEvalCases = 1000

// evalCase is one observation as the eval tools accept it.
type evalCase struct {
	ID          string `json:"id"`
	Input       string `json:"input,omitempty"`
	Output      string `json:"output"`
	GroundTruth string `json:"ground_truth,omitempty"`
}

func (c evalCase) observation() (eval.Observation, error) {
	if c.ID == "" {
		return eval.Observation{}, errors.New("every case needs an id")
	}
	raw := func(s string) json.RawMessage {
		if s == "" {
			return nil
		}
		b, _ := json.Marshal(s)
		return b
	}
	return eval.Observation{ID: c.ID, Input: raw(c.Input), Output: raw(c.Output), GroundTruth: raw(c.GroundTruth)}, nil
}

// evalTools returns eval_run and eval_compare. Both score outputs the
// caller supplies with deterministic scorers from eval.DefaultRegistry; they
// call no model and write nothing.
func evalTools() []agenttypes.Tool {
	caseSchema := agenttypes.PropertyDef{
		Type:     agenttypes.SchemaObject,
		Required: []string{"id", "output"},
		Properties: map[string]agenttypes.PropertyDef{
			"id":           {Type: agenttypes.SchemaString, Description: "Case ID; the compare pairs cases by it"},
			"input":        {Type: agenttypes.SchemaString, Description: "The prompt or question"},
			"output":       {Type: agenttypes.SchemaString, Description: "The output to score"},
			"ground_truth": {Type: agenttypes.SchemaString, Description: "The expected output, for reference-based scorers"},
		},
	}
	cases := func(desc string) agenttypes.PropertyDef {
		return agenttypes.PropertyDef{Type: agenttypes.SchemaArray, Description: desc, Items: &caseSchema}
	}
	scorers := agenttypes.PropertyDef{
		Type:        agenttypes.SchemaArray,
		Description: "Scorers by kind, such as {\"kind\": \"token_f1\"} or {\"kind\": \"contains\", \"params\": {\"substrings\": [\"Paris\"]}}. Kinds: " + scorerKinds(),
		Items: &agenttypes.PropertyDef{
			Type:     agenttypes.SchemaObject,
			Required: []string{"kind"},
			Properties: map[string]agenttypes.PropertyDef{
				"kind":   {Type: agenttypes.SchemaString},
				"name":   {Type: agenttypes.SchemaString, Description: "Renames the metric"},
				"params": {Type: agenttypes.SchemaObject, Description: "Kind-specific parameters"},
			},
		},
	}

	run := &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{
			Name:        "eval_run",
			Description: "Score outputs with saige's deterministic eval scorers and return per-case scores and the aggregate.",
			Capability:  agenttypes.ToolCapabilityRead,
			Parameters: agenttypes.ParameterSchema{
				Type:     agenttypes.SchemaObject,
				Required: []string{"cases", "scorers"},
				Properties: map[string]agenttypes.PropertyDef{
					"name":    {Type: agenttypes.SchemaString, Description: "Suite name"},
					"cases":   cases("Cases to score"),
					"scorers": scorers,
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			var in struct {
				Name    string            `json:"name"`
				Cases   []evalCase        `json:"cases"`
				Scorers []eval.ScorerSpec `json:"scorers"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return "", err
			}
			suite, err := runSuite(ctx, in.Name, in.Cases, in.Scorers)
			if err != nil {
				return "", err
			}
			return encodeJSON(suite)
		},
	}

	compare := &agenttypes.ToolFunc{
		Def: agenttypes.ToolDef{
			Name: "eval_compare",
			Description: "Score a baseline and a candidate set of outputs with the same scorers and compare them case by case: " +
				"per-metric deltas with intervals, and which cases improved or regressed.",
			Capability: agenttypes.ToolCapabilityRead,
			Parameters: agenttypes.ParameterSchema{
				Type:     agenttypes.SchemaObject,
				Required: []string{"base", "candidate", "scorers"},
				Properties: map[string]agenttypes.PropertyDef{
					"name":      {Type: agenttypes.SchemaString, Description: "Comparison name"},
					"base":      cases("Baseline outputs"),
					"candidate": cases("Candidate outputs, paired with the baseline by id"),
					"scorers":   scorers,
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			var in struct {
				Name      string            `json:"name"`
				Base      []evalCase        `json:"base"`
				Candidate []evalCase        `json:"candidate"`
				Scorers   []eval.ScorerSpec `json:"scorers"`
			}
			if err := decodeArgs(args, &in); err != nil {
				return "", err
			}
			base, err := runSuite(ctx, "base", in.Base, in.Scorers)
			if err != nil {
				return "", fmt.Errorf("base: %w", err)
			}
			cand, err := runSuite(ctx, "candidate", in.Candidate, in.Scorers)
			if err != nil {
				return "", fmt.Errorf("candidate: %w", err)
			}
			var opts []eval.Option
			if in.Name != "" {
				opts = append(opts, eval.WithName(in.Name))
			}
			return encodeJSON(eval.CompareSuites(base, cand, opts...))
		},
	}
	return []agenttypes.Tool{run, compare}
}

func runSuite(ctx context.Context, name string, cases []evalCase, specs []eval.ScorerSpec) (*eval.SuiteResult, error) {
	if len(cases) == 0 {
		return nil, errors.New("no cases to score")
	}
	if len(cases) > maxEvalCases {
		return nil, fmt.Errorf("%d cases is more than the limit of %d", len(cases), maxEvalCases)
	}
	if len(specs) == 0 {
		return nil, errors.New("no scorers given")
	}
	scorers, err := eval.DefaultRegistry.BuildAll(specs)
	if err != nil {
		return nil, err
	}
	obs := make([]eval.Observation, 0, len(cases))
	seen := map[string]bool{}
	for _, c := range cases {
		o, err := c.observation()
		if err != nil {
			return nil, err
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("case id %q appears twice", c.ID)
		}
		seen[c.ID] = true
		obs = append(obs, o)
	}
	if name == "" {
		name = "eval"
	}
	return eval.Run(ctx, name, obs, scorers)
}

func scorerKinds() string {
	var out []byte
	for i, k := range eval.DefaultRegistry.Kinds() {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, k.Kind...)
	}
	return string(out)
}

// decodeArgs decodes tool arguments into v, rejecting unknown properties.
func decodeArgs(args map[string]any, v any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", agenttypes.ErrInvalidToolArguments, err)
	}
	return nil
}

func encodeJSON(v any) (string, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
