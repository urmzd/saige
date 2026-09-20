// Package fuzzy provides string similarity utilities for entity deduplication.
package fuzzy

import (
	"strings"
	"unicode"
)

// Normalize lowercases and strips non-alphanumeric characters for comparison.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LevenshteinDistance computes the edit distance between two strings.
func LevenshteinDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// Similarity returns a 0-1 score (1 = identical) based on normalized Levenshtein distance.
func Similarity(a, b string) float64 {
	na, nb := Normalize(a), Normalize(b)
	if na == nb {
		return 1.0
	}
	maxLen := max(len(na), len(nb))
	if maxLen == 0 {
		return 1.0
	}
	dist := LevenshteinDistance(na, nb)
	return 1.0 - float64(dist)/float64(maxLen)
}

// IsFuzzyMatch returns true if two names are similar enough to be the same entity.
// threshold is typically 0.8 for entity names.
func IsFuzzyMatch(a, b string, threshold float64) bool {
	return Similarity(a, b) >= threshold
}
