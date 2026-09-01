package compact

import (
	"os"
	"path/filepath"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Urenzu/trace-portal/internal/query"
)

// ReadTurns returns a compacted day's turns, or false if the day has no
// partition. Reading Parquet decodes only the columns the struct declares, and
// skips row groups outside the file entirely.
func (c *Compactor) ReadTurns(day time.Time) ([]query.Turn, bool, error) {
	path := filepath.Join(c.PartitionDir(day), turnsFile)
	rows, err := parquet.ReadFile[TurnRow](path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	turns := make([]query.Turn, 0, len(rows))
	for _, r := range rows {
		turns = append(turns, FromRow(r))
	}
	return turns, true, nil
}

// ReadDay returns a day's pre-aggregated rollup. This is what the stats
// dashboard reads: one row per day, so its cost scales with the number of days
// in the window rather than the number of turns.
func (c *Compactor) ReadDay(day time.Time) (DayRow, bool, error) {
	rows, err := readOrMissing[DayRow](filepath.Join(c.PartitionDir(day), dayFile))
	if err != nil || len(rows) == 0 {
		return DayRow{}, false, err
	}
	return rows[0], true, nil
}

// ReadModels returns a day's per-model breakdown.
func (c *Compactor) ReadModels(day time.Time) ([]ModelRow, error) {
	return readOrMissing[ModelRow](filepath.Join(c.PartitionDir(day), modelsFile))
}

// ReadTools returns a day's tool-call histogram.
func (c *Compactor) ReadTools(day time.Time) ([]ToolRow, error) {
	return readOrMissing[ToolRow](filepath.Join(c.PartitionDir(day), toolsFile))
}

// ReadProjects returns a day's per-project breakdown.
func (c *Compactor) ReadProjects(day time.Time) ([]ProjectRow, error) {
	return readOrMissing[ProjectRow](filepath.Join(c.PartitionDir(day), projectsFile))
}

// readOrMissing treats an absent partition as an empty result rather than an
// error, so callers can fall back to the JSONL path.
func readOrMissing[T any](path string) ([]T, error) {
	rows, err := parquet.ReadFile[T](path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

// sessionProbe is the narrowest useful projection of TurnRow: which session a
// turn belongs to and when it happened. Both columns are dictionary- or
// delta-encoded, so probing a day for a session costs a fraction of reading it.
//
// Field names and tags must match TurnRow exactly; columns are matched by name.
type sessionProbe struct {
	TS        int64  `parquet:"ts,delta"`
	SessionID string `parquet:"session_id,dict"`
}

// ReadSessionDays returns which sessions touched a compacted day.
//
// Partitions written before this index existed carry no by_session.parquet, so
// the rows are derived from the day's turns instead. That keeps an archive
// built by an older build correct without forcing a recompaction, at the cost
// of reading two columns rather than a prepared rollup.
func (c *Compactor) ReadSessionDays(day time.Time) ([]SessionDayRow, error) {
	rows, err := readOrMissing[SessionDayRow](filepath.Join(c.PartitionDir(day), sessionsFile))
	if err != nil {
		return nil, err
	}
	if rows != nil {
		return rows, nil
	}

	probes, err := readOrMissing[sessionProbe](filepath.Join(c.PartitionDir(day), turnsFile))
	if err != nil || probes == nil {
		return nil, err
	}
	return sessionDaysFromProbes(day, probes), nil
}

func sessionDaysFromProbes(day time.Time, probes []sessionProbe) []SessionDayRow {
	key := day.UTC().Format("2006-01-02")
	byID := map[string]*SessionDayRow{}
	order := make([]string, 0, 8)
	for _, p := range probes {
		row := byID[p.SessionID]
		if row == nil {
			byID[p.SessionID] = &SessionDayRow{
				Day: key, SessionID: p.SessionID, Turns: 1, FirstTS: p.TS, LastTS: p.TS,
			}
			order = append(order, p.SessionID)
			continue
		}
		row.Turns++
		if p.TS < row.FirstTS {
			row.FirstTS = p.TS
		}
		if p.TS > row.LastTS {
			row.LastTS = p.TS
		}
	}

	out := make([]SessionDayRow, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}
