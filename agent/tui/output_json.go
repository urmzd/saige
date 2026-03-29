package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// JSONOutput renders all output as JSON. Suitable for pipes and scripts.
type JSONOutput struct {
	W   io.Writer
	Err io.Writer
}

// NewJSONOutput creates a JSONOutput writing to the given writers.
func NewJSONOutput(w, errW io.Writer) *JSONOutput {
	return &JSONOutput{W: w, Err: errW}
}

func (o *JSONOutput) Header(h OutputHeader) {
	// JSON mode: no header chrome
}

func (o *JSONOutput) Result(v any) error {
	enc := json.NewEncoder(o.W)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (o *JSONOutput) StreamDeltas(_ AgentHeader, ch <-chan types.Delta) VerboseResult {
	var text strings.Builder
	for delta := range ch {
		switch d := delta.(type) {
		case types.TextContentDelta:
			text.WriteString(d.Content)
			fmt.Fprint(o.W, d.Content)
		case types.ErrorDelta:
			return VerboseResult{Text: text.String(), Err: d.Error}
		}
	}
	fmt.Fprintln(o.W)
	return VerboseResult{Text: text.String()}
}

func (o *JSONOutput) Error(err error) {
	fmt.Fprintf(o.Err, "error: %v\n", err)
}

func (o *JSONOutput) Status(msg string) {
	enc := json.NewEncoder(o.W)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{"status": msg})
}
