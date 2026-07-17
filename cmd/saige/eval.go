package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/eval/harness"
)

func newEvalCmd(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run live eval corpora against an OpenAI-compatible API",
	}

	cmd.AddCommand(
		newEvalRunCmd(ctx),
		newEvalInitCmd(),
	)

	return cmd
}

func newEvalRunCmd(ctx context.Context) *cobra.Command {
	var experimentsDir, idFilter, flowSpec, model, apiBase, apiKey string
	var count int
	var force bool

	cmd := &cobra.Command{
		Use:          "run",
		Short:        "Run eval flows over an experiment corpus",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if apiKey == "" {
				apiKey = firstEnv("SAIGE_EVAL_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "GROQ_API_KEY", "OPENROUTER_API_KEY", "MISTRAL_API_KEY", "GITHUB_TOKEN")
			}
			flows, err := resolveEvalFlows(flowSpec)
			if err != nil {
				return err
			}
			experiments, err := harness.LoadCorpus(experimentsDir)
			if err != nil {
				return err
			}
			experiments = harness.FilterExperiments(experiments, idFilter, count)
			runner := &harness.Runner{
				Client: harness.NewClient(apiBase, apiKey, model),
				Flows:  flows,
				Force:  force,
			}
			return runner.Run(ctx, experiments)
		},
	}

	cmd.Flags().StringVar(&experimentsDir, "experiments-dir", "", "Experiment corpus directory (required)")
	_ = cmd.MarkFlagRequired("experiments-dir")
	cmd.Flags().IntVar(&count, "count", 0, "Maximum experiments to run, 0 for all")
	cmd.Flags().StringVar(&idFilter, "id", "", "Experiment ID prefix filter")
	cmd.Flags().StringVar(&flowSpec, "flows", "base,stateless", "Comma-separated flows to run (base|stateless)")
	cmd.Flags().StringVar(&model, "model", "gpt-4o-mini", "OpenAI-compatible model name")
	cmd.Flags().StringVar(&apiBase, "api-base",
		envOr("SAIGE_EVAL_API_BASE", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")),
		"OpenAI-compatible API base URL [$SAIGE_EVAL_API_BASE, $OPENAI_BASE_URL]")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"API key [$SAIGE_EVAL_API_KEY, $OPENAI_API_KEY, $GEMINI_API_KEY, $GOOGLE_API_KEY, $GROQ_API_KEY, $OPENROUTER_API_KEY, $MISTRAL_API_KEY, $GITHUB_TOKEN]")
	cmd.Flags().BoolVar(&force, "force", false, "Re-run experiments even when metrics.json exists")

	return cmd
}

func newEvalInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "init <dir>",
		Short:        "Scaffold a sample eval corpus",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			expDir := filepath.Join(dir, "001-example")
			if _, err := os.Stat(expDir); err == nil {
				return fmt.Errorf("%s already exists", expDir)
			}

			files := map[string]string{
				"system.md": "You are a careful technical writer. Produce the requested document in Markdown. Return only the document, raw, with no commentary.\n",
				"turn-0.md": "Write a short README for a command line tool called hello that prints a greeting. Include an Overview section and a Usage section.\n",
				"turn-1.md": "Add an Installation section explaining that the tool is installed with go install.\n",
			}
			for name, content := range files {
				path := filepath.Join(expDir, name)
				if err := harness.WriteText(path, content); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "created %s\n", path)
			}
			fmt.Fprintf(os.Stderr, "corpus ready; run: saige eval run --experiments-dir %s\n", dir)
			return nil
		},
	}
	return cmd
}

func resolveEvalFlows(spec string) ([]harness.Flow, error) {
	known := map[string]harness.Flow{
		"base":      harness.BaseFlow{},
		"stateless": harness.StatelessFlow{},
	}
	var flows []harness.Flow
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		flow, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("unknown flow %q (known flows: base, stateless)", name)
		}
		flows = append(flows, flow)
	}
	if len(flows) == 0 {
		return nil, fmt.Errorf("no flows specified")
	}
	return flows, nil
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
