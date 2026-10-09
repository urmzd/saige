package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/eval"
	"github.com/urmzd/saige/rag/fusion"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/rag/vectorretriever"
)

// The hybrid retrieval eval compares BM25 alone, vector search alone, and
// both fused, on a small fixed corpus. Its queries come in three kinds:
// lexical queries name an identifier or error code, semantic queries
// paraphrase a passage without its words, and mixed queries do some of
// each. Embeddings are recorded in testdata, so the eval runs offline and
// gives the same numbers every time; set SAIGE_EVAL_REGENERATE=1 with
// OPENAI_API_KEY to record them again.

const (
	hybridCorpusPath     = "testdata/hybrid_corpus.json"
	hybridEmbeddingsPath = "testdata/hybrid_embeddings.json"
	// hybridEmbeddingModel and hybridEmbeddingDims describe the recorded
	// embeddings: the model's vectors cut to their leading dimensions and
	// renormalized, which text-embedding-3 models support by design.
	hybridEmbeddingModel = "text-embedding-3-small"
	hybridEmbeddingDims  = 128
	hybridK              = 5
)

type hybridCorpus struct {
	Documents []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Text  string `json:"text"`
	} `json:"documents"`
	Queries []struct {
		Query    string   `json:"query"`
		Kind     string   `json:"kind"`
		Relevant []string `json:"relevant"`
	} `json:"queries"`
}

func loadHybridCorpus(t *testing.T) hybridCorpus {
	t.Helper()
	data, err := os.ReadFile(hybridCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	var c hybridCorpus
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// recordedEmbedder returns the recorded embedding of each text.
type recordedEmbedder map[string][]float32

func (recordedEmbedder) Register(types.ContentType, types.VariantEmbedder) {}

func (e recordedEmbedder) Embed(_ context.Context, variants []types.ContentVariant) ([][]float32, error) {
	out := make([][]float32, len(variants))
	for i, v := range variants {
		vec, ok := e[v.Text]
		if !ok {
			return nil, fmt.Errorf("no recorded embedding for %q", v.Text)
		}
		out[i] = vec
	}
	return out, nil
}

// hybridArm is one retrieval setup under evaluation.
type hybridArm struct {
	name string
	opts func(bm25, vector types.Retriever) []rag.Option
}

var hybridArms = []hybridArm{
	{"bm25", func(bm25, _ types.Retriever) []rag.Option {
		return []rag.Option{rag.WithRetrievers(bm25)}
	}},
	{"vector", func(_, vector types.Retriever) []rag.Option {
		return []rag.Option{rag.WithRetrievers(vector)}
	}},
	{"hybrid-rrf", func(bm25, vector types.Retriever) []rag.Option {
		return []rag.Option{rag.WithRetrievers(vector, bm25)}
	}},
	{"hybrid-minmax", func(bm25, vector types.Retriever) []rag.Option {
		return []rag.Option{rag.WithRetrievers(vector, bm25),
			rag.WithFuser(fusion.Weighted{Normalization: fusion.NormalizeMinMax})}
	}},
	{"hybrid-zscore", func(bm25, vector types.Retriever) []rag.Option {
		return []rag.Option{rag.WithRetrievers(vector, bm25),
			rag.WithFuser(fusion.Weighted{Normalization: fusion.NormalizeZScore})}
	}},
}

// noExtractor satisfies the pipeline; the eval stores documents directly.
type noExtractor struct{}

func (noExtractor) Extract(context.Context, *types.RawDocument) (*types.Document, error) {
	return nil, fmt.Errorf("not used")
}

// hybridScores holds the mean retrieval scores of one arm over a set of
// queries.
type hybridScores struct {
	n                    int
	mrr, ndcg, recall, p float64
}

func (s *hybridScores) add(r eval.RetrievalScores) {
	s.n++
	s.mrr += r.MRR
	s.ndcg += r.NDCG
	s.recall += r.ContextRecall
	s.p += r.ContextPrecision
}

func (s hybridScores) mean() hybridScores {
	n := float64(s.n)
	return hybridScores{n: s.n, mrr: s.mrr / n, ndcg: s.ndcg / n, recall: s.recall / n, p: s.p / n}
}

// runHybridEval scores every arm on every query, grouped by query kind and
// overall ("all").
func runHybridEval(t *testing.T) map[string]map[string]hybridScores {
	t.Helper()
	ctx := context.Background()
	corpus := loadHybridCorpus(t)
	embeddings := loadHybridEmbeddings(t)

	store := memstore.New()
	bm25 := bm25retriever.New(store, nil)
	for _, d := range corpus.Documents {
		vec, ok := embeddings[d.Text]
		if !ok {
			t.Fatalf("no recorded embedding for document %s; regenerate the fixture", d.ID)
		}
		doc := &types.Document{UUID: d.ID, Title: d.Title, Sections: []types.Section{{
			UUID: d.ID + "-s", DocumentUUID: d.ID,
			Variants: []types.ContentVariant{{
				UUID: d.ID + "-v", SectionUUID: d.ID + "-s", ContentType: types.ContentText,
				Text: d.Text, Embedding: vec,
			}},
		}}}
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := bm25.Index(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	vector := vectorretriever.New(store, embeddings)

	out := map[string]map[string]hybridScores{}
	for _, arm := range hybridArms {
		pipe, err := rag.NewPipeline(append([]rag.Option{
			rag.WithStore(store), rag.WithContentExtractor(noExtractor{}),
		}, arm.opts(bm25, vector)...)...)
		if err != nil {
			t.Fatal(err)
		}
		sums := map[string]*hybridScores{}
		for _, q := range corpus.Queries {
			res, err := pipe.Search(ctx, q.Query, types.WithLimit(hybridK))
			if err != nil {
				t.Fatalf("%s: search %q: %v", arm.name, q.Query, err)
			}
			scores, ok := eval.ScoreRetrieval(res.Hits, q.Relevant, eval.RelevanceDocument, hybridK)
			if !ok {
				t.Fatalf("query %q has no relevance labels", q.Query)
			}
			for _, group := range []string{q.Kind, "all"} {
				if sums[group] == nil {
					sums[group] = &hybridScores{}
				}
				sums[group].add(scores)
			}
		}
		out[arm.name] = map[string]hybridScores{}
		for group, s := range sums {
			out[arm.name][group] = s.mean()
		}
	}
	return out
}

// TestHybridRetrievalEval prints the comparison table and checks the
// properties the fixture was built to show: BM25 leads on lexical queries,
// vector search leads on semantic ones, and fusing them beats either alone
// overall.
func TestHybridRetrievalEval(t *testing.T) {
	results := runHybridEval(t)

	var b strings.Builder
	fmt.Fprintf(&b, "\n| arm | queries | MRR | nDCG@%d | recall@%d | AP@%d |\n|---|---|---|---|---|---|\n", hybridK, hybridK, hybridK)
	groups := []string{"all", "lexical", "semantic", "mixed"}
	for _, group := range groups {
		for _, arm := range hybridArms {
			s := results[arm.name][group]
			fmt.Fprintf(&b, "| %s (%s) | %d | %.3f | %.3f | %.3f | %.3f |\n", arm.name, group, s.n, s.mrr, s.ndcg, s.recall, s.p)
		}
	}
	t.Log(b.String())

	ndcg := func(arm, group string) float64 { return results[arm][group].ndcg }
	if ndcg("bm25", "lexical") <= ndcg("vector", "lexical") {
		t.Errorf("lexical nDCG: bm25 %.3f should beat vector %.3f", ndcg("bm25", "lexical"), ndcg("vector", "lexical"))
	}
	if ndcg("vector", "semantic") <= ndcg("bm25", "semantic") {
		t.Errorf("semantic nDCG: vector %.3f should beat bm25 %.3f", ndcg("vector", "semantic"), ndcg("bm25", "semantic"))
	}
	best := math.Max(ndcg("bm25", "all"), ndcg("vector", "all"))
	if ndcg("hybrid-rrf", "all") <= best {
		t.Errorf("overall nDCG: hybrid-rrf %.3f should beat the best single arm %.3f", ndcg("hybrid-rrf", "all"), best)
	}
}

func loadHybridEmbeddings(t *testing.T) recordedEmbedder {
	t.Helper()
	if os.Getenv("SAIGE_EVAL_REGENERATE") == "1" {
		regenerateHybridEmbeddings(t)
	}
	data, err := os.ReadFile(hybridEmbeddingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var e recordedEmbedder
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// regenerateHybridEmbeddings records embeddings for every document and
// query text of the corpus.
func regenerateHybridEmbeddings(t *testing.T) {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("SAIGE_EVAL_REGENERATE needs OPENAI_API_KEY")
	}
	corpus := loadHybridCorpus(t)
	var texts []string
	for _, d := range corpus.Documents {
		texts = append(texts, d.Text)
	}
	for _, q := range corpus.Queries {
		texts = append(texts, q.Query)
	}
	vecs, err := openai.NewEmbedder(key, hybridEmbeddingModel).Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]float32, len(texts))
	for i, v := range vecs {
		out[texts[i]] = truncateEmbedding(v, hybridEmbeddingDims)
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// One text per line keeps diffs readable.
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(out[k])
		fmt.Fprintf(&b, "  %s: %s", kj, vj)
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	if err := os.WriteFile(filepath.Clean(hybridEmbeddingsPath), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// truncateEmbedding keeps the leading dims components of v, renormalized
// to unit length and rounded to four decimals.
func truncateEmbedding(v []float32, dims int) []float32 {
	v = v[:min(dims, len(v))]
	norm := 0.0
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(math.Round(float64(x)/norm*1e4) / 1e4)
	}
	return out
}
