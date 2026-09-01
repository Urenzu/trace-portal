package compact

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Urenzu/trace-portal/internal/objectstore"
)

// partitionDays lists the days that have a partition, from the keys in the
// object store.
//
// A listing rather than a directory walk, because there are no directories in a
// bucket -- a key is one string and the slashes are a convention. Days are
// recognised by the shape of their first segment, so anything else under the
// root (the rollup prefix, a stray object) is ignored rather than mistaken for
// history.
func (c *Compactor) partitionDays() ([]time.Time, error) {
	keys, err := c.objects.List(storeContext(), "")
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}

	seen := map[string]bool{}
	var days []time.Time
	for _, key := range keys {
		segment, _, ok := strings.Cut(key, "/")
		if !ok || segment == indexDir || seen[segment] {
			continue
		}
		day, err := time.ParseInLocation("2006-01-02", segment, time.UTC)
		if err != nil {
			continue
		}
		seen[segment] = true
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	return days, nil
}

// The consolidated index holds every day's rollups in one set of files rather
// than one set per day. Per-day partitions stay the source of truth — a
// partition is self-contained and can be rebuilt or copied on its own — but
// answering a wide window from them means one cold file open per day, and cold
// opens cost milliseconds each. A year of history is 365 rollup rows; keeping
// them together turns a full-history dashboard query into a fixed handful of
// file opens, independent of how much history exists.
const indexDir = "rollup"

func (c *Compactor) indexKey(name string) string {
	return objectstore.Key(indexDir, name)
}

// RebuildIndex regenerates the consolidated rollup from every partition. It is
// called after compaction, and is cheap: the inputs are one row per day.
func (c *Compactor) RebuildIndex() error {
	entries, err := c.partitionDays()
	if err != nil {
		return err
	}

	var (
		days     []DayRow
		models   []ModelRow
		tools    []ToolRow
		projects []ProjectRow
		sessions []SessionDayRow
	)
	for _, day := range entries {
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

		sd, err := c.ReadSessionDays(day)
		if err != nil {
			return err
		}
		sessions = append(sessions, sd...)
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

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].SessionID != sessions[j].SessionID {
			return sessions[i].SessionID < sessions[j].SessionID
		}
		return sessions[i].Day < sessions[j].Day
	})

	// Each rollup is published by overwriting its object. A PUT is atomic, so a
	// reader sees either the whole previous version of a file or the whole new
	// one -- but the five files are five PUTs, so a reader can briefly catch a
	// mix of old and new.
	//
	// That is harmless here and worth stating rather than engineering around.
	// Every row in every rollup is keyed by day and is independently correct;
	// the only difference between the versions is which days they cover. The
	// worst observable outcome is a just-compacted day appearing in the totals
	// a moment before it appears in the model split, on a rebuild that takes
	// milliseconds and is idempotent. A manifest object naming a consistent set
	// would remove even that, at the cost of a second round trip on every read.
	ctx := storeContext()
	writes := []struct {
		name string
		put  func() error
	}{
		{dayFile, func() error { return putParquet(ctx, c.objects, c.indexKey(dayFile), days) }},
		{modelsFile, func() error { return putParquet(ctx, c.objects, c.indexKey(modelsFile), models) }},
		{toolsFile, func() error { return putParquet(ctx, c.objects, c.indexKey(toolsFile), tools) }},
		{projectsFile, func() error { return putParquet(ctx, c.objects, c.indexKey(projectsFile), projects) }},
		{sessionsFile, func() error { return putParquet(ctx, c.objects, c.indexKey(sessionsFile), sessions) }},
	}
	for _, w := range writes {
		if err := w.put(); err != nil {
			// The cached copy is dropped even on a partial failure: some of the
			// objects have changed, so what is held no longer matches what is
			// stored, and serving it would be serving a version that never
			// existed.
			c.invalidateIndex()
			return fmt.Errorf("publish rollup %s: %w", w.name, err)
		}
	}
	c.invalidateIndex()
	return nil
}

// index is the consolidated rollup, keyed by day.
type index struct {
	days     map[string]DayRow
	models   map[string][]ModelRow
	tools    map[string][]ToolRow
	projects map[string][]ProjectRow
	// sessionDays maps a session to the days it touched, oldest first. A
	// session may skip days entirely, which is why the days it did touch have
	// to be recorded rather than inferred from its first and last turn.
	sessionDays map[string][]time.Time
}

// loadIndex returns the consolidated rollup, from memory when it is already
// held.
//
// The whole rollup is 14 KB for 15 days and grows about a kilobyte a day, so a
// year is a few hundred KB -- small enough to simply keep. Re-reading it is
// five object reads, which cost microseconds from a local directory and a
// network round trip each from a bucket, on every dashboard load.
//
// Invalidation is the easy half here, and worth stating plainly: these files
// are written in exactly one place, by RebuildIndex, at the end of compaction.
// Nothing else can change them, so dropping the cached copy there is complete
// by construction -- there is no expiry to tune and no staleness window to
// reason about, because there is no other writer to race.
//
// The one case that is not covered is a second process compacting the same
// archive: this cache would not hear about it. That is out of scope by design
// rather than by oversight -- a tenant is compacted by one process, and two
// writers over one archive is a problem this system does not have.
func (c *Compactor) loadIndex() (*index, error) {
	c.indexMu.RLock()
	cached := c.index
	c.indexMu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	idx, err := c.readIndex()
	if err != nil {
		return nil, err
	}
	c.indexMu.Lock()
	c.index = idx
	c.indexMu.Unlock()
	return idx, nil
}

// invalidateIndex drops the cached rollup. Called wherever the rollup is
// written, which is one place.
func (c *Compactor) invalidateIndex() {
	c.indexMu.Lock()
	c.index = nil
	c.indexMu.Unlock()
}

// readIndex reads the consolidated rollup in a fixed number of object reads,
// regardless of how many days it covers. A missing index is not an error:
// callers fall back to per-day partitions.
func (c *Compactor) readIndex() (*index, error) {
	days, err := getParquet[DayRow](storeContext(), c.objects, c.indexKey(dayFile))
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, nil
	}
	models, err := getParquet[ModelRow](storeContext(), c.objects, c.indexKey(modelsFile))
	if err != nil {
		return nil, err
	}
	tools, err := getParquet[ToolRow](storeContext(), c.objects, c.indexKey(toolsFile))
	if err != nil {
		return nil, err
	}
	projects, err := getParquet[ProjectRow](storeContext(), c.objects, c.indexKey(projectsFile))
	if err != nil {
		return nil, err
	}
	sessions, err := getParquet[SessionDayRow](storeContext(), c.objects, c.indexKey(sessionsFile))
	if err != nil {
		return nil, err
	}

	idx := &index{
		days:        make(map[string]DayRow, len(days)),
		models:      make(map[string][]ModelRow),
		tools:       make(map[string][]ToolRow),
		projects:    make(map[string][]ProjectRow),
		sessionDays: make(map[string][]time.Time),
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
	for _, sd := range sessions {
		day, err := time.ParseInLocation("2006-01-02", sd.Day, time.UTC)
		if err != nil {
			continue
		}
		idx.sessionDays[sd.SessionID] = append(idx.sessionDays[sd.SessionID], day)
	}
	for id, days := range idx.sessionDays {
		sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
		idx.sessionDays[id] = days
	}
	return idx, nil
}

// daysFor returns the compacted days a session touched, oldest first, and
// whether the index knows the session at all.
func (i *index) daysFor(id string) ([]time.Time, bool) {
	if i == nil {
		return nil, false
	}
	days, ok := i.sessionDays[id]
	return days, ok
}

// oldestDay is the earliest compacted day a session touched. It is what tells a
// backwards scan that a session cannot gain any more turns, without assuming
// its days are contiguous.
func (i *index) oldestDay(id string) (time.Time, bool) {
	days, ok := i.daysFor(id)
	if !ok || len(days) == 0 {
		return time.Time{}, false
	}
	return days[0], true
}

// covers reports whether the index has a rollup for a day. A day compacted
// after the last rebuild is not covered, and must not be treated as empty.
func (i *index) covers(day time.Time) bool {
	if i == nil {
		return false
	}
	_, ok := i.days[day.UTC().Format("2006-01-02")]
	return ok
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
