package api

import (
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// statsFromAggregate shapes a window aggregate into the dashboard payload.
// The aggregate itself comes from pre-computed day rollups wherever the window
// covers a whole compacted day, so this is O(days) rather than O(turns).
func statsFromAggregate(agg compact.Aggregate, from, to time.Time) Stats {
	stats := Stats{
		From:          from,
		To:            to,
		Sessions:      agg.Sessions,
		Turns:         agg.Turns,
		Errors:        agg.Errors,
		Usage:         agg.Usage,
		CostUSD:       agg.CostUSD,
		SavingsUSD:    agg.SavingsUSD,
		UnpricedRun:   agg.UnpricedTurns,
		SessionsExact: agg.SessionsExact,
		CacheHitRate:  inputCacheHitRate(agg.Usage),
		ByModel:       agg.ByModel,
		ToolCalls:     agg.ToolCalls,
	}
	for _, p := range agg.ByProject {
		stats.ByProject = append(stats.ByProject, ProjectStat{
			Project: p.Project, ProjectID: p.ProjectID, InRepo: p.InRepo,
			Turns: p.Turns, Sessions: p.Sessions, CostUSD: p.CostUSD,
			CacheHitRate: p.CacheHitRate(), Errors: p.Errors,
			InputTokens: p.Input + p.Write + p.CacheRead, OutputTokens: p.Output,
		})
	}
	// Most expensive first: the question is which project costs most.
	sort.Slice(stats.ByProject, func(i, j int) bool {
		return stats.ByProject[i].CostUSD > stats.ByProject[j].CostUSD
	})

	if len(stats.ByModel) == 0 {
		stats.ByModel = nil
	}
	if len(stats.ToolCalls) == 0 {
		stats.ToolCalls = nil
	}
	return stats
}

// inputCacheHitRate is cached input tokens over all input tokens. Output tokens
// are excluded: they are never served from cache, and counting them would make
// a turn look worse purely for generating a long answer.
func inputCacheHitRate(u trace.Usage) float64 {
	total := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if total == 0 {
		return 0
	}
	return float64(u.CacheReadInputTokens) / float64(total)
}
