package compact

import (
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
)

// DayPoint is one day of a series.
//
// A window total answers what something cost; it cannot answer whether that is
// rising, whether a change helped, or whether the window has holes in it. Those
// are the questions a series answers, and the day rollups already hold exactly
// this shape — so a trend costs one row per day rather than a scan of turns.
type DayPoint struct {
	Day      string  `json:"day"`
	Turns    int     `json:"turns"`
	Sessions int     `json:"sessions"`
	Errors   int     `json:"errors"`
	CostUSD  float64 `json:"cost_usd"`

	CacheRead int `json:"cache_read"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Write     int `json:"write"`
}

// Empty reports whether the day holds nothing. Days that hold nothing are left
// out of a series rather than sent as zeros: the reader fills its own gaps, and
// a year of mostly-idle days is a much smaller payload for it.
func (p DayPoint) Empty() bool { return p.Turns == 0 }

const dayLayout = "2006-01-02"

// DailyRange returns per-day totals across a window, oldest first.
//
// It prefers the consolidated rollup, then the day's own partition, and only
// counts turns for a day that has neither — which in practice means today.
func (c *Compactor) DailyRange(from, to time.Time) ([]DayPoint, error) {
	idx, err := c.loadIndex()
	if err != nil {
		return nil, err
	}

	hot, err := c.hotDays()
	if err != nil {
		return nil, err
	}

	var out []DayPoint
	err = c.eachDay(from, to, func(day time.Time) error {
		key := day.Format(dayLayout)

		if idx != nil {
			if row, ok := idx.days[key]; ok {
				out = appendPoint(out, fromDayRow(row))
				return nil
			}
		}
		if knownEmpty(idx, hot, day) {
			return nil
		}
		if row, ok, err := c.ReadDay(day); err != nil {
			return err
		} else if ok {
			out = appendPoint(out, fromDayRow(row))
			return nil
		}

		turns, err := c.turnsForDay(day)
		if err != nil {
			return err
		}
		out = appendPoint(out, fromTurns(key, turns, ""))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectDaily returns one project's per-day totals across a window.
//
// The per-project rollup is what makes a project page cheap: "is this
// repository's cache hit rate degrading" is answered from one row per day,
// without reading a single turn.
func (c *Compactor) ProjectDaily(from, to time.Time, projectID string) ([]DayPoint, error) {
	if projectID == "" {
		return nil, nil
	}
	idx, err := c.loadIndex()
	if err != nil {
		return nil, err
	}

	hot, err := c.hotDays()
	if err != nil {
		return nil, err
	}

	var out []DayPoint
	err = c.eachDay(from, to, func(day time.Time) error {
		key := day.Format(dayLayout)

		if idx != nil {
			if rows, ok := idx.projects[key]; ok {
				out = appendPoint(out, fromProjectRows(key, rows, projectID))
				return nil
			}
		}
		if knownEmpty(idx, hot, day) {
			return nil
		}
		rows, err := c.ReadProjects(day)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			out = appendPoint(out, fromProjectRows(key, rows, projectID))
			return nil
		}

		turns, err := c.turnsForDay(day)
		if err != nil {
			return err
		}
		out = appendPoint(out, fromTurns(key, turns, projectID))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hotDays is the set of days the write-ahead window still holds, as YYYY-MM-DD.
//
// It exists to make an empty day free.
//
// A window is walked one day at a time, and a day the rollup does not cover
// used to fall through to a partition read and then a hot-window read. On local
// disk those were two failed file opens -- microseconds, and invisible. Against
// object storage they are network round trips, and a 365-day window over an
// archive holding 15 active days spent 350 of them proving that nothing was
// there. Measured at 1.2 seconds against MinIO on the same machine; against a
// bucket across the internet it would be far worse.
//
// One listing answers all of it. The rollup says which days have partitions and
// this says which days are still in the window; a day in neither is empty, and
// no request has to be made to establish that.
func (c *Compactor) hotDays() (map[string]bool, error) {
	days, err := c.store.Days(storeContext())
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(days))
	for _, d := range days {
		set[d.UTC().Format(dayLayout)] = true
	}
	return set, nil
}

// knownEmpty reports whether a day can be skipped without touching storage.
//
// A day is empty when the rollup does not cover it and the write-ahead window
// does not hold it. Both facts are already in hand -- the rollup is cached and
// the window day set is one query -- so establishing it costs nothing, whereas
// discovering it by reading was costing five object round trips per day.
//
// Conservative when there is no rollup at all: a fresh archive has partitions
// before it has an index, and skipping then would hide real data.
func knownEmpty(idx *index, hot map[string]bool, day time.Time) bool {
	if idx == nil {
		return false
	}
	key := day.UTC().Format(dayLayout)
	if _, covered := idx.days[key]; covered {
		return false
	}
	return !hot[key]
}

// eachDay walks whole UTC days from oldest to newest, inclusive.
func (c *Compactor) eachDay(from, to time.Time, fn func(time.Time) error) error {
	from, to = truncateDay(from), truncateDay(to)
	if to.Before(from) {
		from, to = to, from
	}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if err := fn(day); err != nil {
			return err
		}
	}
	return nil
}

func appendPoint(out []DayPoint, p DayPoint) []DayPoint {
	if p.Empty() {
		return out
	}
	return append(out, p)
}

func fromDayRow(row DayRow) DayPoint {
	return DayPoint{
		Day: row.Day, Turns: int(row.Turns), Sessions: int(row.Sessions),
		Errors: int(row.Errors), CostUSD: row.CostUSD,
		CacheRead: int(row.CacheRead), Input: int(row.InputTokens),
		Output: int(row.OutputTokens),
		Write:  int(row.CacheWrite5m + row.CacheWrite1h),
	}
}

func fromProjectRows(day string, rows []ProjectRow, projectID string) DayPoint {
	point := DayPoint{Day: day}
	for _, r := range rows {
		if r.ProjectID != projectID {
			continue
		}
		point.Turns += int(r.Turns)
		point.Sessions += int(r.Sessions)
		point.Errors += int(r.Errors)
		point.CostUSD += r.CostUSD
		point.CacheRead += int(r.CacheRead)
		point.Input += int(r.InputTokens)
		point.Output += int(r.OutputTokens)
		point.Write += int(r.CacheWrite)
	}
	return point
}

// fromTurns counts a day the slow way, for a day not yet compacted. An empty
// projectID counts every turn.
func fromTurns(day string, turns []query.Turn, projectID string) DayPoint {
	point := DayPoint{Day: day}
	sessions := map[string]bool{}
	for _, t := range turns {
		if projectID != "" && t.ProjectID != projectID {
			continue
		}
		point.Turns++
		sessions[t.SessionID] = true
		if t.Error != "" || t.StatusCode >= 400 {
			point.Errors++
		}
		point.CostUSD += t.CostUSD
		point.CacheRead += t.Usage.CacheReadInputTokens
		point.Input += t.Usage.InputTokens
		point.Output += t.Usage.OutputTokens
		point.Write += t.Usage.CacheCreationInputTokens
	}
	point.Sessions = len(sessions)
	return point
}
