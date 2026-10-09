package tui

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// MarkerResolver decides a pending marker. Renderers that accept one call it
// synchronously when a MarkerDelta arrives, after rendering the marker and
// before reading the next delta, so a prompt never races the stream output.
// The resolver must eventually resolve the marker on its EventStream, or the
// agent loop stays blocked waiting for the decision.
type MarkerResolver func(types.MarkerDelta)

// MarkerResolvingOutput is implemented by Outputs that can resolve markers
// inline while they render a delta stream. StyledOutput and JSONOutput both
// implement it.
type MarkerResolvingOutput interface {
	StreamDeltasResolving(header AgentHeader, ch <-chan types.Delta, resolve MarkerResolver) VerboseResult
}

// StreamDeltasResolving renders ch on out and calls resolve for every
// MarkerDelta. Outputs that implement MarkerResolvingOutput resolve inline.
// Any other Output renders the stream in segments that each end at a marker:
// resolve runs only after out.StreamDeltas has returned for the segment that
// carried the marker, so the renderer and the resolver never write at the
// same time. Later segments get an empty header so it is printed once. A nil
// resolve behaves like out.StreamDeltas.
func StreamDeltasResolving(out Output, header AgentHeader, ch <-chan types.Delta, resolve MarkerResolver) VerboseResult {
	if resolve == nil {
		return out.StreamDeltas(header, ch)
	}
	if ro, ok := out.(MarkerResolvingOutput); ok {
		return ro.StreamDeltasResolving(header, ch, resolve)
	}

	var total VerboseResult
	for {
		seg := make(chan types.Delta)
		fed := make(chan segmentEnd, 1)
		go feedSegment(ch, seg, fed)

		res := out.StreamDeltas(header, seg)
		header = AgentHeader{}
		total.Text += res.Text
		if total.Err == nil {
			total.Err = res.Err
		}

		// A renderer that returns before its segment closes has stopped
		// for good: drain the rest of the stream, still resolving markers
		// so the agent loop is never left waiting.
		stopped := false
		for range seg {
			stopped = true
		}
		end := <-fed
		if end.marker != nil {
			resolve(*end.marker)
		}
		if stopped {
			for d := range ch {
				if m, ok := d.(types.MarkerDelta); ok {
					resolve(m)
				}
			}
			return total
		}
		if end.marker == nil {
			return total
		}
	}
}

// segmentEnd reports why feedSegment closed its segment: at a marker, or
// (marker nil) because the source stream ended.
type segmentEnd struct {
	marker *types.MarkerDelta
}

// feedSegment copies deltas from ch to seg up to and including the next
// marker, then closes seg and reports how the segment ended on fed.
func feedSegment(ch <-chan types.Delta, seg chan<- types.Delta, fed chan<- segmentEnd) {
	defer close(seg)
	for d := range ch {
		seg <- d
		if m, ok := d.(types.MarkerDelta); ok {
			fed <- segmentEnd{marker: &m}
			return
		}
	}
	fed <- segmentEnd{}
}

// ParseApproval interprets one answer to an approval prompt. Only "y" and
// "yes" approve and only "n" and "no" deny, ignoring case and surrounding
// space. Anything else, including an empty answer, is invalid: the caller
// must ask again instead of guessing, because a guess in either direction
// can run a destructive tool the user did not mean to allow.
func ParseApproval(input string) (approved, valid bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
		return true, true
	case "n", "no":
		return false, true
	default:
		return false, false
	}
}

// approvalArgLines caps the tool arguments printed with an approval prompt.
const approvalArgLines = 40

// approvalPrompt is the question shown for a pending marker.
const approvalPrompt = "Approve? (y/n) "

// PromptApproval writes an approval prompt for d, including the tool call's
// arguments so the user sees what they approve, to w and reads answers from
// sc until one is valid. End of input or a read error denies, so a closed or
// non-interactive stdin can never approve a marked tool call.
func PromptApproval(sc *bufio.Scanner, w io.Writer, d types.MarkerDelta) bool {
	_, _ = fmt.Fprintf(w, "%s Tool %q requires approval\n", iconMarker, d.ToolName)
	for _, mk := range d.Markers {
		_, _ = fmt.Fprintf(w, "  %s: %s\n", mk.Kind, mk.Message)
	}
	return askApproval(sc, w, d)
}

// askApproval is PromptApproval without the header and marker lines, for a
// renderer that has already shown them.
func askApproval(sc *bufio.Scanner, w io.Writer, d types.MarkerDelta) bool {
	for _, l := range strings.Split(FormatArgs(d.Arguments, approvalArgLines), "\n") {
		_, _ = fmt.Fprintf(w, "  %s\n", l)
	}
	for {
		_, _ = fmt.Fprint(w, approvalPrompt)
		if !sc.Scan() {
			_, _ = fmt.Fprintln(w)
			return false
		}
		if approved, ok := ParseApproval(sc.Text()); ok {
			return approved
		}
		_, _ = fmt.Fprintln(w, "Please answer y or n.")
	}
}
