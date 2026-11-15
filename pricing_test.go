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
	if got, want := oneHourCacheWriteSurcharge("claude-opus-4-8", 1_000_000, 1_000_000), 3.75; got != want {
		t.Errorf("surcharge = %v, want %v", got, want)
	}
	for _, model := range []string{"mystery-model", modelGPT54ID} {
		if got := oneHourCacheWriteSurcharge(model, 1_000_000, 1_000_000); got != 0 {
			t.Errorf("surcharge(%s) = %v, want 0", model, got)
		}
	}
	// Haiku 5.5 writes at $0.125/M then $0.20/M up to 100k prompt tokens, and
	// at $0.625/M then $1/M above.
	for _, tt := range []struct {
		prompt int
		want   float64
	}{{100_000, 0.075}, {100_001, 0.375}} {
		if got := oneHourCacheWriteSurcharge("claude-haiku-5-5", 1_000_000, tt.prompt); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("haiku 5.5 surcharge at %d prompt tokens = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}

func TestCostFromUsage_claude55Models(t *testing.T) {
	t.Parallel()

	usage := Usage{
		InputTokens:      1_000_000,
		OutputTokens:     1_000_000,
		CacheReadTokens:  200_000,
		CacheWriteTokens: 100_000,
	}
	for _, tt := range []struct {
		model string
		want  float64
	}{
		{"claude-opus-5-5", 23.34},
		{"claude-opus-5.5", 23.34},
		{"claude-sonnet-5-5", 11.67},
		{"claude-sonnet-5.5", 11.67},
		{"claude-sonnet-5", 11.69},
		{"claude-fable-5-2[1m]", 46.64},
		{"claude-fable-5.2", 46.64},
		// A result can sum many requests, so it stays at the base rate.
		{"claude-haiku-5-5", 0.5845},
		{"claude-haiku-5.5", 0.5845},
	} {
		if got := CostFromUsage(tt.model, usage); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("CostFromUsage(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestRequestCost_longPrompt(t *testing.T) {
	t.Parallel()

	usage := Usage{InputTokens: 100_000, OutputTokens: 10_000, CacheReadTokens: 50_000, CacheWriteTokens: 20_000}
	for _, tt := range []struct {
		model  string
		prompt int
		want   float64
	}{
		{"claude-haiku-5-5", 100_000, 0.011},
		{"claude-haiku-5-5", 100_001, 0.055},
		{"claude-haiku-5.5", 100_001, 0.055},
		{"claude-opus-5-5", 100_001, 0.43},
		{"unknown", 100_001, 0},
	} {
		if got := requestCost(tt.model, usage, tt.prompt); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("requestCost(%q, prompt %d) = %v, want %v", tt.model, tt.prompt, got, tt.want)
		}
	}
}
