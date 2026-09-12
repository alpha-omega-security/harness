package harness

import (
	"math"
	"testing"
)

func TestCostFromUsage_gpt56SolAndDaybreakBasePricing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{"uncached input", Usage{InputTokens: 100_000}, 0.40},
		{"output", Usage{OutputTokens: 10_000}, 0.20},
		{"cache reads", Usage{InputTokens: 100_000, CacheReadTokens: 100_000}, 0.04},
		{"cache writes", Usage{InputTokens: 100_000, CacheWriteTokens: 100_000}, 0.50},
		{"standard scan", Usage{InputTokens: 100_000, OutputTokens: 10_000}, 0.60},
		{"mixed", Usage{InputTokens: 100_000, OutputTokens: 10_000, CacheReadTokens: 10_000, CacheWriteTokens: 20_000}, 0.584},
	}
	for _, model := range []struct{ name, id string }{
		{"sol", "gpt-5.6-sol"},
		{"sol_normalized", "openai/gpt-5.6-sol[1m]"},
		{"daybreak", "gpt-daybreak-blue-latest"},
		{"daybreak_normalized", "openai/gpt-daybreak-blue-latest[1m]"},
	} {
		t.Run(model.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					if got := CostFromUsage(model.id, tt.usage); math.Abs(got-tt.want) > 1e-9 {
						t.Errorf("CostFromUsage(%q, %+v) = %v, want %v", model.id, tt.usage, got, tt.want)
					}
				})
			}
		})
	}
}
