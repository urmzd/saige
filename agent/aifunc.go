package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"text/template"

	"github.com/urmzd/saige/agent/types"
)

// AIConfig configures an AIFunc.
type AIConfig struct {
	// Prompt is a text/template rendered with the input value as dot, for
	// example "Classify this ticket: {{.Body}}". The result is the user
	// message of each call. Required.
	Prompt string
	// System is the system prompt. Optional.
	System string
	// Preset or Provider serves the calls. Set exactly one. A preset gives
	// each call a new routing session.
	Preset   types.Preset
	Provider types.Provider
	// ConfigHash identifies the serving configuration in the version. Empty
	// derives it: a preset's configuration key when it reports one (as a
	// preset.Bundle does), otherwise the preset name and catalog revision,
	// or the provider's name and model.
	ConfigHash string
	// Mode selects how the output schema reaches the model. See OutputMode.
	Mode OutputMode
	// Repair is how many times an answer that does not fit Out is sent back
	// to the model with the error. See OutputSpec.Repair.
	Repair int
}

// AIFunction is a typed function an LLM computes: it renders a prompt from
// an In, asks the model for an Out with Structured, and returns the decoded
// value. It is safe for concurrent use: each call runs on its own agent and
// conversation tree.
type AIFunction[In, Out any] struct {
	name, description string
	cfg               AIConfig
	tmpl              *template.Template
	version           string
}

// AIFunc builds an LLM-backed typed function. In and Out must be structs: In
// is rendered into the prompt and becomes the tool schema, and Out is the
// schema the answer must match.
//
// Its version is a hash of the prompt template, the system prompt, the
// input and output schemas, the output mode and the serving configuration
// hash, so a change to any of them is a new version. The tool from Tool
// reports it, so the agent loop records it next to each result in the tree
// (types.ToolResultContent.ToolVersion) and eval provenance lists it
// (eval.Provenance.Tools).
func AIFunc[In, Out any](name, description string, cfg AIConfig) (*AIFunction[In, Out], error) {
	if name == "" {
		return nil, errors.New("agent.AIFunc: name is required")
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		return nil, fmt.Errorf("agent.AIFunc %q: prompt is required", name)
	}
	if (cfg.Preset == nil) == (cfg.Provider == nil) {
		return nil, fmt.Errorf("agent.AIFunc %q: set exactly one of Preset and Provider", name)
	}
	if t := reflect.TypeFor[In](); t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("agent.AIFunc %q: input type %s is not a struct", name, t)
	}
	outSchema, err := outputSchema[Out](nil)
	if err != nil {
		return nil, fmt.Errorf("agent.AIFunc %q: %w", name, err)
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("agent.AIFunc %q: prompt: %w", name, err)
	}
	f := &AIFunction[In, Out]{name: name, description: description, cfg: cfg, tmpl: tmpl}
	f.version, err = aiVersion(cfg, types.SchemaFrom[In](), *outSchema)
	if err != nil {
		return nil, fmt.Errorf("agent.AIFunc %q: %w", name, err)
	}
	return f, nil
}

// Name returns the function's name.
func (f *AIFunction[In, Out]) Name() string { return f.name }

// Version returns the function's content version.
func (f *AIFunction[In, Out]) Version() string { return f.version }

// Render returns the user message a call with in would send.
func (f *AIFunction[In, Out]) Render(in In) (string, error) {
	var b strings.Builder
	if err := f.tmpl.Execute(&b, in); err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	return b.String(), nil
}

// Call computes the function for in. The error wraps ErrSchemaInvalid when
// the model's answer still did not fit Out after the repairs.
func (f *AIFunction[In, Out]) Call(ctx context.Context, in In) (Out, error) {
	var zero Out
	prompt, err := f.Render(in)
	if err != nil {
		return zero, err
	}
	var opts []AgentOption
	if f.cfg.Preset != nil {
		opts = append(opts, WithPreset(f.cfg.Preset))
	}
	a := NewAgent(AgentConfig{Name: f.name, SystemPrompt: f.cfg.System, Provider: f.cfg.Provider}, opts...)
	out, _, err := Structured[Out](ctx, a, []types.Message{types.NewUserMessage(prompt)}, OutputSpec[Out]{
		Mode:   f.cfg.Mode,
		Repair: f.cfg.Repair,
	})
	return out, err
}

// Tool exposes the function as a tool built with Func. It declares itself
// read-only, since a model call changes nothing; a Capability option
// overrides that. It reports the function's version rather than a schema
// hash.
func (f *AIFunction[In, Out]) Tool(opts ...FuncOption) types.Tool {
	all := append([]FuncOption{Capability(types.ToolCapabilityRead)}, opts...)
	all = append(all, withVersion(f.version))
	return Func(f.name, f.description, func(rc RunContext[NoDeps], in In) (Out, error) {
		return f.Call(rc, in)
	}, all...)
}

// aiVersion hashes everything that decides an AIFunc's answers.
func aiVersion(cfg AIConfig, in, out types.ParameterSchema) (string, error) {
	raw, err := json.Marshal(struct {
		Prompt     string                `json:"prompt"`
		System     string                `json:"system"`
		Input      types.ParameterSchema `json:"input"`
		Output     types.ParameterSchema `json:"output"`
		Mode       OutputMode            `json:"mode"`
		ConfigHash string                `json:"config_hash"`
	}{cfg.Prompt, cfg.System, in, out, cfg.Mode, aiConfigHash(cfg)})
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

// aiConfigHash identifies the serving configuration.
func aiConfigHash(cfg AIConfig) string {
	if cfg.ConfigHash != "" {
		return cfg.ConfigHash
	}
	if cfg.Preset != nil {
		d := cfg.Preset.Defaults()
		if k, ok := cfg.Preset.(interface{ ConfigKey(string) string }); ok {
			if h := k.ConfigKey(d.Name); h != "" {
				return h
			}
		}
		return "preset:" + d.Name + "@" + d.CatalogRevision
	}
	return "provider:" + types.ProviderName(cfg.Provider) + "/" + types.ProviderModel(cfg.Provider)
}
