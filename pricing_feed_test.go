package harness

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"testing"
)

// priceFeed is the part of https://models.dev/api.json the price check reads:
// provider id, then model id, then list prices in USD per million tokens.
type priceFeed map[string]struct {
	Models map[string]struct {
		Cost *feedCost `json:"cost"`
	} `json:"models"`
}

type feedRates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type feedCost struct {
	feedRates
	Tiers []feedTier `json:"tiers"`
}

// feedTier holds the rates for a request whose prompt exceeds Tier.Size.
type feedTier struct {
	feedRates
	Tier struct {
		Type string `json:"type"`
		Size int    `json:"size"`
	} `json:"tier"`
}

// feedProviders names the feed provider that lists each backend's models.
var feedProviders = map[string]string{
	"claude":   "anthropic",
	"codex":    "openai",
	"copilot":  "github-copilot",
	"opencode": "anthropic",
}

// acceptedDrift is an intended difference: the detail pricingDrift reports for
// it, and why it is accepted.
type acceptedDrift struct {
	detail, reason string
}

// acceptedPriceDrift lists differences from the feed that are intended, keyed
// as pricingDrift reports them. A difference whose detail changes is reported,
// and so is an entry that stops matching a difference.
var acceptedPriceDrift = map[string]acceptedDrift{
	"github-copilot/claude-opus-4.6 missing":         {"feed has no price", "listed by Copilot CLI, absent from the feed"},
	"github-copilot/gemini-3.1-pro-preview missing":  {"feed has no price", "listed by Copilot CLI, absent from the feed"},
	"github-copilot/mai-code-1-flash-picker missing": {"feed has no price", "listed by Copilot CLI, absent from the feed"},

	"github-copilot/gemini-3.6-flash cache_read":    {"table 0.07, feed 0.075", "Copilot CLI reports 0.07"},
	"github-copilot/gemini-3.7-flash cache_read":    {"table 0.07, feed 0.075", "Copilot CLI reports 0.07"},
	"github-copilot/mai-code-1.1-flash cache_write": {"table 0.25, feed 0", "Copilot CLI reports a write rate"},

	"github-copilot/gpt-5.6-luna long_prompt": {
		"table over 272000 tokens in=0.4 out=1.8 cache_read=0.04 cache_write=0.5, feed over 200000 tokens in=0.4 out=1.8 cache_read=0.04 cache_write=0.5",
		"OpenAI's threshold is 272k, the feed's Copilot entry says 200k",
	},
	"github-copilot/grok-4.5 long_prompt": {
		"table none, feed over 200000 tokens in=4 out=12 cache_read=1 cache_write=0",
		"Copilot CLI reports one rate",
	},
	"github-copilot/grok-4.6 long_prompt": {
		"table none, feed over 200000 tokens in=4 out=12 cache_read=1 cache_write=0",
		"Copilot CLI reports one rate",
	},
}

// pricingDrift compares modelPricing with the feed for every default model of
// every backend. It returns one line per difference that accepted does not
// list with the same detail, and one per accepted entry that matched nothing.
func pricingDrift(feed priceFeed, accepted map[string]acceptedDrift) []string {
	found := map[string]string{}
	for _, name := range backendNames() {
		provider, ok := feedProviders[name]
		if !ok {
			found[name+" provider"] = "backend has no feed provider"
			continue
		}
		for _, model := range harnesses[name].DefaultModels() {
			id := normalizeModelID(model.ID)
			for field, detail := range modelDrift(feed, provider, id) {
				found[provider+"/"+id+" "+field] = detail
			}
		}
	}

	var lines []string
	for key, detail := range found {
		if accepted[key].detail != detail {
			lines = append(lines, key+": "+detail)
		}
	}
	for key, entry := range accepted {
		if _, ok := found[key]; !ok {
			lines = append(lines, key+": accepted difference no longer applies ("+entry.reason+")")
		}
	}
	sort.Strings(lines)
	return lines
}

// backendNames lists the registered backends in a stable order.
func backendNames() []string {
	var names []string
	for name := range harnesses {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// modelDrift returns the differences for one model, keyed by field.
func modelDrift(feed priceFeed, provider, id string) map[string]string {
	price, priced := lookupPrice(id)
	entry, listed := feed[provider].Models[id]
	switch {
	case !priced:
		return map[string]string{"unpriced": "default model has no row in modelPricing"}
	case !listed || entry.Cost == nil:
		return map[string]string{"missing": "feed has no price"}
	}

	drift := map[string]string{}
	rates := []struct {
		field       string
		table, feed float64
	}{
		{"input", price.In, entry.Cost.Input},
		{"output", price.Out, entry.Cost.Output},
		{"cache_read", price.CachedIn, entry.Cost.CacheRead},
		{"cache_write", price.CacheWrite, entry.Cost.CacheWrite},
	}
	for _, rate := range rates {
		if !sameRate(rate.table, rate.feed) {
			drift[rate.field] = fmt.Sprintf("table %v, feed %v", rate.table, rate.feed)
		}
	}
	if table, feed := describeLongPrompt(price), describeTiers(entry.Cost.Tiers); table != feed {
		drift["long_prompt"] = fmt.Sprintf("table %s, feed %s", table, feed)
	}
	return drift
}

func sameRate(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func describeLongPrompt(price modelPrice) string {
	if price.LongPrompt == nil {
		return "none"
	}
	long := price.LongPrompt
	return describeTier(price.LongPromptOver, feedRates{long.In, long.Out, long.CachedIn, long.CacheWrite})
}

func describeTiers(tiers []feedTier) string {
	if len(tiers) == 0 {
		return "none"
	}
	parts := make([]string, len(tiers))
	for i, tier := range tiers {
		parts[i] = describeTier(tier.Tier.Size, tier.feedRates)
		if tier.Tier.Type != "context" {
			parts[i] += " by " + tier.Tier.Type
		}
	}
	return strings.Join(parts, " and ")
}

func describeTier(over int, rates feedRates) string {
	return fmt.Sprintf("over %d tokens in=%v out=%v cache_read=%v cache_write=%v",
		over, rates.Input, rates.Output, rates.CacheRead, rates.CacheWrite)
}

// fixtureFeed maps a provider and model id to the model's cost object. A nil
// cost is a model the feed lists without a price.
type fixtureFeed map[string]map[string]map[string]any

// feedFromTable builds a feed that agrees with modelPricing for every default
// model.
func feedFromTable(t *testing.T) fixtureFeed {
	t.Helper()

	feed := fixtureFeed{}
	for _, name := range backendNames() {
		provider := feedProviders[name]
		if feed[provider] == nil {
			feed[provider] = map[string]map[string]any{}
		}
		for _, model := range harnesses[name].DefaultModels() {
			id := normalizeModelID(model.ID)
			price, ok := lookupPrice(id)
			if !ok {
				t.Fatalf("%s default model %q has no price", name, id)
			}
			cost := map[string]any{"input": price.In, "output": price.Out, "cache_read": price.CachedIn}
			if price.CacheWrite > 0 {
				cost["cache_write"] = price.CacheWrite
			}
			if long := price.LongPrompt; long != nil {
				cost["tiers"] = []map[string]any{fixtureTier(price.LongPromptOver, long.In, long.Out, long.CachedIn, long.CacheWrite)}
			}
			feed[provider][id] = cost
		}
	}
	return feed
}

func fixtureTier(over int, in, out, cacheRead, cacheWrite float64) map[string]any {
	return map[string]any{
		"input": in, "output": out, "cache_read": cacheRead, "cache_write": cacheWrite,
		"tier": map[string]any{"type": "context", "size": over},
	}
}

// decode renders the fixture as feed JSON and parses it the way the live check
// does.
func (feed fixtureFeed) decode(t *testing.T) priceFeed {
	t.Helper()

	doc := map[string]any{}
	for provider, models := range feed {
		entries := map[string]any{}
		for id, cost := range models {
			entries[id] = map[string]any{"cost": cost}
		}
		doc[provider] = map[string]any{"models": entries}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded priceFeed
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestAcceptedPriceDrift_namesDefaultModels(t *testing.T) {
	t.Parallel()

	defaults := map[string]bool{}
	for _, name := range backendNames() {
		for _, model := range harnesses[name].DefaultModels() {
			defaults[feedProviders[name]+"/"+normalizeModelID(model.ID)] = true
		}
	}
	for key, entry := range acceptedPriceDrift {
		model, field, ok := strings.Cut(key, " ")
		if !ok || !defaults[model] || field == "" || entry.detail == "" || entry.reason == "" {
			t.Errorf("accepted difference %q does not name a default model, a field, a detail and a reason", key)
		}
	}
}

func TestPricingDrift_matchingFeed(t *testing.T) {
	t.Parallel()

	if got := pricingDrift(feedFromTable(t).decode(t), nil); len(got) != 0 {
		t.Errorf("pricingDrift() = %q, want none", got)
	}
}

func TestPricingDrift_reportsDifferences(t *testing.T) {
	t.Parallel()

	feed := feedFromTable(t)
	anthropic := feed["anthropic"]
	anthropic["claude-opus-5-5"]["input"] = 3.5
	anthropic["claude-sonnet-5-5"]["cache_write"] = 9.0
	delete(anthropic["claude-haiku-5-5"], "tiers")
	delete(anthropic, "claude-fable-5-2")
	anthropic["claude-opus-4-8"]["tiers"] = []map[string]any{fixtureTier(200_000, 10, 37.5, 1, 12.5)}
	feed["openai"]["gpt-5.5"]["tiers"] = []map[string]any{fixtureTier(272_000, 10, 50, 1, 0)}
	feed["github-copilot"]["gpt-5-mini"] = nil

	want := []string{
		"anthropic/claude-fable-5-2 missing: feed has no price",
		"anthropic/claude-haiku-5-5 long_prompt: table over 100000 tokens in=0.5 out=2.5 cache_read=0.05 cache_write=0.625, feed none",
		"anthropic/claude-opus-4-8 long_prompt: table none, feed over 200000 tokens in=10 out=37.5 cache_read=1 cache_write=12.5",
		"anthropic/claude-opus-5-5 input: table 4, feed 3.5",
		"anthropic/claude-sonnet-5-5 cache_write: table 2.5, feed 9",
		"github-copilot/gpt-5-mini missing: feed has no price",
		"openai/gpt-5.5 long_prompt: table over 272000 tokens in=10 out=45 cache_read=1 cache_write=0, feed over 272000 tokens in=10 out=50 cache_read=1 cache_write=0",
	}
	if got := pricingDrift(feed.decode(t), nil); !slices.Equal(got, want) {
		t.Errorf("pricingDrift() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPricingDrift_acceptedDifferences(t *testing.T) {
	t.Parallel()

	feed := feedFromTable(t)
	feed["anthropic"]["claude-opus-5-5"]["input"] = 3.5
	feed["anthropic"]["claude-opus-5-5"]["cache_read"] = 0.3
	accepted := map[string]acceptedDrift{
		"anthropic/claude-opus-5-5 input":      {"table 4, feed 3.5", "feed lags the price cut"},
		"anthropic/claude-opus-5-5 output":     {"table 20, feed 18", "feed lags the price cut"},
		"anthropic/claude-opus-5-5 cache_read": {"table 0.2, feed 0.25", "feed lags the price cut"},
	}

	want := []string{
		"anthropic/claude-opus-5-5 cache_read: table 0.2, feed 0.3",
		"anthropic/claude-opus-5-5 output: accepted difference no longer applies (feed lags the price cut)",
	}
	if got := pricingDrift(feed.decode(t), accepted); !slices.Equal(got, want) {
		t.Errorf("pricingDrift() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
