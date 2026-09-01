package compact

import (
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/pricing"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// TurnsRange returns every turn between from and to, reading each day from its
// Parquet partition when one exists and falling back to the raw JSONL log
// otherwise. Today is always read from JSONL, since it is still being written.
func (c *Compactor) TurnsRange(from, to time.Time) ([]query.Turn, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}

	idx, err := c.loadIndex()
	if err != nil {
		return nil, err
	}
	hot, err := c.hotDays()
	if err != nil {
		return nil, err
	}

	var turns []query.Turn
	for day := truncateDay(from); !day.After(truncateDay(to)); day = day.AddDate(0, 0, 1) {
		if knownEmpty(idx, hot, day) {
			continue
		}
		dayTurns, err := c.turnsForDay(day)
		if err != nil {
			return nil, err
		}
		turns = append(turns, dayTurns...)
	}

	// Daily partitions cover whole days, so trim to the requested instants.
	filtered := turns[:0]
	for _, t := range turns {
		if t.StartedAt.Before(from) || t.StartedAt.After(to) {
			continue
		}
		filtered = append(filtered, t)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].StartedAt.Before(filtered[j].StartedAt) })
	return filtered, nil
}

func (c *Compactor) turnsForDay(day time.Time) ([]query.Turn, error) {
	if turns, ok, err := c.ReadTurns(day); err != nil {
		return nil, err
	} else if ok {
		return turns, nil
	}

	events, err := c.store.Events(storeContext(), day)
	if err != nil {
		return nil, err
	}
	return query.BuildTurns(events), nil
}

// Aggregate is the rolled-up view of a time window.
type Aggregate struct {
	Sessions      int
	Turns         int
	Errors        int
	Usage         trace.Usage
	CostUSD       float64
	SavingsUSD    float64
	UnpricedTurns int
	ByModel       map[string]int
	ToolCalls     map[string]int
	ByProject     map[string]*ProjectTotals

	// SessionsExact is false when any day in the window was answered from a
	// rollup. Rollups store a per-day distinct session count, so a session
	// spanning midnight is counted once per day it touched; the totals stay
	// exact but the session count becomes an upper bound.
	SessionsExact bool
}

// ProjectTotals is one project's share of a window.
type ProjectTotals struct {
	Project   string
	ProjectID string
	// InRepo separates work done inside a repository from work done in an
	// ordinary directory. Both cost money; only one is a project.
	InRepo    bool
	Turns     int
	Sessions  int
	CacheRead int
	Input     int
	Output    int
	Write     int
	CostUSD   float64
	Errors    int
}

// CacheHitRate is the share of this project's input tokens served from cache.
func (p ProjectTotals) CacheHitRate() float64 {
	total := p.Input + p.Write + p.CacheRead
	if total == 0 {
		return 0
	}
	return float64(p.CacheRead) / float64(total)
}

// addProject folds one project's numbers into the aggregate.
func (a *Aggregate) addProject(id, name string, inRepo bool, turns, sessions, cacheRead, input, output, write int, cost float64, errs int) {
	if id == "" {
		return
	}
	p := a.ByProject[id]
	if p == nil {
		p = &ProjectTotals{Project: name, ProjectID: id, InRepo: inRepo}
		a.ByProject[id] = p
	}
	p.Turns += turns
	p.Sessions += sessions
	p.CacheRead += cacheRead
	p.Input += input
	p.Output += output
	p.Write += write
	p.CostUSD += cost
	p.Errors += errs
}

// AggregateRange summarizes a window, preferring pre-aggregated day rollups.
// A rollup is a single row per day, so a full-history dashboard query costs
// O(days) rather than O(turns).
func (c *Compactor) AggregateRange(from, to time.Time) (Aggregate, error) {
	agg := Aggregate{
		ByModel:       map[string]int{},
		ToolCalls:     map[string]int{},
		ByProject:     map[string]*ProjectTotals{},
		SessionsExact: true,
	}
	from, to = from.UTC(), to.UTC()

	// A rollup covers a whole UTC day, so it can only answer for days that lie
	// entirely inside the window. Partial days at either edge fall through to
	// the turn-level path.
	firstWhole := truncateDay(from)
	if firstWhole.Before(from) {
		firstWhole = firstWhole.AddDate(0, 0, 1)
	}
	lastWhole := truncateDay(to).AddDate(0, 0, -1)

	// Load the consolidated rollup once. It covers every compacted day in three
	// file opens, so a wide window does not pay a cold file open per day.
	idx, err := c.loadIndex()
	if err != nil {
		return agg, err
	}

	hot, err := c.hotDays()
	if err != nil {
		return agg, err
	}

	sessions := map[string]bool{}
	for day := truncateDay(from); !day.After(truncateDay(to)); day = day.AddDate(0, 0, 1) {
		wholeDay := !day.Before(firstWhole) && !day.After(lastWhole)

		// Nothing here, and nothing had to be read to find that out. This is
		// the line that makes a year-wide window cost the same as a week-wide
		// one over an archive that is mostly idle days -- which every archive
		// is, since nobody runs an agent every day.
		if knownEmpty(idx, hot, day) {
			continue
		}

		if wholeDay {
			if idx != nil && idx.add(day, &agg) {
				agg.SessionsExact = false
				continue
			}
			if used, err := c.aggregateFromRollup(day, &agg); err != nil {
				return agg, err
			} else if used {
				agg.SessionsExact = false
				continue
			}
		}

		turns, err := c.turnsForDay(day)
		if err != nil {
			return agg, err
		}
		for _, t := range turns {
			if t.StartedAt.Before(from) || t.StartedAt.After(to) {
				continue
			}
			addTurn(&agg, t, sessions)
		}
	}

	agg.Sessions += len(sessions)
	return agg, nil
}

// aggregateFromRollup folds one day's rollup into agg, reporting whether a
// rollup existed.
func (c *Compactor) aggregateFromRollup(day time.Time, agg *Aggregate) (bool, error) {
	row, ok, err := c.ReadDay(day)
	if err != nil || !ok {
		return false, err
	}

	addDayRow(row, agg)

	models, err := c.ReadModels(day)
	if err != nil {
		return false, err
	}
	for _, m := range models {
		agg.ByModel[m.Model] += int(m.Turns)
	}

	tools, err := c.ReadTools(day)
	if err != nil {
		return false, err
	}
	for _, t := range tools {
		agg.ToolCalls[t.Tool] += int(t.Calls)
	}

	projects, err := c.ReadProjects(day)
	if err != nil {
		return false, err
	}
	for _, p := range projects {
		agg.addProject(p.ProjectID, p.Project, p.InRepo, int(p.Turns), int(p.Sessions),
			int(p.CacheRead), int(p.InputTokens), int(p.OutputTokens), int(p.CacheWrite),
			p.CostUSD, int(p.Errors))
	}
	return true, nil
}

// addDayRow folds one pre-aggregated day into agg.
func addDayRow(row DayRow, agg *Aggregate) {
	agg.Sessions += int(row.Sessions)
	agg.Turns += int(row.Turns)
	agg.Errors += int(row.Errors)
	agg.CostUSD += row.CostUSD
	agg.SavingsUSD += row.SavingsUSD
	agg.UnpricedTurns += int(row.UnpricedTurns)
	agg.Usage.Add(trace.Usage{
		InputTokens:              int(row.InputTokens),
		OutputTokens:             int(row.OutputTokens),
		CacheCreationInputTokens: int(row.CacheWrite5m + row.CacheWrite1h),
		CacheReadInputTokens:     int(row.CacheRead),
		CacheCreation: &trace.CacheCreation{
			Ephemeral5mInputTokens: int(row.CacheWrite5m),
			Ephemeral1hInputTokens: int(row.CacheWrite1h),
		},
	})
}

func addTurn(agg *Aggregate, t query.Turn, sessions map[string]bool) {
	agg.Turns++
	sessions[t.SessionID] = true
	agg.Usage.Add(t.Usage)
	agg.CostUSD += t.CostUSD
	agg.SavingsUSD += pricing.CacheSavings(t.Model, t.Usage)
	if !t.Priced && t.Model != "" {
		agg.UnpricedTurns++
	}
	if t.Error != "" || t.StatusCode >= 400 {
		agg.Errors++
	}
	if t.Model != "" {
		agg.ByModel[t.Model]++
	}
	for _, tc := range t.ToolCalls {
		agg.ToolCalls[tc.Name]++
	}
	if t.ProjectID != "" {
		w5, w1h := t.Usage.CacheWrites()
		errs := 0
		if t.Error != "" || t.StatusCode >= 400 {
			errs = 1
		}
		agg.addProject(t.ProjectID, t.Project, t.InRepo, 1, 0,
			t.Usage.CacheReadInputTokens, t.Usage.InputTokens, t.Usage.OutputTokens,
			w5+w1h, t.CostUSD, errs)
	}
}
