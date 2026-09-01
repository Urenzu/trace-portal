package compact

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/Urenzu/trace-portal/internal/eventstore"
	"github.com/Urenzu/trace-portal/internal/objectstore"
	"github.com/Urenzu/trace-portal/internal/pricing"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// File names inside a partition directory.
const (
	turnsFile    = "turns.parquet"
	dayFile      = "day.parquet"
	modelsFile   = "by_model.parquet"
	toolsFile    = "by_tool.parquet"
	projectsFile = "by_project.parquet"
	sessionsFile = "by_session.parquet"
)

// Compactor converts completed days of JSONL into Parquet partitions.
type Compactor struct {
	store   eventstore.Store
	objects objectstore.Store

	// root is the local directory when there is one, for diagnostics and for
	// telling a person where their archive is. Empty when partitions live in a
	// bucket.
	root string

	// prune releases a day from the hot window once its partition is verified.
	// See PruneCompacted.
	prune bool

	// index caches the consolidated rollup. See loadIndex for why holding it is
	// safe and why invalidating it is trivial.
	indexMu sync.RWMutex
	index   *index
}

// New returns a Compactor writing partitions under dataDir/compact.
//
// The hot window is an interface because it varies by deployment — files
// locally, Postgres on a server — while everything this package writes does
// not. Partitions and rollups are the same Parquet either way, which is what
// keeps a local archive and a served one readable by the same code, and by
// DuckDB.
func New(st eventstore.Store, dataDir string) (*Compactor, error) {
	root := filepath.Join(dataDir, "compact")
	objects, err := objectstore.NewLocal(root)
	if err != nil {
		return nil, err
	}
	return &Compactor{store: st, objects: objects, root: root}, nil
}

// NewWithObjects returns a Compactor writing partitions into an object store.
//
// The keys are identical to the paths the local layout uses -- "2026-08-28/
// turns.parquet", "rollup/day.parquet" -- so an archive can be copied between a
// directory and a bucket and stay readable by both. That was not free: it is
// why keys are assembled with objectstore.Key rather than filepath.Join, which
// on Windows would produce backslashes and quietly fork the layout by platform.
func NewWithObjects(st eventstore.Store, objects objectstore.Store) *Compactor {
	return &Compactor{store: st, objects: objects}
}

// partitionKey is the key prefix holding one day of Parquet files.
func partitionKey(day time.Time) string { return day.UTC().Format("2006-01-02") }

// partitionFile names one file inside a day partition.
func partitionFile(day time.Time, name string) string {
	return objectstore.Key(partitionKey(day), name)
}

// Root is the directory holding every partition.
func (c *Compactor) Root() string { return c.root }

// PartitionDir is the local directory holding one day of Parquet files. It is
// empty when partitions live in a bucket, and exists for diagnostics and for
// the DuckDB invitation in the README rather than as a read path.
func (c *Compactor) PartitionDir(day time.Time) string {
	if c.root == "" {
		return ""
	}
	return filepath.Join(c.root, partitionKey(day))
}

// IsCompacted reports whether a day has a complete partition. Partitions are
// published by renaming a fully written temp directory into place, so the
// presence of the directory implies every file in it is complete.
func (c *Compactor) IsCompacted(day time.Time) bool {
	ok, err := c.objects.Exists(storeContext(), partitionFile(day, turnsFile))
	return err == nil && ok
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

	events, err := c.store.Events(storeContext(), day)
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
	dayRow, modelRows, toolRows, projectRows, sessionRows := rollup(day, turns)

	// The rollups are written first and turns.parquet last, because an object
	// store has no rename.
	//
	// The local layout used to publish a partition by renaming a fully written
	// temporary directory into place, which made the directory existing proof
	// that everything inside it was complete. That trick does not exist in a
	// bucket, so completeness is published by ordering instead: IsCompacted
	// tests for turns.parquet, and turns.parquet is the last thing written, so
	// its presence still means the whole partition landed. A crash halfway
	// leaves rollups with no turns file, which reads as "not compacted" and is
	// simply redone.
	ctx := storeContext()
	writes := []struct {
		name string
		put  func() error
	}{
		{dayFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, dayFile), []DayRow{dayRow}) }},
		{modelsFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, modelsFile), modelRows) }},
		{toolsFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, toolsFile), toolRows) }},
		{projectsFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, projectsFile), projectRows) }},
		{sessionsFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, sessionsFile), sessionRows) }},
		{turnsFile, func() error { return putParquet(ctx, c.objects, partitionFile(day, turnsFile), rows) }},
	}
	for _, w := range writes {
		if err := w.put(); err != nil {
			return false, fmt.Errorf("write %s for %s: %w", w.name, partitionKey(day), err)
		}
	}

	return true, nil
}

// CompactAll compacts every completed day that has no partition yet, returning
// how many it wrote.
func (c *Compactor) CompactAll() (int, error) {
	days, err := c.store.Days(storeContext())
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

	// Refresh the consolidated rollup so wide queries stay a fixed number of
	// file opens. Also rebuild when nothing was written but the index is absent
	// or predates the session-day table, so an archive compacted by an older
	// build picks one up without needing to be recompacted.
	if written > 0 || !c.hasIndex() {
		if err := c.RebuildIndex(); err != nil {
			return written, err
		}
	}

	// Releasing compacted days is a separate sweep rather than a step inside
	// CompactDay, and that is what makes it self-healing. It looks at what the
	// hot window still holds rather than at what this run happened to write, so
	// a day whose prune failed last time is retried, and a deployment that has
	// been compacting for months without pruning drains on its first run of a
	// build that does.
	if c.prune {
		if err := c.pruneHotWindow(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *Compactor) hasIndex() bool {
	for _, name := range []string{dayFile, sessionsFile} {
		ok, err := c.objects.Exists(storeContext(), c.indexKey(name))
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// putParquet encodes rows and stores them under key.
//
// Encoded into memory rather than streamed. A day of turns is tens of kilobytes
// and the rollups are smaller still, so the buffer is irrelevant -- and writing
// the whole object in one call is what makes the PUT atomic, which is what
// replaced the rename.
func putParquet[T any](ctx context.Context, objects objectstore.Store, key string, rows []T) error {
	var buf bytes.Buffer
	// Zstd is a large win on columnar data and costs nothing on the hot path,
	// since compaction runs in the background.
	if err := parquet.Write(&buf, rows, parquet.Compression(&parquet.Zstd)); err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	return objects.Put(ctx, key, buf.Bytes())
}

// storeContext is the context compaction reads the hot window with.
//
// It is deliberately not a request context. Compaction is a background job that
// nobody is waiting on, and the read paths that *are* request-driven still call
// through here — threading the caller's context all the way down is a separate
// change, listed in todo.md, and doing it halfway would cancel a background
// compaction because a browser tab closed.
func storeContext() context.Context { return context.Background() }

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
		TenantID:     t.TenantID,
		UserID:       t.UserID,
		MachineID:    t.MachineID,
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
		Identity:     trace.Identity{TenantID: r.TenantID, UserID: r.UserID, MachineID: r.MachineID},
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

func rollup(day time.Time, turns []query.Turn) (DayRow, []ModelRow, []ToolRow, []ProjectRow, []SessionDayRow) {
	key := day.Format("2006-01-02")
	d := DayRow{Day: key}

	sessions := map[string]*SessionDayRow{}
	byModel := map[string]*ModelRow{}
	byTool := map[string]int64{}
	byProject := map[string]*ProjectRow{}
	projectSessions := map[string]map[string]bool{}

	for _, t := range turns {
		d.Turns++
		if sr := sessions[t.SessionID]; sr == nil {
			ms := t.StartedAt.UTC().UnixMilli()
			sessions[t.SessionID] = &SessionDayRow{
				Day: key, SessionID: t.SessionID, Turns: 1, FirstTS: ms, LastTS: ms,
			}
		} else {
			ms := t.StartedAt.UTC().UnixMilli()
			sr.Turns++
			if ms < sr.FirstTS {
				sr.FirstTS = ms
			}
			if ms > sr.LastTS {
				sr.LastTS = ms
			}
		}

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

	sessionDays := make([]SessionDayRow, 0, len(sessions))
	for _, sr := range sessions {
		sessionDays = append(sessionDays, *sr)
	}
	sort.Slice(sessionDays, func(i, j int) bool { return sessionDays[i].SessionID < sessionDays[j].SessionID })

	return d, models, tools, projects, sessionDays
}
