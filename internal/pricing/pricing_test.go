package pricing

import (
	"math"
	"testing"

	"github.com/Urenzu/trace-portal/internal/trace"
)

func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %.10f, want %.10f", got, want)
	}
}

func TestCostBaseTokens(t *testing.T) {
	// Opus 5 is $5/MTok in, $25/MTok out.
	cost, ok := Cost("claude-opus-5", trace.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok {
		t.Fatal("opus 5 should be priced")
	}
	closeTo(t, cost, 30.0)
}

func TestCostCacheMultipliers(t *testing.T) {
	// Cache reads bill at 0.1x input, 5-minute writes at 1.25x, 1-hour at 2x.
	read, _ := Cost("claude-opus-5", trace.Usage{CacheReadInputTokens: 1_000_000})
	closeTo(t, read, 0.5)

	write5m, _ := Cost("claude-opus-5", trace.Usage{
		CacheCreationInputTokens: 1_000_000,
		CacheCreation:            &trace.CacheCreation{Ephemeral5mInputTokens: 1_000_000},
	})
	closeTo(t, write5m, 6.25)

	write1h, _ := Cost("claude-opus-5", trace.Usage{
		CacheCreationInputTokens: 1_000_000,
		CacheCreation:            &trace.CacheCreation{Ephemeral1hInputTokens: 1_000_000},
	})
	closeTo(t, write1h, 10.0)
}

// Older API responses report no TTL breakdown; those writes bill at the
// 5-minute rate rather than silently costing nothing.
func TestCostCacheWriteWithoutBreakdown(t *testing.T) {
	cost, _ := Cost("claude-opus-5", trace.Usage{CacheCreationInputTokens: 1_000_000})
	closeTo(t, cost, 6.25)
}

func TestCostPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  float64 // 1M input + 1M output
	}{
		{"claude-fable-5", 60},
		{"claude-opus-5", 30},
		{"claude-sonnet-5", 12},
		{"claude-sonnet-4-6", 18},
		{"claude-haiku-4-5", 6},
	}
	for _, tc := range tests {
		got, ok := Cost(tc.model, trace.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
		if !ok {
			t.Errorf("%s: not priced", tc.model)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: got %.4f, want %.4f", tc.model, got, tc.want)
		}
	}
}

// Traced model strings can carry a date suffix; they must still price.
func TestLookupDateSuffixedModel(t *testing.T) {
	rate, ok := Lookup("claude-haiku-4-5-20251001")
	if !ok {
		t.Fatal("date-suffixed model should resolve")
	}
	if rate.Input != 1 || rate.Output != 5 {
		t.Errorf("rate = %+v", rate)
	}
}

// An unknown model must report false rather than costing zero silently, so the
// UI can surface unpriced traffic instead of understating the bill.
func TestCostUnknownModel(t *testing.T) {
	cost, ok := Cost("some-other-llm", trace.Usage{InputTokens: 1_000_000})
	if ok {
		t.Error("unknown model reported as priced")
	}
	if cost != 0 {
		t.Errorf("cost = %f, want 0", cost)
	}
}
