package fuzzy

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "helloworld"},
		{"  spaces  ", "spaces"},
		{"special!@#chars$%^", "special#chars"},
		{"C++", "c++"},
		{"C#", "c#"},
		{"!!!", ""},
		{"MiXeD CaSe 123", "mixedcase123"},
		{"", ""},
		{"日本語", "日本語"},
		{"hyphen-ated", "hyphenated"},
		{"under_score", "underscore"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := Normalize(tt.input)
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"kitten", "sitting", 3},
		{"sunday", "saturday", 3},
		{"a", "b", 1},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := LevenshteinDistance(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("LevenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSimilarity(t *testing.T) {
	tests := []struct {
		a, b   string
		minSim float64
		maxSim float64
	}{
		{"abc", "abc", 1.0, 1.0},
		{"ABC", "abc", 1.0, 1.0},        // case-insensitive via Normalize
		{"", "", 1.0, 1.0},              // both empty
		{"OpenAI", "Open AI", 1.0, 1.0}, // normalize strips space
		{"John", "Jon", 0.5, 0.9},
		{"completely", "different", 0.0, 0.5},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := Similarity(tt.a, tt.b)
			if got < tt.minSim || got > tt.maxSim {
				t.Errorf("Similarity(%q, %q) = %f, want in [%f, %f]", tt.a, tt.b, got, tt.minSim, tt.maxSim)
			}
		})
	}
}

func TestIsFuzzyMatch(t *testing.T) {
	tests := []struct {
		a, b      string
		threshold float64
		want      bool
	}{
		{"OpenAI", "OpenAI", 0.8, true},
		{"OpenAI", "Open AI", 0.8, true},
		{"John Smith", "Jon Smith", 0.8, true},
		{"apple", "orange", 0.8, false},
		{"abc", "xyz", 0.8, false},
		{"abcd", "abce", 0.5, true},
		// Regressions: names that look alike but are distinct entities.
		{"C++", "C", 0.5, false},
		{"C#", "C", 0.5, false},
		{"C++", "C#", 0.5, false},
		{"iPhone 14", "iPhone 15", 0.8, false},
		{"Model 3", "Model S", 0.8, false},
		{"Windows 10", "Windows 11", 0.8, false},
		{"!!!", "???", 0.0, false},
		{"", "", 0.0, false},
		{"abc", "abd", 0.5, false}, // too short for a fuzzy match
		{"Go", "go", 0.8, true},    // identical after normalizing
		{"iPhone 14", "iphone14", 0.8, true},
	}

	for _, tt := range tests {
		t.Run(tt.a+"_"+tt.b, func(t *testing.T) {
			got := IsFuzzyMatch(tt.a, tt.b, tt.threshold)
			if got != tt.want {
				t.Errorf("IsFuzzyMatch(%q, %q, %f) = %v, want %v", tt.a, tt.b, tt.threshold, got, tt.want)
			}
		})
	}
}
