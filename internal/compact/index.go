package compact

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The consolidated index holds every day's rollup in three files rather than
// three files per day. Per-day partitions stay the source of truth — a
// partition is self-contained and can be rebuilt or copied on its own — but
// answering a wide window from them means one cold file open per day, and cold
// opens cost milliseconds each. A year of history is 365 rollup rows; keeping
// them together turns a full-history dashboard query into three file opens
// total, independent of how much history exists.
const indexDir = "rollup"

func (c *Compactor) indexPath(name string) string {
	return filepath.Join(c.root, indexDir, name)
}

// RebuildIndex regenerates the consolidated rollup from every partition. It is
// called after compaction, and is cheap: the inputs are one row per day.
func (c *Compactor) RebuildIndex() error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}

	var (
		days     []DayRow
		models   []ModelRow
		tools    []ToolRow
		projects []ProjectRow
	)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == indexDir {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", e.Name(), time.UTC)
		if err != nil {
			continue // not a partition directory
		}

		if row, ok, err := c.ReadDay(day); err != nil {
			return err
		} else if ok {
			days = append(days, row)
		}
		m, err := c.ReadModels(day)
		if err != nil {
			return err
		}
		models = append(models, m...)

		tl, err := c.ReadTools(day)
		if err != nil {
			return err
		}
		tools = append(tools, tl...)

		pj, err := c.ReadProjects(day)
		if err != nil {
			return err
		}
		projects = append(projects, pj...)
	}

	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })
	sort.Slice(models, func(i, j int) bool {
		if models[i].Day != models[j].Day {
			return models[i].Day < models[j].Day
		}
		return models[i].Model < models[j].Model
	})
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Day != tools[j].Day {
			return tools[i].Day < tools[j].Day
		}
		return tools[i].Tool < tools[j].Tool
	})

	tmp, err := os.MkdirTemp(c.root, ".tmpidx-*")
	if err != nil {
		return fmt.Errorf("create temp index: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := writeParquet(filepath.Join(tmp, dayFile), days); err != nil {
		return err
	}
	if err := writeParquet(filepath.Join(tmp, modelsFile), models); err != nil {
		return err
	}
	if err := writeParquet(filepath.Join(tmp, toolsFile), tools); err != nil {
		return err
	}
	if err := writeParquet(filepath.Join(tmp, projectsFile), projects); err != nil {
		return err
	}

	final := filepath.Join(c.root, indexDir)
	old, err := os.MkdirTemp(c.root, ".oldidx-*")
	if err != nil {
		return fmt.Errorf("stage index swap: %w", err)
	}
	defer os.RemoveAll(old)

	// Swap the new index in: move any existing one aside first, since rename
	// onto an existing directory fails on Windows.
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, filepath.Join(old, "prev")); err != nil {
			return fmt.Errorf("retire old index: %w", err)
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("publish index: %w", err)
	}
	return nil
}

// index is the consolidated rollup, keyed by day.
type index struct {
	days     map[string]DayRow
	models   map[string][]ModelRow
	tools    map[string][]ToolRow
	projects map[string][]ProjectRow
}

// loadIndex reads the consolidated rollup in three file opens, regardless of
// how many days it covers. A missing index is not an error: callers fall back
// to per-day partitions.
func (c *Compactor) loadIndex() (*index, error) {
	days, err := readOrMissing[DayRow](c.indexPath(dayFile))
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}
	models, err := readOrMissing[ModelRow](c.indexPath(modelsFile))
	if err != nil {
		return nil, err
	}
	tools, err := readOrMissing[ToolRow](c.indexPath(toolsFile))
	if err != nil {
		return nil, err
	}
	projects, err := readOrMissing[ProjectRow](c.indexPath(projectsFile))
	if err != nil {
		return nil, err
	}

	idx := &index{
		days:     make(map[string]DayRow, len(days)),
		models:   make(map[string][]ModelRow),
		tools:    make(map[string][]ToolRow),
		projects: make(map[string][]ProjectRow),
	}
	for _, d := range days {
		idx.days[d.Day] = d
	}
	for _, m := range models {
		idx.models[m.Day] = append(idx.models[m.Day], m)
	}
	for _, t := range tools {
		idx.tools[t.Day] = append(idx.tools[t.Day], t)
	}
	for _, p := range projects {
		idx.projects[p.Day] = append(idx.projects[p.Day], p)
	}
	return idx, nil
}

// add folds one day of the consolidated index into agg, reporting whether that
// day was present.
func (i *index) add(day time.Time, agg *Aggregate) bool {
	key := day.Format("2006-01-02")
	row, ok := i.days[key]
	if !ok {
		return false
	}

	addDayRow(row, agg)
	for _, m := range i.models[key] {
		agg.ByModel[m.Model] += int(m.Turns)
	}
	for _, t := range i.tools[key] {
		agg.ToolCalls[t.Tool] += int(t.Calls)
	}
	for _, p := range i.projects[key] {
		agg.addProject(p.ProjectID, p.Project, p.InRepo, int(p.Turns), int(p.Sessions),
			int(p.CacheRead), int(p.InputTokens), int(p.OutputTokens), int(p.CacheWrite),
			p.CostUSD, int(p.Errors))
	}
	return true
}
