package compact

import (
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// SessionRow is a narrow projection of TurnRow holding only what a session
// summary needs. Parquet is columnar, so declaring fewer fields means fewer
// columns are read off disk — notably skipping the two 64-character blob refs
// and the offered-tool list, which dominate a turn's width but are only needed
// when drilling into a single turn.
//
// Field names and tags must match TurnRow exactly; columns are matched by name.
type SessionRow struct {
	TS        int64  `parquet:"ts,delta"`
	SessionID string `parquet:"session_id,dict"`
	Model     string `parquet:"model,dict"`

	// Identity is read even in the narrow projection: "whose session was that"
	// is a question the list itself has to answer once more than one person
	// shares a tenant, and three dictionary columns cost almost nothing beside
	// the ones already here. MachineID is left out — it matters for
	// deduplication, which happens before a session is ever listed.
	TenantID string `parquet:"tenant_id,dict"`
	UserID   string `parquet:"user_id,dict"`

	Project    string `parquet:"project,dict"`
	ProjectID  string `parquet:"project_id,dict"`
	GitBranch  string `parquet:"git_branch,dict"`
	StatusCode int32  `parquet:"status_code"`
	DurationMS int64  `parquet:"duration_ms,delta"`

	InputTokens  int64 `parquet:"input_tokens,delta"`
	OutputTokens int64 `parquet:"output_tokens,delta"`
	CacheWrite5m int64 `parquet:"cache_write_5m,delta"`
	CacheWrite1h int64 `parquet:"cache_write_1h,delta"`
	CacheRead    int64 `parquet:"cache_read,delta"`

	CostUSD   float64  `parquet:"cost_usd"`
	Priced    bool     `parquet:"priced"`
	ToolCalls []string `parquet:"tool_calls"`
	Error     string   `parquet:"error"`
}

// SessionsRange lists sessions in a window, reading only the columns a session
// summary needs from compacted days and falling back to the raw log otherwise.
func (c *Compactor) SessionsRange(from, to time.Time) ([]query.Session, error) {
	turns, err := c.SessionTurnsRange(from, to)
	if err != nil {
		return nil, err
	}
	return query.SessionsFromTurns(turns), nil
}

// SessionTurnsRange returns a window's turns in the narrow projection, oldest
// first.
//
// Exported because a caller that wants both sessions and something the rollup
// into a session discards — a tool histogram, a model split — would otherwise
// have to walk the same partitions twice. Rolling up is cheap; reading is not.
func (c *Compactor) SessionTurnsRange(from, to time.Time) ([]query.Turn, error) {
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
		dayTurns, err := c.sessionTurnsForDay(day)
		if err != nil {
			return nil, err
		}
		turns = append(turns, dayTurns...)
	}

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

func (c *Compactor) sessionTurnsForDay(day time.Time) ([]query.Turn, error) {
	rows, err := getParquet[SessionRow](storeContext(), c.objects, partitionFile(day, turnsFile))
	if err != nil {
		return nil, err
	}
	if rows != nil {
		turns := make([]query.Turn, 0, len(rows))
		for _, r := range rows {
			turns = append(turns, sessionTurn(r))
		}
		return turns, nil
	}

	events, err := c.store.Events(storeContext(), day)
	if err != nil {
		return nil, err
	}
	return query.BuildTurns(events), nil
}

func sessionTurn(r SessionRow) query.Turn {
	t := query.Turn{
		SessionID:  r.SessionID,
		StartedAt:  time.UnixMilli(r.TS).UTC(),
		Model:      r.Model,
		Identity:   trace.Identity{TenantID: r.TenantID, UserID: r.UserID},
		Project:    r.Project,
		ProjectID:  r.ProjectID,
		GitBranch:  r.GitBranch,
		StatusCode: int(r.StatusCode),
		DurationMS: r.DurationMS,
		Usage: trace.Usage{
			InputTokens:              int(r.InputTokens),
			OutputTokens:             int(r.OutputTokens),
			CacheCreationInputTokens: int(r.CacheWrite5m + r.CacheWrite1h),
			CacheReadInputTokens:     int(r.CacheRead),
			CacheCreation: &trace.CacheCreation{
				Ephemeral5mInputTokens: int(r.CacheWrite5m),
				Ephemeral1hInputTokens: int(r.CacheWrite1h),
			},
		},
		CostUSD: r.CostUSD,
		Priced:  r.Priced,
		Error:   r.Error,
	}
	for _, name := range r.ToolCalls {
		t.ToolCalls = append(t.ToolCalls, trace.ToolCall{Name: name})
	}
	return t
}
