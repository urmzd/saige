package eval

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
)

// ContentQuality holds deterministic text comparison metrics.
type ContentQuality struct {
	SequenceSimilarity float64 `json:"sequence_similarity"`
	TokenF1            float64 `json:"token_f1"`
	RougeL             float64 `json:"rouge_l"`
}

// ComputeContentQuality compares output text against reference text
// using deterministic string-based metrics.
func ComputeContentQuality(output, reference string) ContentQuality {
	return ContentQuality{
		SequenceSimilarity: SequenceSimilarity(output, reference),
		TokenF1:            TokenF1(output, reference),
		RougeL:             RougeL(output, reference),
	}
}

// SequenceSimilarity computes the ratio of matching characters between two
// strings using a longest-common-subsequence approach (similar to Python's
// difflib.SequenceMatcher).
func SequenceSimilarity(a, b string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0.0
	}
	lcs := lcsLength(a, b)
	return 2.0 * float64(lcs) / float64(len(a)+len(b))
}

// TokenF1 computes word-token-level F1 between two texts. Case and
// punctuation are ignored.
func TokenF1(a, b string) float64 {
	aToks := tokenize(a)
	bToks := tokenize(b)

	if len(aToks) == 0 && len(bToks) == 0 {
		return 1.0
	}
	if len(aToks) == 0 || len(bToks) == 0 {
		return 0.0
	}

	aSet := make(map[string]int, len(aToks))
	for _, t := range aToks {
		aSet[t]++
	}
	bSet := make(map[string]int, len(bToks))
	for _, t := range bToks {
		bSet[t]++
	}

	var overlap int
	for tok, countA := range aSet {
		if countB, ok := bSet[tok]; ok {
			if countA < countB {
				overlap += countA
			} else {
				overlap += countB
			}
		}
	}

	precision := float64(overlap) / float64(len(bToks))
	recall := float64(overlap) / float64(len(aToks))
	if precision+recall == 0 {
		return 0.0
	}
	return 2.0 * precision * recall / (precision + recall)
}

// RougeL computes the ROUGE-L F1 score using longest common subsequence
// at the word-token level. Case and punctuation are ignored.
func RougeL(a, b string) float64 {
	aToks := tokenize(a)
	bToks := tokenize(b)

	if len(aToks) == 0 && len(bToks) == 0 {
		return 1.0
	}
	if len(aToks) == 0 || len(bToks) == 0 {
		return 0.0
	}

	lcs := lcsLengthTokens(aToks, bToks)
	precision := float64(lcs) / float64(len(bToks))
	recall := float64(lcs) / float64(len(aToks))
	if precision+recall == 0 {
		return 0.0
	}
	return 2.0 * precision * recall / (precision + recall)
}

// SequenceSimilarityScorer returns a [Scorer] that computes sequence
// similarity between Output and GroundTruth (both expected as JSON strings).
func SequenceSimilarityScorer() Scorer {
	return NewScorerFunc("sequence_similarity", func(_ context.Context, obs Observation) (Score, error) {
		output, gt, err := extractTextPair(obs)
		if err != nil {
			return Score{}, err
		}
		if output == "" && gt == "" {
			return Score{}, nil
		}
		return Score{Name: "sequence_similarity", Value: SequenceSimilarity(output, gt)}, nil
	})
}

// TokenF1Scorer returns a [Scorer] that computes token F1 between Output
// and GroundTruth.
func TokenF1Scorer() Scorer {
	return NewScorerFunc("token_f1", func(_ context.Context, obs Observation) (Score, error) {
		output, gt, err := extractTextPair(obs)
		if err != nil {
			return Score{}, err
		}
		if output == "" && gt == "" {
			return Score{}, nil
		}
		return Score{Name: "token_f1", Value: TokenF1(output, gt)}, nil
	})
}

// RougeLScorer returns a [Scorer] that computes ROUGE-L between Output
// and GroundTruth.
func RougeLScorer() Scorer {
	return NewScorerFunc("rouge_l", func(_ context.Context, obs Observation) (Score, error) {
		output, gt, err := extractTextPair(obs)
		if err != nil {
			return Score{}, err
		}
		if output == "" && gt == "" {
			return Score{}, nil
		}
		return Score{Name: "rouge_l", Value: RougeL(output, gt)}, nil
	})
}

// extractTextPair unmarshals Output and GroundTruth as plain JSON strings.
// Returns empty strings if either is nil.
func extractTextPair(obs Observation) (string, string, error) {
	var output, gt string
	if obs.Output != nil {
		if err := json.Unmarshal(obs.Output, &output); err != nil {
			return "", "", err
		}
	}
	if obs.GroundTruth != nil {
		if err := json.Unmarshal(obs.GroundTruth, &gt); err != nil {
			return "", "", err
		}
	}
	return output, gt, nil
}

// tokenize splits text into lowercase word tokens. Punctuation separates
// tokens and is dropped, so "Paris." and "paris" are the same token, the
// normalization the SQuAD-style token F1 and ROUGE-L both assume.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// lcsLength computes the length of the longest common subsequence of two strings.
func lcsLength(a, b string) int {
	m, n := len(a), len(b)
	prev := make([]int, n+1)
	curr := make([]int, n+1)

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
			} else {
				if prev[j] > curr[j-1] {
					curr[j] = prev[j]
				} else {
					curr[j] = curr[j-1]
				}
			}
		}
		prev, curr = curr, prev
		for k := range curr {
			curr[k] = 0
		}
	}
	return prev[n]
}

// lcsLengthTokens computes LCS length over string slices.
func lcsLengthTokens(a, b []string) int {
	m, n := len(a), len(b)
	prev := make([]int, n+1)
	curr := make([]int, n+1)

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
			} else {
				if prev[j] > curr[j-1] {
					curr[j] = prev[j]
				} else {
					curr[j] = curr[j-1]
				}
			}
		}
		prev, curr = curr, prev
		for k := range curr {
			curr[k] = 0
		}
	}
	return prev[n]
}
