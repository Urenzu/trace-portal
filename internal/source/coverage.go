package source

import (
	"encoding/json"
	"sort"
	"sync"
)

// Coverage records what a scan understood and what it did not.
//
// Agent log formats are internal and move constantly — Claude Code shipped
// fifteen versions and three schema changes in under a month. The risk is not
// that parsing breaks loudly; it is that a renamed field starts reading as zero
// and nobody notices. Counting outcomes per producing version turns that silent
// drift into something visible.
type Coverage struct {
	mu sync.Mutex

	// Records is every line examined, Parsed the subset that became an event,
	// and Skipped the rest — most of which are legitimately not API calls.
	Records int `json:"records"`
	Parsed  int `json:"parsed"`
	Skipped int `json:"skipped"`

	// Unreadable is a record this build could not decode: valid JSON whose
	// shape it does not model, or genuinely broken input. Either way the turn
	// is lost, which is why it is counted rather than ignored.
	Unreadable int `json:"unreadable"`

	// ByVersion counts parsed turns per producing tool version.
	ByVersion map[string]int `json:"by_version,omitempty"`

	// MissingField counts turns whose producing version did not report a field
	// this tool knows how to read. It is how a build that predates a field
	// becomes visible instead of silently reading as zero.
	MissingField map[string]int `json:"missing_field,omitempty"`

	// UnknownField counts keys seen in the log that this build does not read.
	// A new one appearing is the earliest warning that the format moved and
	// there is now data being left on the floor.
	UnknownField map[string]int `json:"unknown_field,omitempty"`
}

// NewCoverage returns an empty Coverage ready to record into.
func NewCoverage() *Coverage {
	return &Coverage{
		ByVersion:    map[string]int{},
		MissingField: map[string]int{},
		UnknownField: map[string]int{},
	}
}

func (c *Coverage) record(version string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Parsed++
	if version != "" {
		c.ByVersion[version]++
	}
}

func (c *Coverage) skip(malformed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Skipped++
	if malformed {
		c.Unreadable++
	}
}

func (c *Coverage) seen() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Records++
}

func (c *Coverage) missing(field string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MissingField[field]++
}

// noteUnknown records top-level keys present in a record that this build does
// not read, so a format change shows up as a rising count rather than as data
// quietly going missing.
func (c *Coverage) noteUnknown(raw []byte, known map[string]bool) {
	if c == nil {
		return
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range all {
		if !known[key] {
			c.UnknownField[key]++
		}
	}
}

// Merge folds another coverage report into this one.
func (c *Coverage) Merge(other *Coverage) {
	if c == nil || other == nil {
		return
	}
	other.mu.Lock()
	snapshot := Coverage{
		Records: other.Records, Parsed: other.Parsed,
		Skipped: other.Skipped, Unreadable: other.Unreadable,
		ByVersion:    maps(other.ByVersion),
		MissingField: maps(other.MissingField),
		UnknownField: maps(other.UnknownField),
	}
	other.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.Records += snapshot.Records
	c.Parsed += snapshot.Parsed
	c.Skipped += snapshot.Skipped
	c.Unreadable += snapshot.Unreadable
	for k, v := range snapshot.ByVersion {
		c.ByVersion[k] += v
	}
	for k, v := range snapshot.MissingField {
		c.MissingField[k] += v
	}
	for k, v := range snapshot.UnknownField {
		c.UnknownField[k] += v
	}
}

// Reset clears the counters after they have been folded into a persisted
// total, so the same records cannot be counted twice.
func (c *Coverage) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Records, c.Parsed, c.Skipped, c.Unreadable = 0, 0, 0, 0
	c.ByVersion = map[string]int{}
	c.MissingField = map[string]int{}
	c.UnknownField = map[string]int{}
}

// Versions lists the producing versions seen, sorted.
func (c *Coverage) Versions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.ByVersion))
	for v := range c.ByVersion {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func maps(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
