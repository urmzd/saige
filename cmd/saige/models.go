package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// newModelsCmd surfaces the capability catalog. Without it the only way to
// learn that o3 rejects --temperature, or that gemini-2.5 sizes reasoning by
// token budget while gemini-3 sizes it by level, is to send the request and
// read the error.
func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models [model]",
		Short: "List model capabilities, or inspect one model",
		Long: "Print the capability table: which flags each model family accepts.\n" +
			"With a model argument, resolve that model (against --provider) and print\n" +
			"its full capability surface, including whether it was recognised at all.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cf := persistentFlagVars
			if len(args) == 1 {
				return describeModel(cf.resolvedProvider(), args[0], cf.isJSON())
			}
			return listModels(cf.isJSON())
		},
	}
	return cmd
}

// capabilityRow is the JSON shape of one table row.
type capabilityRow struct {
	Provider        string   `json:"provider"`
	Family          string   `json:"family"`
	Reasoning       string   `json:"reasoning"`
	StructuredOut   string   `json:"structured_output"`
	Tools           bool     `json:"tools"`
	Temperature     bool     `json:"temperature"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ServerTools     []string `json:"server_tools,omitempty"`
	Pricing         string   `json:"pricing"`
	Revisions       int      `json:"revisions"`
	Capabilities    []string `json:"capabilities"`
}

func listModels(asJSON bool) error {
	var rows []capabilityRow
	for _, provider := range catalog.Providers() {
		families := catalog.Families(provider)
		sort.Strings(families)
		for _, family := range families {
			rows = append(rows, newRow(catalog.MustLookup(provider, family)))
		}
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tFAMILY\tREASONING\tSCHEMA\tTOOLS\tTEMP\tCONTEXT\tSERVER TOOLS\tPRICING")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Provider, r.Family, r.Reasoning, dash(r.StructuredOut),
			yesNo(r.Tools), yesNo(r.Temperature), count(r.ContextWindow),
			dash(strings.Join(r.ServerTools, ",")), r.Pricing)
	}
	return w.Flush()
}

func describeModel(provider, model string, asJSON bool) error {
	caps, found := catalog.Lookup(provider, model)

	if asJSON {
		out := struct {
			capabilityRow
			Model              string   `json:"model"`
			Known              bool     `json:"known"`
			ReasoningEfforts   []string `json:"reasoning_efforts,omitempty"`
			MinReasoningBudget int      `json:"min_reasoning_budget,omitempty"`
			Notes              []string `json:"notes,omitempty"`
		}{newRow(caps), model, found, caps.ReasoningEfforts, caps.MinReasoningBudget, caps.Notes}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Printf("%s / %s\n", provider, model)
	if !found {
		fmt.Printf("  not in the catalog: showing the conservative %s baseline.\n", provider)
		fmt.Println("  Treat every capability below as unverified for this model.")
	} else {
		fmt.Printf("  family: %s\n", caps.Family)
	}
	fmt.Printf("  reasoning: %s\n", reasoningSummary(caps))
	fmt.Printf("  structured output: %s\n", dash(string(caps.StructuredOutput)))
	if caps.ContextWindow > 0 {
		fmt.Printf("  context window: %d tokens\n", caps.ContextWindow)
	}
	if caps.MaxOutputTokens > 0 {
		fmt.Printf("  max output: %d tokens\n", caps.MaxOutputTokens)
	}
	if media := mediaList(caps); media != "" {
		fmt.Printf("  native media: %s\n", media)
	}
	fmt.Printf("  pricing: %s\n", caps.Pricing.Describe())
	if len(caps.ServerTools) > 0 {
		kinds := make([]string, 0, len(caps.ServerTools))
		for _, k := range caps.ServerTools {
			kinds = append(kinds, string(k))
		}
		fmt.Printf("  server tools: %s\n", strings.Join(kinds, ", "))
	}
	if found {
		fmt.Printf("  registry revisions: %d\n", catalog.Revisions(provider, caps.Family))
	}
	fmt.Printf("  flags: %s\n", strings.Join(capNames(caps), ", "))
	for _, n := range caps.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	return nil
}

func newRow(caps types.ModelCapabilities) capabilityRow {
	serverTools := make([]string, 0, len(caps.ServerTools))
	for _, k := range caps.ServerTools {
		serverTools = append(serverTools, string(k))
	}
	revisions := 0
	if caps.Family != "" {
		revisions = catalog.Revisions(caps.Provider, caps.Family)
	}
	return capabilityRow{
		Provider:        caps.Provider,
		Family:          caps.Family,
		ServerTools:     serverTools,
		Pricing:         caps.Pricing.Describe(),
		Revisions:       revisions,
		Reasoning:       reasoningSummary(caps),
		StructuredOut:   string(caps.StructuredOutput),
		Tools:           caps.Supports(types.CapTools),
		Temperature:     caps.Supports(types.CapTemperature),
		ContextWindow:   caps.ContextWindow,
		MaxOutputTokens: caps.MaxOutputTokens,
		Capabilities:    capNames(caps),
	}
}

// reasoningSummary names how reasoning is sized, not merely whether it exists:
// "yes" would hide the difference between a token budget and an effort enum,
// which is exactly the difference that breaks a shared config.
func reasoningSummary(caps types.ModelCapabilities) string {
	if !caps.Supports(types.CapReasoning) {
		return "no"
	}
	switch {
	case caps.Supports(types.CapReasoningBudget):
		return "budget"
	case caps.Supports(types.CapReasoningEffort):
		return "effort"
	case caps.Supports(types.CapReasoningToggle):
		return "toggle"
	default:
		return "fixed"
	}
}

func capNames(caps types.ModelCapabilities) []string {
	list := caps.List()
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = string(c)
	}
	return out
}

func mediaList(caps types.ModelCapabilities) string {
	var out []string
	for mt, ok := range caps.Media.NativeTypes {
		if ok {
			out = append(out, string(mt))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func count(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}
