package analysis

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestZ(t *testing.T) {
	tests := []struct {
		confidence, want float64
	}{
		{0.95, 1.959964},
		{0.90, 1.644854},
		{0.99, 2.575829},
		{0, 1.959964},   // invalid falls back to the default
		{1.5, 1.959964}, // invalid falls back to the default
	}
	for _, tt := range tests {
		if got := Z(tt.confidence); !near(got, tt.want, 1e-5) {
			t.Errorf("Z(%v) = %v, want %v", tt.confidence, got, tt.want)
		}
	}
}

func TestWilson(t *testing.T) {
	tests := []struct {
		name          string
		successes, n  int
		wantLow, want float64
	}{
		{"none pass", 0, 10, 0, 0.2775},
		{"half pass", 5, 10, 0.2366, 0.7634},
		{"all pass", 10, 10, 0.7225, 1},
		{"no trials", 0, 0, 0, 1},
		{"successes clamped to n", 12, 10, 0.7225, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high := Wilson(tt.successes, tt.n, 0.95)
			if !near(low, tt.wantLow, 1e-3) || !near(high, tt.want, 1e-3) {
				t.Errorf("Wilson(%d, %d) = [%v, %v], want [%v, %v]", tt.successes, tt.n, low, high, tt.wantLow, tt.want)
			}
		})
	}
}

func TestRateAndCompleteness(t *testing.T) {
	s := Rate(3, 4, 8, 0.95)
	if s.N != 4 || s.Value != 0.75 || s.Completeness != 0.5 {
		t.Fatalf("Rate = %+v, want N=4 Value=0.75 Completeness=0.5", s)
	}
	if !s.Contains(0.75) || s.Contains(0.1) {
		t.Errorf("interval [%v, %v] should contain 0.75 and not 0.1", s.Low, s.High)
	}
	if got := Completeness(3, 0); got != 0 {
		t.Errorf("Completeness(3, 0) = %v, want 0", got)
	}
}

func TestMeanStat(t *testing.T) {
	tests := []struct {
		name               string
		values             []float64
		wantValue, wantLow float64
		wantHigh           float64
	}{
		{"empty", nil, 0, 0, 0},
		{"single collapses", []float64{0.4}, 0.4, 0.4, 0.4},
		{"constant has zero width", []float64{1, 1, 1}, 1, 1, 1},
		// mean 2.5, sample sd 1.2910, se 0.6455, half 1.2652
		{"spread", []float64{1, 2, 3, 4}, 2.5, 1.2348, 3.7652},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := MeanStat(tt.values, len(tt.values), 0.95)
			if !near(s.Value, tt.wantValue, 1e-3) || !near(s.Low, tt.wantLow, 1e-3) || !near(s.High, tt.wantHigh, 1e-3) {
				t.Errorf("MeanStat = %+v, want value %v in [%v, %v]", s, tt.wantValue, tt.wantLow, tt.wantHigh)
			}
		})
	}
}

func TestQuantile(t *testing.T) {
	values := []float64{4, 1, 3, 2}
	tests := []struct{ q, want float64 }{
		{0, 1}, {0.5, 2.5}, {1, 4}, {0.25, 1.75}, {-1, 1}, {2, 4},
	}
	for _, tt := range tests {
		if got := Quantile(values, tt.q); !near(got, tt.want, 1e-9) {
			t.Errorf("Quantile(%v) = %v, want %v", tt.q, got, tt.want)
		}
	}
	if values[0] != 4 {
		t.Error("Quantile modified its input")
	}
	if got := Quantile(nil, 0.5); got != 0 {
		t.Errorf("Quantile(nil) = %v, want 0", got)
	}
}

func TestPassAtK(t *testing.T) {
	tests := []struct {
		name    string
		n, c, k int
		want    float64
		wantNaN bool
	}{
		{"pass@1 is the pass rate", 10, 3, 1, 0.3, false},
		{"pass@5", 10, 3, 5, 1 - 21.0/252.0, false},
		{"never passes", 10, 0, 3, 0, false},
		{"always passes", 5, 5, 1, 1, false},
		{"too few failures to fill k", 4, 3, 2, 1, false},
		{"k above n", 3, 1, 4, 0, true},
		{"k zero", 3, 1, 0, 0, true},
		{"c above n", 3, 4, 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PassAtK(tt.n, tt.c, tt.k)
			if tt.wantNaN {
				if !math.IsNaN(got) {
					t.Errorf("PassAtK = %v, want NaN", got)
				}
				return
			}
			if !near(got, tt.want, 1e-9) {
				t.Errorf("PassAtK(%d, %d, %d) = %v, want %v", tt.n, tt.c, tt.k, got, tt.want)
			}
		})
	}
}

func TestPairedDiff(t *testing.T) {
	tests := []struct {
		name              string
		p                 Paired
		want, low, high   float64
		excludesZero      bool
		wantRateA, rateB  float64
		wantPValueBelow05 bool
	}{
		{
			name: "candidate fixes ten cases", p: Paired{Both: 40, OnlyB: 10},
			want: 0.2, low: 0.0891, high: 0.3109, excludesZero: true,
			wantRateA: 0.8, rateB: 1, wantPValueBelow05: true,
		},
		{
			name: "balanced disagreement", p: Paired{Both: 20, OnlyA: 5, OnlyB: 5, Neither: 20},
			want: 0, low: -0.124, high: 0.124,
			wantRateA: 0.5, rateB: 0.5,
		},
		{
			name: "identical arms", p: Paired{Both: 7, Neither: 3},
			want: 0, low: 0, high: 0,
			wantRateA: 0.7, rateB: 0.7,
		},
		{
			name: "no pairs", p: Paired{},
			want: 0, low: -1, high: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.p.Diff(0.95)
			if !near(s.Value, tt.want, 1e-3) || !near(s.Low, tt.low, 1e-3) || !near(s.High, tt.high, 1e-3) {
				t.Errorf("Diff = %+v, want %v in [%v, %v]", s, tt.want, tt.low, tt.high)
			}
			if got := !s.Contains(0); got != tt.excludesZero {
				t.Errorf("excludes zero = %v, want %v", got, tt.excludesZero)
			}
			if !near(tt.p.RateA(), tt.wantRateA, 1e-9) || !near(tt.p.RateB(), tt.rateB, 1e-9) {
				t.Errorf("rates = %v, %v, want %v, %v", tt.p.RateA(), tt.p.RateB(), tt.wantRateA, tt.rateB)
			}
			if got := tt.p.McNemarP() < 0.05; got != tt.wantPValueBelow05 {
				t.Errorf("McNemarP = %v, below 0.05 = %v, want %v", tt.p.McNemarP(), got, tt.wantPValueBelow05)
			}
		})
	}
}

func TestMcNemarP(t *testing.T) {
	tests := []struct {
		onlyA, onlyB int
		want         float64
	}{
		{0, 10, 2.0 / 1024.0},
		{10, 0, 2.0 / 1024.0},
		{5, 5, 1},
		{0, 0, 1},
		{1, 4, 0.375}, // 2 * (1 + 5) / 32
	}
	for _, tt := range tests {
		p := Paired{OnlyA: tt.onlyA, OnlyB: tt.onlyB}
		if got := p.McNemarP(); !near(got, tt.want, 1e-9) {
			t.Errorf("McNemarP(%d, %d) = %v, want %v", tt.onlyA, tt.onlyB, got, tt.want)
		}
	}
}

func TestBootstrap(t *testing.T) {
	tests := []struct {
		name         string
		values       []float64
		containsZero bool
	}{
		{"constant shift excludes zero", []float64{0.3, 0.3, 0.3, 0.3, 0.3}, false},
		{"symmetric noise contains zero", []float64{-1, 1, -1, 1, -1, 1, -1, 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			low, high := Bootstrap(tt.values, 1000, 0.95, 42)
			if got := low <= 0 && 0 <= high; got != tt.containsZero {
				t.Errorf("[%v, %v] contains zero = %v, want %v", low, high, got, tt.containsZero)
			}
			low2, high2 := Bootstrap(tt.values, 1000, 0.95, 42)
			if low != low2 || high != high2 {
				t.Error("same seed gave a different interval")
			}
		})
	}
	if low, high := Bootstrap([]float64{0.5}, 1000, 0.95, 42); low != 0.5 || high != 0.5 {
		t.Errorf("single value interval = [%v, %v], want [0.5, 0.5]", low, high)
	}
}
