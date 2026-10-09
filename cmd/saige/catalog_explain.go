package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// explainedEntry is how one chain entry compiles a set of dials.
type explainedEntry struct {
	Profile  string `json:"profile"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Surface is the API the entry is served on, when it changes the mapping.
	Surface string `json:"surface,omitempty"`
	// Effective is the raw options the entry would send, by option name.
	Effective     map[string]any       `json:"effective,omitempty"`
	EffectiveHash string               `json:"effective_hash,omitempty"`
	Decisions     []types.DialDecision `json:"decisions,omitempty"`
	// Error is set when a dial was rejected, so the entry cannot serve.
	Error string `json:"error,omitempty"`
}

type explainedPreset struct {
	Name     string           `json:"name"`
	Revision string           `json:"catalog_revision,omitempty"`
	Dials    types.Dials      `json:"dials"`
	Policy   string           `json:"policy"`
	Tools    bool             `json:"tools,omitempty"`
	Chain    []explainedEntry `json:"chain"`
}

func newCatalogExplainCmd() *cobra.Command {
	var dials, surface string
	var tools, strict bool
	var per []string
	cmd := &cobra.Command{
		Use:   "explain [preset|provider/model]",
		Short: "Show what dials compile to on every chain entry",
		Long: "Compiles dials for each chain entry of a preset, on top of the entry's own\n" +
			"dials, and prints the raw options it would send and each dial decision:\n" +
			"applied, mapped, dropped, rejected, raw_override or deferred, with the reason.\n\n" +
			"  saige catalog explain default --dials '{\"creativity\":\"focused\",\"reasoning\":{\"depth\":\"high\"}}'\n" +
			"  saige catalog explain openai/gpt-6-luna --dials '{\"reasoning\":{\"depth\":\"high\"}}' --tools --surface chat",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d types.Dials
			if strings.TrimSpace(dials) != "" {
				dec := json.NewDecoder(strings.NewReader(dials))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&d); err != nil {
					return fmt.Errorf("--dials: %w", err)
				}
				if err := d.Validate(); err != nil {
					return fmt.Errorf("--dials: %w", err)
				}
			}
			pol := types.DialPolicy{Strict: strict}
			for _, kv := range per {
				name, h, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("--policy %q: want name=handling", kv)
				}
				if pol.Per == nil {
					pol.Per = map[types.DialName]types.Handling{}
				}
				pol.Per[types.DialName(name)] = types.Handling(h)
			}
			if err := pol.Validate(); err != nil {
				return err
			}
			switch surface {
			case "", types.SurfaceChat, types.SurfaceResponses:
			default:
				return fmt.Errorf("--surface %q: want chat or responses", surface)
			}
			cf := persistentFlagVars
			cat, err := cf.catalog()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			} else if name, cat, _, err = cf.selectPreset(cat); err != nil {
				return err
			}
			var rp catalog.ResolvedPreset
			if _, ok := cat.Presets[name]; ok || !strings.Contains(name, "/") {
				rp, err = cat.Resolve(name)
			} else {
				p, m, _ := strings.Cut(name, "/")
				rp, err = cat.ResolveModel(p, m)
			}
			if err != nil {
				return err
			}
			return writeExplained(cmd.OutOrStdout(), explainPreset(rp, d, pol, tools, surface), cf.isJSON())
		},
	}
	f := cmd.Flags()
	f.StringVar(&dials, "dials", "", `Request dials as JSON, for example {"creativity":"focused","reasoning":{"depth":"high"}}`)
	f.BoolVar(&tools, "tools", false, "Compile for a request that offers tools")
	f.StringVar(&surface, "surface", "", "OpenAI API surface: chat or responses (default: the one provider.Build picks)")
	f.BoolVar(&strict, "strict", false, "Reject every dial a model cannot honor exactly")
	f.StringArrayVar(&per, "policy", nil, "Per-dial handling as name=reject|nearest|drop (repeatable)")
	return cmd
}

// explainPreset compiles d, at request scope, for every entry of rp.
func explainPreset(rp catalog.ResolvedPreset, d types.Dials, pol types.DialPolicy, tools bool, surface string) explainedPreset {
	out := explainedPreset{Name: rp.Name, Revision: rp.CatalogRevision, Dials: d, Policy: pol.String(), Tools: tools}
	for _, e := range rp.Chain {
		layers := append([]types.DialLayer(nil), e.Dials...)
		if !d.IsZero() {
			layers = append(layers, types.DialLayer{Scope: types.DialScopeRequest, Dials: d.Clone()})
		}
		x := explainedEntry{Profile: e.ProfileID, Provider: e.Provider, Model: e.Model, Surface: entrySurface(e, surface)}
		eff, rep, err := types.ResolveDials(e.Caps, e.Options, types.DialContext{Tools: tools, Surface: x.Surface}, pol, layers...)
		x.Decisions = rep.Decisions
		if err != nil {
			x.Error = types.OptionReason(err)
		} else {
			shown := e
			shown.Options = eff
			x.Effective, x.EffectiveHash = optionValues(shown), rep.EffectiveHash
		}
		out.Chain = append(out.Chain, x)
	}
	return out
}

// entrySurface names the OpenAI API an entry is served on: the one asked
// for, or the one provider.Build selects for the row and the entry's own
// dials. Request dials do not change the adapter an entry is built with.
func entrySurface(e catalog.ResolvedEntry, asked string) string {
	if e.Provider != "openai" {
		return ""
	}
	if asked != "" {
		return asked
	}
	var merged types.Dials
	for _, l := range e.Dials {
		merged = merged.Merge(l.Dials)
	}
	switch e.Caps.ChatCompletionsTools {
	case types.ChatToolsResponsesOnly:
		return types.SurfaceResponses
	case types.ChatToolsNoReasoning:
		if r := merged.Reasoning; r != nil && r.Mode != types.ReasoningOff {
			return types.SurfaceResponses
		}
	}
	return types.SurfaceChat
}

func writeExplained(w io.Writer, xp explainedPreset, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(xp)
	}
	fmt.Fprintf(w, "preset %s (catalog %s), policy %s\n", xp.Name, dash(xp.Revision), xp.Policy)
	for i, e := range xp.Chain {
		fmt.Fprintf(w, "\n%d. %s  %s/%s", i+1, e.Profile, e.Provider, e.Model)
		if e.Surface != "" {
			fmt.Fprintf(w, "  (%s)", e.Surface)
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   DIAL\tREQUESTED\tACTION\tSENT\tSCOPE\tREASON")
		for _, d := range e.Decisions {
			fmt.Fprintf(tw, "   %s\t%s\t%s\t%s\t%s\t%s\n", d.Dial, d.Requested, d.Action, dash(d.Sent), dash(d.Scope), dash(d.Reason))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if e.Error != "" {
			fmt.Fprintf(w, "   error: %s\n", e.Error)
			continue
		}
		var parts []string
		for _, n := range append(types.AllOptionNames(), "prompt_cache", "server_tools") {
			if v, ok := e.Effective[n]; ok {
				parts = append(parts, fmt.Sprintf("%s=%v", n, v))
			}
		}
		fmt.Fprintf(w, "   sends: %s\n", dash(strings.Join(parts, " ")))
	}
	return nil
}
