package message

import (
	"strconv"
	"testing"
)

func BenchmarkFoldText(b *testing.B) {
	for _, chunks := range []int{1, 128, 4096} {
		b.Run(strconv.Itoa(chunks), func(b *testing.B) {
			events := []Event{ContentBlockStart{ContentBlock: Block{Type: BlockText}}}
			chunk := ContentBlockDelta{Delta: BlockDelta{Type: deltaText, Text: "word"}}
			for range chunks {
				events = append(events, chunk)
			}
			events = append(events, ContentBlockStop{}, MessageStop{})
			b.ReportAllocs()
			for b.Loop() {
				Fold(events, 0)
			}
		})
	}
}
