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

func TestNormalizeModelID(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"anthropic/claude-haiku-4-5-20251001[1m]", "claude-haiku-4-5"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"gpt-5.6-terra", "gpt-5.6-terra"},
		{"claude-x-2025101", "claude-x-2025101"},
		{"claude-x-2025101a", "claude-x-2025101a"},
		{"-20251001", "-20251001"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeModelID(tt.in); got != tt.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	usage := Usage{InputTokens: 1_000_000}
	if got, want := CostFromUsage("claude-haiku-4-5-20251001", usage), 1.0; got != want {
		t.Errorf("dated id cost = %v, want %v", got, want)
	}
}

func TestOneHourCacheWriteSurcharge(t *testing.T) {
	t.Parallel()

	// Opus writes at $6.25/M for five minutes and $10/M for one hour.
	if got, want := oneHourCacheWriteSurcharge("claude-opus-4-8", 1_000_000), 3.75; got != want {
		t.Errorf("surcharge = %v, want %v", got, want)
	}
	for _, model := range []string{"mystery-model", modelGPT54ID} {
		if got := oneHourCacheWriteSurcharge(model, 1_000_000); got != 0 {
			t.Errorf("surcharge(%s) = %v, want 0", model, got)
		}
	}
}
