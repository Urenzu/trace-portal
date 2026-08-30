package compact

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Urenzu/trace-portal/internal/pricing"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// File names inside a partition directory.
const (
	turnsFile    = "turns.parquet"
	dayFile      = "day.parquet"
	modelsFile   = "by_model.parquet"
	toolsFile    = "by_tool.parquet"
	projectsFile = "by_project.parquet"
)

// Compactor converts completed days of JSONL into Parquet partitions.
type Compactor struct {
	store *store.Store
	root  string // <data>/compact
}

// New returns a Compactor writing partitions under dataDir/compact.
func New(st *store.Store, dataDir string) (*Compactor, error) {
	root := filepath.Join(dataDir, "compact")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create compact dir: %w", err)
	}
	return &Compactor{store: st, root: root}, nil
}

// Root is the directory holding every partition.
func (c *Compactor) Root() string { return c.root }

// PartitionDir is the directory holding one day of Parquet files.
func (c *Compactor) PartitionDir(day time.Time) string {
	return filepath.Join(c.root, day.UTC().Format("2006-01-02"))
}

// IsCompacted reports whether a day has a complete partition. Partitions are
// published by renaming a fully written temp directory into place, so the
// presence of the directory implies every file in it is complete.
func (c *Compactor) IsCompacted(day time.Time) bool {
	_, err := os.Stat(filepath.Join(c.PartitionDir(day), turnsFile))
	return err == nil
}

// CompactDay rewrites one day of events into a Parquet partition, reporting
// whether it wrote one. It is a no-op for an already-compacted day unless force
// is set.
//
// Today is never compacted: its JSONL file is still being appended to, and a
// partition built from a partial day would be silently wrong.
func (c *Compactor) CompactDay(day time.Time, force bool) (bool, error) {
	day = truncateDay(day)
	if !day.Before(truncateDay(time.Now())) {
		return false, nil // today or later: still being written
	}
	if c.IsCompacted(day) && !force {
		return false, nil
	}

	events, err := c.store.Events(day)
	if err != nil {
		return false, fmt.Errorf("read events for %s: %w", day.Format("2006-01-02"), err)
	}
	if len(events) == 0 {
		return false, nil
	}

	turns := query.BuildTurns(events)
	rows := make([]TurnRow, 0, len(turns))
	for _, t := range turns {
		rows = append(rows, toRow(t))
	}
	dayRow, modelRows, toolRows, projectRows := rollup(day, turns)

	// Write into a temp directory and rename it into place, so a crash during
	// compaction cannot leave a half-written partition that reads as complete.
	tmp, err := os.MkdirTemp(c.root, ".tmp-"+day.Format("2006-01-02")+"-*")
	if err != nil {
		return false, fmt.Errorf("create temp partition: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := writeParquet(filepath.Join(tmp, turnsFile), rows); err != nil {
		return false, err
	}
	if err := writeParquet(filepath.Join(tmp, dayFile), []DayRow{dayRow}); err != nil {
		return false, err
	}
	if err := writeParquet(filepath.Join(tmp, modelsFile), modelRows); err != nil {
		return false, err
	}
	if err := writeParquet(filepath.Join(tmp, toolsFile), toolRows); err != nil {
		return false, err
	}
	if err := writeParquet(filepath.Join(tmp, projectsFile), projectRows); err != nil {
		return false, err
	}

	final := c.PartitionDir(day)
	if force {
		os.RemoveAll(final)
	}
	if err := os.Rename(tmp, final); err != nil {
		// Another compactor may have published this day first, which is
		// harmless: the partition exists either way.
		if c.IsCompacted(day) {
			return false, nil
		}
		return false, fmt.Errorf("publish partition: %w", err)
	}
	return true, nil
}

// CompactAll compacts every completed day that has no partition yet, returning
// how many it wrote.
func (c *Compactor) CompactAll() (int, error) {
	days, err := c.store.Days()
	if err != nil {
		return 0, err
	}
	var written int
	for _, day := range days {
		ok, err := c.CompactDay(day, false)
		if err != nil {
			return written, err
		}
		if ok {
			written++
		}
	}

	// Refresh the consolidated rollup so wide queries stay a fixed three file
	// opens. Also rebuild when nothing was written but no index exists yet, so
	// a directory compacted by an older build picks one up.
	if written > 0 || !c.hasIndex() {
		if err := c.RebuildIndex(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *Compactor) hasIndex() bool {
	_, err := os.Stat(c.indexPath(dayFile))
	return err == nil
}

func writeParquet[T any](path string, rows []T) error {
	// Zstd is a large win on columnar data and costs nothing on the hot path,
	// since compaction runs in the background.
	if err := parquet.WriteFile(path, rows, parquet.Compression(&parquet.Zstd)); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func toRow(t query.Turn) TurnRow {
	write5m, write1h := t.Usage.CacheWrites()
	row := TurnRow{
		TS:           t.StartedAt.UTC().UnixMilli(),
		SessionID:    t.SessionID,
		TurnID:       t.TurnID,
		Model:        t.Model,
		Stream:       t.Stream,
		Project:      t.Project,
		ProjectID:    t.ProjectID,
		GitBranch:    t.GitBranch,
		Source:       t.Source,
		MessageID:    t.MessageID,
		StatusCode:   int32(t.StatusCode),
		StopReason:   t.StopReason,
		DurationMS:   t.DurationMS,
		TTFBMS:       t.TTFBMS,
		MessageCount: int32(t.MessageCount),
		SystemBlocks: int32(t.SystemBlocks),
		InputTokens:  int64(t.Usage.InputTokens),
		OutputTokens: int64(t.Usage.OutputTokens),
		CacheWrite5m: int64(write5m),
		CacheWrite1h: int64(write1h),
		CacheRead:    int64(t.Usage.CacheReadInputTokens),
		CostUSD:      t.CostUSD,
		Priced:       t.Priced,
		ToolsOffered: t.ToolsOffered,
		RequestBlob:  t.RequestBlob,
		ResponseBlob: t.ResponseBlob,
		Error:        t.Error,
		Pending:      t.Pending,
	}
	for _, tc := range t.ToolCalls {
		row.ToolCalls = append(row.ToolCalls, tc.Name)
	}
	return row
}

// FromRow reverses toRow. Tool-call IDs are not carried into Parquet: they are
// only meaningful inside the raw payload, which the blob ref still points at.
func FromRow(r TurnRow) query.Turn {
	t := query.Turn{
		TurnID:       r.TurnID,
		SessionID:    r.SessionID,
		StartedAt:    time.UnixMilli(r.TS).UTC(),
		Model:        r.Model,
		Stream:       r.Stream,
		Project:      r.Project,
		ProjectID:    r.ProjectID,
		GitBranch:    r.GitBranch,
		Source:       r.Source,
		MessageID:    r.MessageID,
		StatusCode:   int(r.StatusCode),
		StopReason:   r.StopReason,
		DurationMS:   r.DurationMS,
		TTFBMS:       r.TTFBMS,
		MessageCount: int(r.MessageCount),
		SystemBlocks: int(r.SystemBlocks),
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
		CostUSD:      r.CostUSD,
		Priced:       r.Priced,
		ToolsOffered: r.ToolsOffered,
		RequestBlob:  r.RequestBlob,
		ResponseBlob: r.ResponseBlob,
		Error:        r.Error,
		Pending:      r.Pending,
	}
	for _, name := range r.ToolCalls {
		t.ToolCalls = append(t.ToolCalls, trace.ToolCall{Name: name})
	}
	return t
}

func rollup(day time.Time, turns []query.Turn) (DayRow, []ModelRow, []ToolRow, []ProjectRow) {
	key := day.Format("2006-01-02")
	d := DayRow{Day: key}

	sessions := map[string]bool{}
	byModel := map[string]*ModelRow{}
	byTool := map[string]int64{}
	byProject := map[string]*ProjectRow{}
	projectSessions := map[string]map[string]bool{}

	for _, t := range turns {
		d.Turns++
		sessions[t.SessionID] = true

		write5m, write1h := t.Usage.CacheWrites()
		d.InputTokens += int64(t.Usage.InputTokens)
		d.OutputTokens += int64(t.Usage.OutputTokens)
		d.CacheWrite5m += int64(write5m)
		d.CacheWrite1h += int64(write1h)
		d.CacheRead += int64(t.Usage.CacheReadInputTokens)
		d.CostUSD += t.CostUSD
		d.SavingsUSD += pricing.CacheSavings(t.Model, t.Usage)
		if !t.Priced && t.Model != "" {
			d.UnpricedTurns++
		}
		if t.Error != "" || t.StatusCode >= 400 {
			d.Errors++
		}

		if t.Model != "" {
			m := byModel[t.Model]
			if m == nil {
				m = &ModelRow{Day: key, Model: t.Model}
				byModel[t.Model] = m
			}
			m.Turns++
			m.InputTokens += int64(t.Usage.InputTokens)
			m.OutputTokens += int64(t.Usage.OutputTokens)
			m.CacheRead += int64(t.Usage.CacheReadInputTokens)
			m.CostUSD += t.CostUSD
		}
		for _, tc := range t.ToolCalls {
			byTool[tc.Name]++
		}

		if t.Project != "" {
			pr := byProject[t.ProjectID]
			if pr == nil {
				pr = &ProjectRow{Day: key, Project: t.Project, ProjectID: t.ProjectID, InRepo: t.InRepo}
				byProject[t.ProjectID] = pr
				projectSessions[t.ProjectID] = map[string]bool{}
			}
			pr.Turns++
			pr.InputTokens += int64(t.Usage.InputTokens)
			pr.OutputTokens += int64(t.Usage.OutputTokens)
			w5p, w1hp := t.Usage.CacheWrites()
			pr.CacheWrite += int64(w5p + w1hp)
			pr.CacheRead += int64(t.Usage.CacheReadInputTokens)
			pr.CostUSD += t.CostUSD
			if t.Error != "" || t.StatusCode >= 400 {
				pr.Errors++
			}
			projectSessions[t.ProjectID][t.SessionID] = true
		}
	}
	d.Sessions = int64(len(sessions))

	models := make([]ModelRow, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })

	tools := make([]ToolRow, 0, len(byTool))
	for name, calls := range byTool {
		tools = append(tools, ToolRow{Day: key, Tool: name, Calls: calls})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Tool < tools[j].Tool })

	projects := make([]ProjectRow, 0, len(byProject))
	for id, pr := range byProject {
		pr.Sessions = int64(len(projectSessions[id]))
		projects = append(projects, *pr)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })

	return d, models, tools, projects
}
