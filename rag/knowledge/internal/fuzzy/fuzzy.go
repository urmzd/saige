// Package fuzzy provides string similarity utilities for entity deduplication.
package fuzzy

import (
	"slices"
	"strings"
	"unicode"
)

// MinFuzzyLength is the shortest normalized name, in runes, that may match
// another name fuzzily. Shorter names ("C", "Go", "AWS") only match exactly:
// one edit is too large a share of them.
const MinFuzzyLength = 4

// Normalize lowercases and keeps letters, digits, '+' and '#'. The two
// symbols are kept because they distinguish names such as "C", "C++" and
// "C#"; all other punctuation and whitespace is dropped.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '#' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LevenshteinDistance computes the edit distance between two strings,
// counting runes rather than bytes.
func LevenshteinDistance(a, b string) int {
	return levenshtein([]rune(a), []rune(b))
}

func levenshtein(a, b []rune) int {
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
	return similarity(Normalize(a), Normalize(b))
}

func similarity(na, nb string) float64 {
	if na == nb {
		return 1.0
	}
	ra, rb := []rune(na), []rune(nb)
	maxLen := max(len(ra), len(rb))
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(levenshtein(ra, rb))/float64(maxLen)
}

// IsFuzzyMatch reports whether two names are similar enough to be the same
// entity. threshold is typically 0.8 for entity names.
//
// Names whose normalized form is empty never match. Names that normalize
// identically always match. Otherwise the names match only when both are at
// least MinFuzzyLength runes long, carry the same digit sequences ("iPhone
// 14" never matches "iPhone 15", "Model 3" never matches "Model S"), and
// their similarity reaches threshold.
func IsFuzzyMatch(a, b string, threshold float64) bool {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	if len([]rune(na)) < MinFuzzyLength || len([]rune(nb)) < MinFuzzyLength {
		return false
	}
	if !slices.Equal(digitRuns(na), digitRuns(nb)) {
		return false
	}
	return similarity(na, nb) >= threshold
}

// digitRuns returns the maximal runs of digits in s, in order.
func digitRuns(s string) []string {
	var runs []string
	start := -1
	for i, r := range s {
		if unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			runs = append(runs, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		runs = append(runs, s[start:])
	}
	return runs
}
