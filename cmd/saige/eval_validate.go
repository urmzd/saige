package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/eval/harness"
)

// errEvalInvalid is returned by saige eval validate after it printed the
// issues, so the error is not printed a second time.
var errEvalInvalid = errors.New("validation failed")

func newEvalValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [path]",
		Short: "Check a suite manifest and its corpus without running them",
		Long: `Check a suite manifest and its corpus without calling a model.

path is a manifest file, a directory holding ` + harness.ManifestFile + `, or a corpus
directory; it defaults to the current directory. The manifest is decoded
strictly, so an unknown or misspelled key is an error. Each issue is printed
as file:/json/pointer: severity: message. The command fails when any issue is
an error; warnings alone pass.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			issues, err := validateEvalPath(path)
			if err != nil {
				return err
			}
			return reportEvalIssues(cmd.OutOrStdout(), issues)
		},
	}
}

// validateEvalPath validates a manifest (and the corpus it names) or a bare
// corpus directory.
func validateEvalPath(path string) ([]harness.Issue, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	manifestPath := path
	if info.IsDir() {
		manifestPath = filepath.Join(path, harness.ManifestFile)
		if _, err := os.Stat(manifestPath); err != nil {
			return harness.ValidateCorpus(path), nil
		}
	}
	m, issues, err := harness.LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if m != nil && m.Corpus != "" {
		issues = append(issues, harness.ValidateCorpus(m.Resolve(m.Corpus))...)
	}
	return issues, nil
}

// reportEvalIssues prints issues as text, or as JSON with --format json, and
// returns an error when any is an error.
func reportEvalIssues(w io.Writer, issues []harness.Issue) error {
	valid := !harness.HasErrors(issues)
	if persistentFlagVars.isJSON() {
		if issues == nil {
			issues = []harness.Issue{}
		}
		if err := json.NewEncoder(w).Encode(struct {
			Valid  bool            `json:"valid"`
			Issues []harness.Issue `json:"issues"`
		}{valid, issues}); err != nil {
			return err
		}
	} else {
		printIssues(w, issues, false)
		errorsN := 0
		for _, is := range issues {
			if is.Severity == harness.SeverityError {
				errorsN++
			}
		}
		if valid {
			fmt.Fprintf(w, "ok (%d warnings)\n", len(issues))
		} else {
			fmt.Fprintf(w, "%d errors, %d warnings\n", errorsN, len(issues)-errorsN)
		}
	}
	if !valid {
		return reportedError{invalidInput(errEvalInvalid)}
	}
	return nil
}
