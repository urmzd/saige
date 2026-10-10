package tui

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/urmzd/saige/agent/types"
)

// StyledOutput renders output with lipgloss styling and template control.
// Suitable for human-readable terminal output.
type StyledOutput struct {
	W        io.Writer
	Err      io.Writer
	Template Template
	Width    int
}

// NewStyledOutput creates a StyledOutput writing to the given writers.
func NewStyledOutput(w, errW io.Writer, tmpl Template) *StyledOutput {
	return &StyledOutput{W: w, Err: errW, Template: tmpl, Width: 80}
}

// Header implements Output.
func (o *StyledOutput) Header(h OutputHeader) {
	if !o.Template.ShowHeader {
		return
	}
	ah := AgentHeader{Name: h.Operation}
	if h.Provider != "" {
		ah.Provider = h.Provider
	}
	PopulateEnv(&ah)
	fmt.Fprintln(o.W, renderHeader(ah, o.Width))
	fmt.Fprintln(o.W)
}

// Result implements Output.
func (o *StyledOutput) Result(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(o.W, string(data))
	return nil
}

// StreamDeltas renders ch on W. A terminal error is returned, not printed:
// the caller reports it once through Error, on the error writer.
func (o *StyledOutput) StreamDeltas(header AgentHeader, ch <-chan types.Delta) VerboseResult {
	return streamVerbose(header, ch, o.W, o.Template, nil, false)
}

// StreamDeltasResolving implements MarkerResolvingOutput.
func (o *StyledOutput) StreamDeltasResolving(header AgentHeader, ch <-chan types.Delta, resolve MarkerResolver) VerboseResult {
	return streamVerbose(header, ch, o.W, o.Template, resolve, false)
}

func (o *StyledOutput) Error(err error) {
	fmt.Fprintln(o.Err, statusError.Render(fmt.Sprintf("%s Error: %v", iconError, err)))
}

// Status implements Output.
func (o *StyledOutput) Status(msg string) {
	fmt.Fprintln(o.W, statusDone.Render(fmt.Sprintf("%s %s", iconDone, msg)))
}
