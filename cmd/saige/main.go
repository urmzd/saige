// Command saige is the CLI for the saige SDK.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/tui"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// cliName is the binary and agent name.
const cliName = "saige"

// Provider name constants.
const (
	providerOllama    = "ollama"
	providerOpenAI    = "openai"
	providerGoogle    = "google"
	providerAnthropic = "anthropic"
	// providerVertex is Google models served through Vertex AI. It is a CLI
	// name only: its entries use provider "google" with a vertex block.
	providerVertex = "vertex"
)

func main() {
	// SIGTERM is what container runtimes send on stop; cancelling the context
	// lets in-flight work and deferred cleanup finish instead of being cut off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the CLI with args and returns the process exit code. Errors
// are printed once, to errW, unless the command already rendered them.
func run(ctx context.Context, args []string, errW io.Writer) int {
	rootCmd := newRootCmd(ctx)
	rootCmd.SetArgs(args)
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		var rep reportedError
		if !errors.As(err, &rep) {
			fmt.Fprintf(errW, "error: %v\n", err)
		}
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			return coded.ExitCode()
		}
		return 1
	}
	return 0
}

func newRootCmd(ctx context.Context) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     cliName,
		Short:   "AI SDK CLI: chat, ask, RAG, knowledge graph",
		Version: version,
		// Commands return errors instead of exiting so deferred cleanup runs;
		// run prints them once.
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	addPersistentFlags(rootCmd)
	rootCmd.AddCommand(
		newChatCmd(ctx),
		newAskCmd(ctx),
		newRagCmd(ctx),
		newKgCmd(ctx),
		newEvalCmd(ctx),
		newServeCmd(ctx),
		newModelsCmd(),
		newCatalogCmd(ctx),
		newUpdateCmd(),
		newVersionCmd(),
	)
	return rootCmd
}

// reportedError marks an error the command already rendered through its
// Output, so run does not print it a second time.
type reportedError struct{ error }

// exitInvalid is the exit status for invalid input, such as a manifest or
// corpus that fails validation, so scripts can tell it from a failed run
// (status 1).
const exitInvalid = 2

// invalidInputError marks err as invalid input; run exits with exitInvalid.
type invalidInputError struct{ error }

func (e invalidInputError) Unwrap() error { return e.error }

// ExitCode reports exitInvalid.
func (invalidInputError) ExitCode() int { return exitInvalid }

// invalidInput wraps a non-nil err as invalid input.
func invalidInput(err error) error {
	if err == nil {
		return nil
	}
	return invalidInputError{err}
}

func (e reportedError) Unwrap() error { return e.error }

// reported renders err on out and returns it marked as already shown.
func reported(out tui.Output, err error) error {
	out.Error(err)
	return reportedError{err}
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
