package types

import "testing"

// The registry's whole job: three producers citing one page produce one
// footnote, not three. Provider-native search, a local search tool, and a RAG
// retriever all reach the same URL during one answer.
func TestOneSourceCitedByThreeProducersGetsOneNumber(t *testing.T) {
	r := NewCitationRegistry()

	fromProvider := NewCitation(CitationWeb, "https://example.com/a", "Example A")
	fromProvider.Producer = "anthropic"
	fromTool := NewCitation(CitationTool, "https://example.com/a", "Example A")
	fromTool.Producer = "web_search"
	fromRAG := NewCitation(CitationRetrieval, "https://example.com/a", "Example A")
	fromRAG.Producer = "rag"

	a, _ := r.Add(fromProvider)
	b, _ := r.Add(fromTool)
	c, _ := r.Add(fromRAG)

	if a.Ordinal != 1 || b.Ordinal != 1 || c.Ordinal != 1 {
		t.Errorf("ordinals = %d, %d, %d; want all 1", a.Ordinal, b.Ordinal, c.Ordinal)
	}
	if r.Len() != 1 {
		t.Errorf("registry holds %d sources, want 1", r.Len())
	}
}

func TestOrdinalsAreAssignedInFirstSeenOrder(t *testing.T) {
	r := NewCitationRegistry()
	first, _ := r.Add(NewCitation(CitationWeb, "https://a", "A"))
	second, _ := r.Add(NewCitation(CitationWeb, "https://b", "B"))
	repeat, _ := r.Add(NewCitation(CitationWeb, "https://a", "A"))

	if first.Ordinal != 1 || second.Ordinal != 2 {
		t.Errorf("ordinals = %d, %d; want 1, 2", first.Ordinal, second.Ordinal)
	}
	if repeat.Ordinal != 1 {
		t.Errorf("repeat ordinal = %d, want the original 1", repeat.Ordinal)
	}
	if first.Marker() != "[1]" || second.Marker() != "[2]" {
		t.Errorf("markers = %q, %q", first.Marker(), second.Marker())
	}
}

func TestURLMatchingIsCaseInsensitive(t *testing.T) {
	r := NewCitationRegistry()
	a, _ := r.Add(NewCitation(CitationWeb, "https://Example.com/Page", "A"))
	b, _ := r.Add(NewCitation(CitationWeb, "https://example.com/page", "A"))
	if a.Ordinal != b.Ordinal {
		t.Error("the same URL in different case must be one source")
	}
}

func TestQuotesAccumulateAgainstOneSource(t *testing.T) {
	r := NewCitationRegistry()
	base := NewCitation(CitationDocument, "file://report.pdf", "Report")

	q1 := base
	q1.Quote = "revenue grew 12%"
	q1.Start, q1.End = 0, 20
	q2 := base
	q2.Quote = "margins narrowed"
	q2.Start, q2.End = 40, 60

	r.Add(q1)
	r.Add(q2)

	if r.Len() != 1 {
		t.Fatalf("sources = %d, want 1", r.Len())
	}
	quotes := r.Quotes(1)
	if len(quotes) != 2 {
		t.Fatalf("quotes = %d, want 2", len(quotes))
	}
	// The canonical source record must not adopt one arbitrary quote as its own.
	src, _ := r.Lookup(1)
	if src.Quote != "" || src.HasSpan() {
		t.Error("the source record holds identity, not one of its quotes")
	}
}

func TestCitationWithNoIdentityIsRejected(t *testing.T) {
	r := NewCitationRegistry()
	if _, ok := r.Add(Citation{Kind: CitationWeb, Quote: "something"}); ok {
		t.Error("a citation with neither URI nor Title must be rejected: it would footnote nothing")
	}
	if r.Len() != 0 {
		t.Error("a rejected citation must not consume an ordinal")
	}
}

func TestATitleArrivingLaterFillsInABareURL(t *testing.T) {
	r := NewCitationRegistry()
	r.Add(NewCitation(CitationWeb, "https://a", ""))
	r.Add(NewCitation(CitationWeb, "https://a", "The Real Title"))

	src, _ := r.Lookup(1)
	if src.Title != "The Real Title" {
		t.Errorf("title = %q, want the one supplied by the later mention", src.Title)
	}
}

func TestSourcesWithoutURIsAreDistinguishedByTitle(t *testing.T) {
	r := NewCitationRegistry()
	a, _ := r.Add(NewCitation(CitationTool, "", "Internal memo 1"))
	b, _ := r.Add(NewCitation(CitationTool, "", "Internal memo 2"))
	if a.Ordinal == b.Ordinal {
		t.Error("two differently-titled sources with no URI must be distinct")
	}
}

func TestUnregisteredCitationRendersNoMarker(t *testing.T) {
	c := NewCitation(CitationWeb, "https://a", "A")
	if c.Marker() != "" {
		t.Errorf("marker = %q, want empty for an unregistered citation, not %q", c.Marker(), "[0]")
	}
}

func TestBibliographyIsOrderedByOrdinal(t *testing.T) {
	r := NewCitationRegistry()
	r.Add(NewCitation(CitationWeb, "https://a", "Alpha"))
	r.Add(NewCitation(CitationWeb, "https://b", "Beta"))

	got := r.Bibliography()
	want := "[1] Alpha - https://a\n[2] Beta - https://b\n"
	if got != want {
		t.Errorf("bibliography =\n%q\nwant\n%q", got, want)
	}
}

func TestConcurrentAddsAssignUniqueOrdinals(t *testing.T) {
	r := NewCitationRegistry()
	const n = 50
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			r.Add(NewCitation(CitationWeb, "https://example.com/"+string(rune('a'+i%26))+string(rune('0'+i/26)), "T"))
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	seen := map[int]bool{}
	for _, s := range r.Sources() {
		if seen[s.Ordinal] {
			t.Fatalf("ordinal %d assigned twice", s.Ordinal)
		}
		seen[s.Ordinal] = true
	}
	if len(seen) != r.Len() {
		t.Errorf("distinct ordinals = %d, sources = %d", len(seen), r.Len())
	}
}
