// Command saige is the CLI for the saige SDK.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// Provider name constants.
const (
	providerOllama    = "ollama"
	providerOpenAI    = "openai"
	providerGoogle    = "google"
	providerAnthropic = "anthropic"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rootCmd := &cobra.Command{
		Use:     "saige",
		Short:   "AI SDK CLI — chat, ask, RAG, knowledge graph",
		Version: version,
	}

	addPersistentFlags(rootCmd)
	rootCmd.AddCommand(
		newChatCmd(ctx),
		newAskCmd(ctx),
		newRagCmd(ctx),
		newKgCmd(ctx),
		newUpdateCmd(),
		newVersionCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the saige version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version)
		},
	}
}
