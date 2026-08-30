// Package pricing turns token counts into dollars.
//
// Rates are first-party Anthropic API list prices in USD per million tokens.
// Bedrock and Vertex are partner-operated with separate pricing and are not
// modelled here; neither is fast mode, which bills Opus 5 at a premium.
package pricing

import (
	"sort"
	"strings"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// Cache multipliers applied to a model's base input rate.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
	cacheReadMultiplier    = 0.1
)

// Rate is a model's list price in USD per million tokens.
type Rate struct {
	Input  float64
	Output float64
}

// rates is keyed by model ID. Traced model strings may carry a date suffix
// (claude-haiku-4-5-20251001), so lookup falls back to longest-prefix match.
var rates = map[string]Rate{
	"claude-fable-5":    {Input: 10, Output: 50},
	"claude-mythos-5":   {Input: 10, Output: 50},
	"claude-opus-5":     {Input: 5, Output: 25},
	"claude-opus-4-8":   {Input: 5, Output: 25},
	"claude-opus-4-7":   {Input: 5, Output: 25},
	"claude-opus-4-6":   {Input: 5, Output: 25},
	"claude-sonnet-5":   {Input: 2, Output: 10},
	"claude-sonnet-4-6": {Input: 3, Output: 15},
	"claude-haiku-4-5":  {Input: 1, Output: 5},
}

// prefixes holds the rate keys longest-first so that lookup prefers the most
// specific match (claude-opus-4-8 before any shorter key that also matches).
var prefixes = func() []string {
	keys := make([]string, 0, len(rates))
	for k := range rates {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return keys
}()

// Lookup returns the rate for a model ID. The second result is false for models
// we have no price for, so callers can report unpriced traffic rather than
// silently costing it at zero.
func Lookup(model string) (Rate, bool) {
	if r, ok := rates[model]; ok {
		return r, true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(model, p) {
			return rates[p], true
		}
	}
	return Rate{}, false
}

// Cost returns the USD cost of one usage block for the given model. Unknown
// models cost zero and report false.
func Cost(model string, u trace.Usage) (float64, bool) {
	rate, ok := Lookup(model)
	if !ok {
		return 0, false
	}

	write5m, write1h := u.CacheWrites()
	perToken := func(tokens int, ratePerMillion float64) float64 {
		return float64(tokens) * ratePerMillion / 1e6
	}

	total := perToken(u.InputTokens, rate.Input) +
		perToken(u.OutputTokens, rate.Output) +
		perToken(write5m, rate.Input*cacheWrite5mMultiplier) +
		perToken(write1h, rate.Input*cacheWrite1hMultiplier) +
		perToken(u.CacheReadInputTokens, rate.Input*cacheReadMultiplier)
	return total, true
}

// Models returns every priced model ID, sorted, for diagnostics.
func Models() []string {
	out := make([]string, 0, len(rates))
	for k := range rates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CacheSavings is the difference between what cache-read tokens would have cost
// at full input price and the 0.1x they actually cost — the money caching saved.
//
// Cache writes are deliberately excluded: their 1.25x/2x premium is the cost of
// buying that saving, and netting it here would conflate what caching saved
// with what it cost.
func CacheSavings(model string, u trace.Usage) float64 {
	rate, ok := Lookup(model)
	if !ok || u.CacheReadInputTokens == 0 {
		return 0
	}
	full := float64(u.CacheReadInputTokens) * rate.Input / 1e6
	return full - full*cacheReadMultiplier
}
