// Example: arxiv: Fetches an arXiv paper abstract, ingests it through the saige
// RAG pipeline, and demonstrates the full search pipeline with chunking, hybrid search
// (BM25 + vector), MMR reranking, citations, lookup, update, evaluation metrics,
// and agent tool registration.
//
// Usage:
//
//	go run ./examples/arxiv
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/tool"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/eval"
	"github.com/urmzd/saige/rag/types"
)

// --- arXiv API types ---

type arxivFeed struct {
	XMLName xml.Name     `xml:"feed"`
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	Title   string `xml:"title"`
	Summary string `xml:"summary"`
	ID      string `xml:"id"`
}

// --- Simple content extractor ---

type paragraphExtractor struct{}

func (e *paragraphExtractor) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	text := string(raw.Data)
	paragraphs := splitParagraphs(text)

	docUUID := newUUID("doc", raw.SourceURI)
	now := time.Now()

	var sections []types.Section
	for i, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		secUUID := newUUID("sec", docUUID, fmt.Sprint(i))
		varUUID := newUUID("var", secUUID, "text")

		sections = append(sections, types.Section{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        i,
			Variants: []types.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: types.ContentText,
				MIMEType:    "text/plain",
				Text:        para,
			}},
		})
	}

	title := ""
	if md := raw.Metadata; md != nil {
		title = md["title"]
	}

	return &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     title,
		Sections:  sections,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func splitParagraphs(text string) []string {
	parts := strings.Split(text, "\n\n")
	if len(parts) <= 1 {
		return splitSentences(text, 3)
	}
	return parts
}

func splitSentences(text string, perChunk int) []string {
	sentences := strings.Split(text, ". ")
	var chunks []string
	for i := 0; i < len(sentences); i += perChunk {
		end := i + perChunk
		if end > len(sentences) {
			end = len(sentences)
		}
		chunk := strings.Join(sentences[i:end], ". ")
		if !strings.HasSuffix(chunk, ".") {
			chunk += "."
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// --- Simple bag-of-words embedder ---

const embedDim = 256

type bowEmbedderRegistry struct{}

func (r *bowEmbedderRegistry) Register(_ types.ContentType, _ types.VariantEmbedder) {}

func (r *bowEmbedderRegistry) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	result := make([][]float32, len(variants))
	for i, v := range variants {
		result[i] = embedText(v.Text)
	}
	return result, nil
}

func embedText(text string) []float32 {
	vec := make([]float32, embedDim)
	words := strings.Fields(strings.ToLower(text))
	for _, w := range words {
		h := fnv.New32a()
		h.Write([]byte(w))
		idx := h.Sum32() % uint32(embedDim)
		vec[idx] += 1.0
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vec
}

// --- Helpers ---

func newUUID(parts ...string) string {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write([]byte(p))
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

func main() {
	ctx := context.Background()

	// 1. Fetch the "Attention Is All You Need" paper abstract from arXiv.
	fmt.Println("Fetching paper from arXiv...")
	raw, err := fetchArxivPaper("1706.03762")
	if err != nil {
		log.Fatalf("fetch arxiv: %v", err)
	}
	fmt.Printf("Fetched: %s (%d bytes)\n\n", raw.Metadata["title"], len(raw.Data))

	// 2. Build the pipeline with hybrid search (vector + BM25), recursive chunking, and MMR reranking.
	// For persistent storage with HNSW vector search, use PostgreSQL + pgvector:
	//   pool, _ := postgres.NewPool(ctx, postgres.Config{URL: os.Getenv("DATABASE_URL")})
	//   postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{RAGEmbeddingDim: embedDim})
	//   store := ragpgstore.NewStore(pool, nil)
	store := memstore.New()
	pipe, err := rag.NewPipeline(
		rag.WithStore(store),
		rag.WithContentExtractor(&paragraphExtractor{}),
		rag.WithEmbedders(&bowEmbedderRegistry{}),
		rag.WithRecursiveChunker(256, 25),
		rag.WithBM25(nil),
		rag.WithMMR(0.7),
	)
	if err != nil {
		log.Fatalf("create pipeline: %v", err)
	}
	defer pipe.Close(ctx)

	// 3. Ingest the paper.
	fmt.Println("Ingesting with recursive chunking + BM25 indexing...")
	result, err := pipe.Ingest(ctx, raw)
	if err != nil {
		log.Fatalf("ingest: %v", err)
	}
	fmt.Printf("Ingested: doc=%s sections=%d variants=%d\n\n",
		result.DocumentUUID, result.Sections, result.Variants)

	// 4. Hybrid search with context assembly: vector + BM25 via RRF.
	fmt.Println("=== Hybrid Search (Vector + BM25) with Citations ===")
	sr, err := pipe.Search(ctx, "attention mechanism self-attention",
		types.WithLimit(3),
		types.WithContextAssembly(4096),
	)
	if err != nil {
		log.Fatalf("search: %v", err)
	}

	fmt.Printf("Query: %q\n", sr.Query)
	fmt.Printf("Hits: %d\n\n", len(sr.Hits))
	for i, hit := range sr.Hits {
		text := hit.Variant.Text
		if len(text) > 120 {
			text = text[:120] + "..."
		}
		fmt.Printf("  %d. [score=%.4f] %s\n", i+1, hit.Score, text)
		fmt.Printf("     Source: %s | Section: %d\n", hit.Provenance.SourceURI, hit.Provenance.SectionIndex)
	}

	if sr.Context != nil {
		fmt.Printf("\n--- Assembled Context (%d tokens) ---\n", sr.Context.TokenCount)
		fmt.Println(sr.Context.Prompt)
		fmt.Printf("\nCitation blocks: %d\n", len(sr.Context.Blocks))
		for _, block := range sr.Context.Blocks {
			fmt.Printf("  %s → %s\n", block.Citation, block.Provenance.SourceURI)
		}
	}
	fmt.Println()

	// 5. BM25-only exact-term search demonstration.
	fmt.Println("=== BM25 Exact-Term Search ===")
	sr2, err := pipe.Search(ctx, "28.4 BLEU", types.WithLimit(3))
	if err != nil {
		log.Fatalf("bm25 search: %v", err)
	}
	fmt.Printf("Query: %q → %d hits\n", "28.4 BLEU", len(sr2.Hits))
	for i, hit := range sr2.Hits {
		text := hit.Variant.Text
		if len(text) > 120 {
			text = text[:120] + "..."
		}
		fmt.Printf("  %d. [score=%.4f] %s\n", i+1, hit.Score, text)
	}
	fmt.Println()

	// 6. Lookup by variant UUID: show dereference pattern.
	if len(sr.Hits) > 0 {
		variantUUID := sr.Hits[0].Variant.UUID
		fmt.Printf("=== Lookup variant %s ===\n", variantUUID)
		hit, err := pipe.Lookup(ctx, variantUUID)
		if err != nil {
			log.Fatalf("lookup: %v", err)
		}
		fmt.Printf("Content type: %s\n", hit.Variant.ContentType)
		fmt.Printf("Document: %s\n", hit.Provenance.DocumentTitle)
		text := hit.Variant.Text
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		fmt.Printf("Text: %s\n\n", text)
	}

	// 7. Update document: re-ingest.
	fmt.Println("=== Update Document ===")
	updatedRaw := &types.RawDocument{
		SourceURI: raw.SourceURI,
		MIMEType:  raw.MIMEType,
		Data:      append(raw.Data, []byte("\n\nUpdated: This paper introduced the Transformer architecture.")...),
		Metadata:  raw.Metadata,
	}
	updateResult, err := pipe.Update(ctx, result.DocumentUUID, updatedRaw)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	fmt.Printf("Updated: doc=%s sections=%d variants=%d\n\n",
		updateResult.DocumentUUID, updateResult.Sections, updateResult.Variants)

	// 8. Re-search to show updated results.
	fmt.Println("=== Re-search after update ===")
	sr3, err := pipe.Search(ctx, "Transformer architecture", types.WithLimit(2))
	if err != nil {
		log.Fatalf("re-search: %v", err)
	}
	for i, hit := range sr3.Hits {
		text := hit.Variant.Text
		if len(text) > 120 {
			text = text[:120] + "..."
		}
		fmt.Printf("  %d. [score=%.4f] %s\n", i+1, hit.Score, text)
	}
	fmt.Println()

	// 9. Evaluation metrics demo.
	fmt.Println("=== Evaluation Metrics ===")
	var relevantUUIDs []string
	for _, hit := range sr.Hits {
		relevantUUIDs = append(relevantUUIDs, hit.Variant.UUID)
	}
	if len(relevantUUIDs) > 0 {
		precision := eval.ContextPrecision(sr.Hits, relevantUUIDs)
		recall := eval.ContextRecall(sr.Hits, relevantUUIDs)
		ndcg := eval.NDCG(sr.Hits, relevantUUIDs, 10)
		mrr := eval.MRR(sr.Hits, relevantUUIDs)
		hitRate := eval.HitRate(sr.Hits, relevantUUIDs, 10)
		fmt.Printf("Context Precision: %.3f\n", precision)
		fmt.Printf("Context Recall:    %.3f\n", recall)
		fmt.Printf("NDCG@10:           %.3f\n", ndcg)
		fmt.Printf("MRR:               %.3f\n", mrr)
		fmt.Printf("Hit Rate@10:       %.3f\n", hitRate)
	}
	fmt.Println()

	// 10. Show agent tool registration pattern.
	fmt.Println("=== Agent Tool Registration ===")
	tools := tool.NewTools(pipe)
	for _, tool := range tools {
		def := tool.Definition()
		schema, _ := json.MarshalIndent(def.Parameters, "  ", "  ")
		fmt.Printf("Tool: %s\n  Description: %s\n  Parameters: %s\n\n",
			def.Name, def.Description, schema)
	}

	// 11. Reconstruct the document.
	fmt.Println("=== Reconstructed Document ===")
	doc, err := pipe.Reconstruct(ctx, updateResult.DocumentUUID)
	if err != nil {
		log.Fatalf("reconstruct: %v", err)
	}
	fmt.Printf("Title: %s\n", doc.Title)
	fmt.Printf("Source: %s\n", doc.SourceURI)
	fmt.Printf("Sections: %d\n", len(doc.Sections))
	for _, sec := range doc.Sections {
		text := sec.Variants[0].Text
		if len(text) > 80 {
			text = text[:80] + "..."
		}
		fmt.Printf("  [%d] %s\n", sec.Index, text)
	}
}

func fetchArxivPaper(arxivID string) (*types.RawDocument, error) {
	url := fmt.Sprintf("http://export.arxiv.org/api/query?id_list=%s", arxivID)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parse XML: %w", err)
	}

	if len(feed.Entries) == 0 {
		return nil, fmt.Errorf("no entries found for %s", arxivID)
	}

	entry := feed.Entries[0]
	title := strings.TrimSpace(entry.Title)
	abstract := strings.TrimSpace(entry.Summary)

	return &types.RawDocument{
		SourceURI: fmt.Sprintf("https://arxiv.org/abs/%s", arxivID),
		MIMEType:  "text/plain",
		Data:      []byte(abstract),
		Metadata: map[string]string{
			"title":    title,
			"arxiv_id": arxivID,
		},
	}, nil
}
