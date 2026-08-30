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
