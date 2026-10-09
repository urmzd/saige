package bm25retriever

import (
	"math"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// evaluate scores every variant that matches q. The main clause and Must
// are required, Should adds to the score (and is required only when nothing
// else is), and MustNot excludes. The caller holds r.mu for reading.
func (r *Retriever) evaluate(q types.KeywordQuery) map[string]float64 {
	var must []map[string]float64
	if main := q.Main(); strings.TrimSpace(main.Text) != "" {
		must = append(must, r.clause(main))
	}
	for _, c := range q.Must {
		if strings.TrimSpace(c.Text) != "" {
			must = append(must, r.clause(c))
		}
	}
	var should []map[string]float64
	for _, c := range q.Should {
		if strings.TrimSpace(c.Text) != "" {
			should = append(should, r.clause(c))
		}
	}

	var scores map[string]float64
	if len(must) > 0 {
		scores = must[0]
		for _, m := range must[1:] {
			for uuid, s := range scores {
				extra, ok := m[uuid]
				if !ok {
					delete(scores, uuid)
					continue
				}
				scores[uuid] = s + extra
			}
		}
		for _, m := range should {
			for uuid := range scores {
				scores[uuid] += m[uuid]
			}
		}
	} else {
		scores = make(map[string]float64)
		for _, m := range should {
			for uuid, s := range m {
				scores[uuid] += s
			}
		}
	}
	for _, c := range q.MustNot {
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		for uuid := range r.clause(c) {
			delete(scores, uuid)
		}
	}
	return scores
}

// clause returns the BM25 score of every variant that matches c.
func (r *Retriever) clause(c types.KeywordClause) map[string]float64 {
	tokens := tokenize(c.Text)
	scores := make(map[string]float64)
	if len(tokens) == 0 {
		return scores
	}
	if c.Mode == types.KeywordPhrase && len(tokens) > 1 {
		return r.phrase(tokens, c.Slop)
	}

	// Each query token matches the indexed terms it expands to; a variant
	// scores the best of them, so one token counts once however many terms
	// it expands to.
	matched := make(map[string]int)
	for _, tok := range tokens {
		best := make(map[string]float64)
		for _, term := range r.expand(tok, c) {
			for uuid, s := range r.termScores(term) {
				best[uuid] = max(best[uuid], s)
			}
		}
		for uuid, s := range best {
			scores[uuid] += s
			matched[uuid]++
		}
	}
	if c.Mode == types.KeywordAll {
		for uuid := range scores {
			if matched[uuid] < len(tokens) {
				delete(scores, uuid)
			}
		}
	}
	return scores
}

// expand returns the indexed terms a query token matches under c's prefix
// and fuzziness settings.
func (r *Retriever) expand(tok string, c types.KeywordClause) []string {
	if !c.Prefix && c.Fuzziness == 0 {
		if _, ok := r.index[tok]; ok {
			return []string{tok}
		}
		return nil
	}
	query := []rune(tok)
	var out []string
	for term := range r.index {
		switch {
		case c.Fuzziness == 0:
			if strings.HasPrefix(term, tok) {
				out = append(out, term)
			}
		case editDistance(query, []rune(term), c.Transpositions, c.Prefix) <= c.Fuzziness:
			out = append(out, term)
		}
	}
	return out
}

// termScores returns the BM25 score of term for every variant that holds
// it.
func (r *Retriever) termScores(term string) map[string]float64 {
	postings := r.index[term]
	out := make(map[string]float64, len(postings))
	if len(postings) == 0 {
		return out
	}
	N := float64(r.docCount)
	df := float64(len(postings))
	idf := math.Log(1 + (N-df+0.5)/(df+0.5))
	k1, b := r.cfg.K1, r.cfg.B
	for _, p := range postings {
		dl := r.docLen[p.variantUUID]
		tf := p.termFreq
		out[p.variantUUID] = idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*dl/r.avgDL))
	}
	return out
}

// phrase scores the variants that hold tokens in order. slop is how many
// position moves the match may need: a phrase starting at p matches when
// the sum, over its terms, of the distance between each term's nearest
// position and where the phrase expects it (p plus the term's offset) is at
// most slop. One extra word in between costs 1 and two swapped words cost
// 2.
func (r *Retriever) phrase(tokens []string, slop int) map[string]float64 {
	positions := make([]map[string][]int32, len(tokens))
	scores := make(map[string]float64)
	for i, tok := range tokens {
		positions[i] = make(map[string][]int32)
		for _, p := range r.index[tok] {
			positions[i][p.variantUUID] = p.positions
		}
		for uuid, s := range r.termScores(tok) {
			if i == 0 {
				scores[uuid] = s
			} else if _, ok := scores[uuid]; ok {
				scores[uuid] += s
			}
		}
	}
	for uuid := range scores {
		if !phraseMatches(uuid, positions, slop) {
			delete(scores, uuid)
		}
	}
	return scores
}

func phraseMatches(uuid string, positions []map[string][]int32, slop int) bool {
	for i := range positions {
		if len(positions[i][uuid]) == 0 {
			return false
		}
	}
	for _, start := range positions[0][uuid] {
		cost := 0
		for i := 1; i < len(positions) && cost <= slop; i++ {
			want := start + int32(i)
			nearest := int32(math.MaxInt32)
			for _, p := range positions[i][uuid] {
				nearest = min(nearest, abs32(p-want))
			}
			cost += int(nearest)
		}
		if cost <= slop {
			return true
		}
	}
	return false
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

// editDistance is the Levenshtein distance between a and b, counting an
// adjacent transposition as one edit when transpositions is set. With
// prefix, it is the smallest distance between a and any prefix of b.
func editDistance(a, b []rune, transpositions, prefix bool) int {
	// rows[i][j] is the distance between a[:i] and b[:j].
	rows := make([][]int, len(a)+1)
	for i := range rows {
		rows[i] = make([]int, len(b)+1)
		rows[i][0] = i
	}
	for j := range rows[0] {
		rows[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := min(rows[i-1][j]+1, rows[i][j-1]+1, rows[i-1][j-1]+cost)
			if transpositions && i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d = min(d, rows[i-2][j-2]+1)
			}
			rows[i][j] = d
		}
	}
	last := rows[len(a)]
	if !prefix {
		return last[len(b)]
	}
	best := last[0]
	for _, d := range last {
		best = min(best, d)
	}
	return best
}
