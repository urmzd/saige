package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// JSONOutput renders all output as JSON. Suitable for pipes and scripts.
// Results and statuses are JSON documents; agent streams are JSON lines.
type JSONOutput struct {
	W   io.Writer
	Err io.Writer
}

// NewJSONOutput creates a JSONOutput writing to the given writers.
func NewJSONOutput(w, errW io.Writer) *JSONOutput {
	return &JSONOutput{W: w, Err: errW}
}

// Header implements Output.
func (o *JSONOutput) Header(h OutputHeader) {
	// JSON mode: no header chrome
}

// Result implements Output.
func (o *JSONOutput) Result(v any) error {
	enc := json.NewEncoder(o.W)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// StreamDeltas writes each delta as one line of JSON: a wire version 2
// envelope (see types.Envelope), the same contract every other consumer of
// the stream decodes. The final answer is the concatenated text of the
// part.delta envelopes. Media bytes are written inline however large they
// are: a pipe has no inline limit, and nothing is dropped.
func (o *JSONOutput) StreamDeltas(header AgentHeader, ch <-chan types.Delta) VerboseResult {
	return o.StreamDeltasResolving(header, ch, nil)
}

// StreamDeltasResolving implements MarkerResolvingOutput. Markers are written
// like every other delta and then decided inline by resolve.
func (o *JSONOutput) StreamDeltasResolving(_ AgentHeader, ch <-chan types.Delta, resolve MarkerResolver) VerboseResult {
	var text strings.Builder
	enc, _ := types.NewEncoder(types.EncodeOptions{Version: types.WireVersion, MaxInlineBytes: -1})
	for delta := range ch {
		o.writeDelta(enc, delta)
		switch d := delta.(type) {
		case types.MarkerDelta:
			if resolve != nil {
				resolve(d)
			}
		case types.PartDelta:
			text.WriteString(d.Text)
		case types.ErrorDelta:
			return VerboseResult{Text: text.String(), Err: d.Error}
		}
	}
	return VerboseResult{Text: text.String()}
}

// writeDelta writes d as one envelope line. A delta the codec cannot encode
// is reported on the error writer instead of breaking the stream.
func (o *JSONOutput) writeDelta(enc *types.Encoder, d types.Delta) {
	envs, err := enc.Encode(d)
	for _, env := range envs {
		b, mErr := json.Marshal(env)
		if mErr != nil {
			err = mErr
			break
		}
		b = append(b, '\n')
		_, _ = o.W.Write(b)
	}
	if err != nil && o.Err != nil {
		fmt.Fprintf(o.Err, "error: encode %T: %v\n", d, err)
	}
}

func (o *JSONOutput) Error(err error) {
	fmt.Fprintf(o.Err, "error: %v\n", err)
}

// Status implements Output.
func (o *JSONOutput) Status(msg string) {
	enc := json.NewEncoder(o.W)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]string{"status": msg})
}
