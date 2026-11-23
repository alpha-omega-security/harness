package harness

import "strings"

// modelPrice is the USD list price per million tokens. In is uncached input,
// Out is output, CachedIn is cache reads, and CacheWrite is cache creation.
//
// LongPrompt, when set, replaces the base rates for a request whose prompt has
// more than LongPromptOver tokens.
type modelPrice struct {
	In, Out, CachedIn, CacheWrite float64

	LongPromptOver int
	LongPrompt     *modelPrice
}

// forPrompt returns the rates for one request with the given prompt size.
func (p modelPrice) forPrompt(tokens int) modelPrice {
	if p.LongPrompt != nil && tokens > p.LongPromptOver {
		return *p.LongPrompt
	}
	return p
}

// over returns p with long as its rates for prompts above tokens.
func (p modelPrice) over(tokens int, long modelPrice) modelPrice {
	p.LongPromptOver = tokens
	p.LongPrompt = &long
	return p
}

// openAILongPrompt is the prompt size above which OpenAI charges its
// long-context rates.
const openAILongPrompt = 272_000

// modelPricing lets callers calculate cost when a backend reports tokens but
// no dollar amount. Rows for backends that report cost directly remain here so
// tests can require every default model to have a known price.
//
// Prices are USD rates per million tokens. Copilot-only rows use
// the token prices returned by CLI 1.0.80's models.list, converting one AI
// credit to $0.01. Update this table with each backend's DefaultModels list.
// TestPricingMatchesFeed, built with the pricecheck tag, compares it with
// https://models.dev/api.json.
//
//nolint:mnd // pricing is data, and named constants would obscure the table
var modelPricing = map[string]modelPrice{
	// Anthropic
	"claude-opus-4-6":    {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-4-7":    {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-4-8":    {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	modelClaudeOpus5ID:   {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-5-5":    {In: 4.00, Out: 20.00, CachedIn: 0.20, CacheWrite: 5.00},
	"claude-sonnet-4-6":  {In: 3.00, Out: 15.00, CachedIn: 0.30, CacheWrite: 3.75},
	modelClaudeSonnet5ID: {In: 2.00, Out: 10.00, CachedIn: 0.20, CacheWrite: 2.50},
	"claude-sonnet-5-5":  {In: 2.00, Out: 10.00, CachedIn: 0.10, CacheWrite: 2.50},
	"claude-haiku-4-5":   {In: 1.00, Out: 5.00, CachedIn: 0.10, CacheWrite: 1.25},
	"claude-haiku-5-5":   claudeHaiku55Price,
	"claude-fable-5":     {In: 10.00, Out: 50.00, CachedIn: 1.00, CacheWrite: 12.50},
	"claude-fable-5-1":   {In: 10.00, Out: 50.00, CachedIn: 0.25, CacheWrite: 12.50},
	"claude-fable-5-2":   {In: 8.00, Out: 40.00, CachedIn: 0.20, CacheWrite: 10.00},

	// Copilot uses dotted Anthropic version ids.
	"claude-opus-4.6":   {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-4.7":   {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-4.8":   {In: 5.00, Out: 25.00, CachedIn: 0.50, CacheWrite: 6.25},
	"claude-opus-5.5":   {In: 4.00, Out: 20.00, CachedIn: 0.20, CacheWrite: 5.00},
	"claude-sonnet-4.6": {In: 3.00, Out: 15.00, CachedIn: 0.30, CacheWrite: 3.75},
	"claude-sonnet-5.5": {In: 2.00, Out: 10.00, CachedIn: 0.10, CacheWrite: 2.50},
	"claude-haiku-4.5":  {In: 1.00, Out: 5.00, CachedIn: 0.10, CacheWrite: 1.25},
	"claude-haiku-5.5":  claudeHaiku55Price,
	"claude-fable-5.2":  {In: 8.00, Out: 40.00, CachedIn: 0.20, CacheWrite: 10.00},

	// OpenAI
	// https://developers.openai.com/api/docs/pricing
	"gpt-6.1-sol": modelPrice{In: 2.00, Out: 10.00, CachedIn: 0.10, CacheWrite: 2.50}.
		over(openAILongPrompt, modelPrice{In: 4.00, Out: 15.00, CachedIn: 0.20, CacheWrite: 5.00}),
	"gpt-6.1-terra": modelPrice{In: 1.00, Out: 5.00, CachedIn: 0.05, CacheWrite: 1.25}.
		over(openAILongPrompt, modelPrice{In: 2.00, Out: 7.50, CachedIn: 0.10, CacheWrite: 2.50}),
	"gpt-6-astra": modelPrice{In: 10.00, Out: 50.00, CachedIn: 1.00, CacheWrite: 12.50}.
		over(openAILongPrompt, modelPrice{In: 20.00, Out: 75.00, CachedIn: 2.00, CacheWrite: 25.00}),
	"gpt-6-sol": modelPrice{In: 2.00, Out: 10.00, CachedIn: 0.20, CacheWrite: 2.50}.
		over(openAILongPrompt, modelPrice{In: 4.00, Out: 15.00, CachedIn: 0.40, CacheWrite: 5.00}),
	"gpt-6-luna": modelPrice{In: 0.10, Out: 0.50, CachedIn: 0.01, CacheWrite: 0.125}.
		over(openAILongPrompt, modelPrice{In: 0.20, Out: 0.75, CachedIn: 0.02, CacheWrite: 0.25}),
	// GPT-5.6 Sol's promotional standard rates apply through 2026-11-30.
	modelGPT56SolID: modelPrice{In: 4.00, Out: 20.00, CachedIn: 0.40, CacheWrite: 5.00}.
		over(openAILongPrompt, modelPrice{In: 8.00, Out: 30.00, CachedIn: 0.80, CacheWrite: 10.00}),
	modelGPT56TerraID: modelPrice{In: 2.00, Out: 12.00, CachedIn: 0.20, CacheWrite: 2.50}.
		over(openAILongPrompt, modelPrice{In: 4.00, Out: 18.00, CachedIn: 0.40, CacheWrite: 5.00}),
	modelGPT56LunaID: modelPrice{In: 0.20, Out: 1.20, CachedIn: 0.02, CacheWrite: 0.25}.
		over(openAILongPrompt, modelPrice{In: 0.40, Out: 1.80, CachedIn: 0.04, CacheWrite: 0.50}),
	modelGPT55ID: modelPrice{In: 5.00, Out: 30.00, CachedIn: 0.50}.
		over(openAILongPrompt, modelPrice{In: 10.00, Out: 45.00, CachedIn: 1.00}),
	modelGPT54ID: modelPrice{In: 2.50, Out: 15.00, CachedIn: 0.25}.
		over(openAILongPrompt, modelPrice{In: 5.00, Out: 22.50, CachedIn: 0.50}),
	modelGPT54MiniID:  {In: 0.75, Out: 4.50, CachedIn: 0.075},
	modelGPT53CodexID: {In: 1.75, Out: 14.00, CachedIn: 0.175},
	"gpt-5-mini":      {In: 0.25, Out: 2.00, CachedIn: 0.025},

	// Google
	// Carbon's rates double on 2026-12-01.
	// https://aistudio.google.com/docs/pricing
	"gemini-4-neon":     {In: 3.00, Out: 15.00, CachedIn: 0.15},
	"gemini-4-carbon":   {In: 2.00, Out: 10.00, CachedIn: 0.10},
	"gemini-4-hydrogen": {In: 1.50, Out: 7.50, CachedIn: 0.075},

	// Copilot-hosted Microsoft, Google, and xAI models.
	"mai-code-1-flash-picker": {In: 0.75, Out: 4.50, CachedIn: 0.07},
	"mai-code-1.1-flash":      {In: 0.20, Out: 1.20, CachedIn: 0.02, CacheWrite: 0.25},
	"gemini-3.7-flash":        {In: 0.75, Out: 3.75, CachedIn: 0.07},
	"gemini-3.6-flash":        {In: 0.75, Out: 3.75, CachedIn: 0.07},
	"gemini-3.5-flash":        {In: 1.50, Out: 9.00, CachedIn: 0.15},
	"gemini-3.1-pro-preview":  {In: 2.00, Out: 12.00, CachedIn: 0.20},
	"grok-4.5":                {In: 2.00, Out: 6.00, CachedIn: 0.50},
	"grok-4.6":                {In: 2.00, Out: 6.00, CachedIn: 0.50},
}

// claudeHaiku55Price is five times dearer for prompts over 100,000 tokens.
//
//nolint:mnd // pricing is data
var claudeHaiku55Price = modelPrice{In: 0.10, Out: 0.50, CachedIn: 0.01, CacheWrite: 0.125}.
	over(100_000, modelPrice{In: 0.50, Out: 2.50, CachedIn: 0.05, CacheWrite: 0.625})

const perMillion = 1e6

// CostFromUsage calculates a result event's list-price cost. It returns zero
// for an unknown model rather than presenting an incorrect estimate.
//
// InputTokens includes all prompt tokens. CacheReadTokens is a discounted
// subset. CacheWriteTokens is separate only for models with a dedicated write
// rate; it remains ordinary input when CacheWrite is zero.
//
// A result's usage can sum many requests, so a model priced by prompt length
// is charged at its base rate. Usage events carry a per-request cost that
// applies the long-prompt rate.
func CostFromUsage(model string, usage Usage) float64 {
	price, ok := lookupPrice(model)
	if !ok {
		return 0
	}
	return price.cost(usage)
}

// requestCost is CostFromUsage for a single request whose prompt has
// promptTokens tokens, cached or not.
func requestCost(model string, usage Usage, promptTokens int) float64 {
	price, ok := lookupPrice(model)
	if !ok {
		return 0
	}
	return price.forPrompt(promptTokens).cost(usage)
}

func (price modelPrice) cost(usage Usage) float64 {
	uncached := usage.InputTokens - usage.CacheReadTokens
	if price.CacheWrite > 0 {
		uncached -= usage.CacheWriteTokens
	}
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*price.In +
		float64(usage.CacheReadTokens)*price.CachedIn +
		float64(usage.CacheWriteTokens)*price.CacheWrite +
		float64(usage.OutputTokens)*price.Out) / perMillion
}

// lookupPrice finds a model's list price under its normalized id.
func lookupPrice(model string) (modelPrice, bool) {
	model = normalizeModelID(model)
	// Daybreak Blue currently aliases Sol and shares its pricing.
	// https://developers.openai.com/api/docs/models/gpt-daybreak-blue-latest
	if model == modelDaybreakBlueID {
		model = modelGPT56SolID
	}
	price, ok := modelPricing[model]
	return price, ok
}

// oneHourCacheWriteMultiplier is Anthropic's one-hour cache write rate as a
// multiple of the base input rate. The table's CacheWrite is the five-minute
// rate.
const oneHourCacheWriteMultiplier = 2

// oneHourCacheWriteSurcharge is the cost tokens written to the one-hour cache
// add on top of the five-minute write rate CostFromUsage already charged them.
// It is zero for an unknown model or one without a dedicated write rate.
// promptTokens is the size of the request's whole prompt.
func oneHourCacheWriteSurcharge(model string, tokens, promptTokens int) float64 {
	price, ok := lookupPrice(model)
	if !ok || price.CacheWrite == 0 || tokens <= 0 {
		return 0
	}
	price = price.forPrompt(promptTokens)
	return float64(tokens) * (oneHourCacheWriteMultiplier*price.In - price.CacheWrite) / perMillion
}

// normalizeModelID removes OpenCode's provider prefix, a context-window
// variant suffix and a trailing -YYYYMMDD date so all backends share one
// base-model pricing key.
func normalizeModelID(id string) string {
	if slash := strings.LastIndexByte(id, '/'); slash >= 0 {
		id = id[slash+1:]
	}
	if bracket := strings.IndexByte(id, '['); bracket > 0 {
		id = id[:bracket]
	}
	return trimDateSuffix(id)
}

const dateSuffixDigits = 8

// trimDateSuffix strips a final hyphen followed by exactly eight digits, as in
// claude-haiku-4-5-20251001.
func trimDateSuffix(id string) string {
	cut := len(id) - dateSuffixDigits - 1
	if cut <= 0 || id[cut] != '-' {
		return id
	}
	for _, c := range id[cut+1:] {
		if c < '0' || c > '9' {
			return id
		}
	}
	return id[:cut]
}
