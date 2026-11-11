package runtime

import (
	"errors"
	"fmt"
)

// DefaultStride is the size of one attempt's sequence band.
//
// It bounds the number of events a single attempt may publish (Stride-2 deltas
// between the attempt's opening and closing lifecycle events). A million deltas
// in one turn is not a busy agent, it is a loop, so the bound doubles as a
// backstop.
const DefaultStride = 1 << 20

// SeqAccepted is where every run's stream starts: the accepted event, published
// once when the ledger takes the run, before any attempt exists.
const SeqAccepted = 1

// maxSeq is the largest sequence number a run may reach: 2^53, the largest
// integer a JSON number carries exactly. Bands are refused past it rather than
// silently wrapping into a number a reader cannot represent.
const maxSeq = 1 << 53

// ErrSeqExhausted reports an attempt that published more events than its band
// holds. It is terminal: the next attempt would produce the same flood.
var ErrSeqExhausted = errors.New("runtime: attempt sequence band exhausted")

// band is one attempt's slice of a run's sequence space.
//
// # Why sequences are banded by attempt
//
// Event.Seq is per-run, and the bus treats a sequence at or below a run's
// high-water mark as an idempotent no-op: first write wins. That rule is what
// stops durable replay from duplicating a stream, and it means the runtime must
// choose sequence numbers with a clear head about what should be deduplicated
// and what must not be.
//
// Two things happen to a run that make the naive answer wrong. A memoized replay
// re-executes the workflow body but not the agent, so it publishes only
// lifecycle events. A retry after a crash re-executes the agent for real, and
// its output is DIFFERENT from the partial output the interrupted attempt
// published. Numbering both from the same base would make the retry's entire
// answer land at or below the high-water mark, so the bus would drop it — and,
// worse, drop the completed event too. A subscriber would watch a run go quiet
// forever while the ledger recorded success.
//
// So each attempt gets its own band, laid out as:
//
//	seq 1                        accepted (no attempt yet)
//	[1+a*Stride]                 started (a == 1) or resumed (a > 1)
//	(1+a*Stride, band ceiling)   the attempt's deltas, in order
//	[band ceiling]               completed, failed, or parked
//
// The consequences are all the ones we want. Within one attempt, numbering is a
// pure function of the attempt, so re-executing the same attempt publishes the
// identical stream and the bus deduplicates it exactly. Across attempts,
// numbering only ever increases, so a retry appends its real output after the
// interrupted attempt's partial output instead of vanishing behind it. Gaps
// between bands are free: Subscription.From is "strictly after this sequence",
// so a consumer never notices, and no backend stores anything for a number that
// was never published.
//
// The cost is honest and small: a run that publishes completed, crashes before
// the ledger records it, and replays will publish completed twice with two
// sequence numbers. Sinks are already required to tolerate redelivery, and
// duplicating a terminal event is strictly better than losing one.
type band struct {
	open  uint64 // the attempt's first sequence: started or resumed
	close uint64 // the attempt's last sequence: completed, failed, or parked
	next  uint64 // the next delta sequence
}

// newBand returns the band for an attempt, counting from 1. An attempt of 0 or
// less is treated as 1: outside a durable run there is exactly one attempt.
func newBand(stride uint64, attempt int) (band, error) {
	if stride == 0 {
		stride = DefaultStride
	}
	if attempt < 1 {
		attempt = 1
	}
	// Refuse before multiplying rather than after: an overflowed product would
	// wrap into a low sequence and start silently overwriting the run's stream.
	if uint64(attempt) > (maxSeq-1)/stride {
		return band{}, fmt.Errorf("%w: attempt %d exceeds the %d bands a stride of %d allows", ErrSeqExhausted, attempt, (maxSeq-1)/stride, stride)
	}
	start := SeqAccepted + uint64(attempt)*stride
	return band{open: start, close: start + stride - 1, next: start + 1}, nil
}

// delta allocates the next sequence for an event the agent produced.
func (b *band) delta() (uint64, error) {
	if b.next >= b.close {
		return 0, fmt.Errorf("%w: an attempt may publish %d events", ErrSeqExhausted, b.close-b.open-1)
	}
	s := b.next
	b.next++
	return s, nil
}
