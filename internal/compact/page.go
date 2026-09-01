package compact

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
)

// SessionPage is one page of the session list, newest first.
type SessionPage struct {
	Sessions []query.Session `json:"sessions"`
	// NextCursor is empty when the last page has been reached.
	NextCursor string `json:"next_cursor,omitempty"`
	// DaysScanned reports how many daily partitions were read to fill the
	// page. Listing walks backwards from the newest day and stops as soon as
	// the page is full, so this is normally far smaller than the window.
	DaysScanned int `json:"days_scanned"`
}

// Cursor identifies the last session on a page. Sessions are ordered by end
// time descending, with the ID breaking ties so the order is total and a page
// boundary can never repeat or skip a session.
type cursor struct {
	endedAt time.Time
	id      string
}

func encodeCursor(s query.Session) string {
	raw := fmt.Sprintf("%d|%s", s.EndedAt.UTC().UnixMilli(), s.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(encoded string) (cursor, error) {
	if encoded == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return cursor{}, fmt.Errorf("invalid cursor")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return cursor{}, fmt.Errorf("invalid cursor")
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("invalid cursor")
	}
	return cursor{endedAt: time.UnixMilli(ms).UTC(), id: parts[1]}, nil
}

// after reports whether s sorts strictly after the cursor, i.e. belongs on a
// later page.
func (c cursor) after(s query.Session) bool {
	if c.id == "" {
		return true // no cursor: everything qualifies
	}
	if s.EndedAt.Equal(c.endedAt) {
		return s.ID < c.id
	}
	return s.EndedAt.Before(c.endedAt)
}

// SessionsPage lists sessions newest-first, reading only as many daily
// partitions as the page needs.
//
// Days are walked backwards from the newest. Once no older day can add to a
// session it is complete and safe to emit; sessions still open at the scan
// frontier are held back, which is what keeps a paged list identical to an
// unpaged one.
//
// The filter is applied here rather than in the browser, so a search covers the
// whole window instead of whichever page happens to be loaded. It costs a
// string compare per completed session, and narrows nothing that has been read
// — a narrow search simply walks further back before the page fills, which
// DaysScanned reports.
func (c *Compactor) SessionsPage(from, to time.Time, limit int, encodedCursor string, filter Filter) (SessionPage, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}
	if limit <= 0 {
		limit = 50
	}

	cur, err := decodeCursor(encodedCursor)
	if err != nil {
		return SessionPage{}, err
	}

	// The session-day index says which days each session actually touched, so a
	// session paused over an idle day is held rather than emitted as finished.
	// Without it, one conversation resumed the next day was listed twice.
	idx, err := c.loadIndex()
	if err != nil {
		return SessionPage{}, err
	}

	var (
		page      SessionPage
		pending   = map[string][]query.Turn{} // sessions still at the frontier
		complete  []query.Session
		firstDay  = truncateDay(from)
		lastDay   = truncateDay(to)
		scanned   int
		exhausted = true
	)

	for day := lastDay; !day.Before(firstDay); day = day.AddDate(0, 0, -1) {
		turns, err := c.sessionTurnsForDay(day)
		if err != nil {
			return SessionPage{}, err
		}
		scanned++

		for _, t := range turns {
			if t.StartedAt.Before(from) || t.StartedAt.After(to) {
				continue
			}
			pending[t.SessionID] = append(pending[t.SessionID], t)
		}

		// A session is complete once the day just read is older than the oldest
		// day it is known to touch. Its own turns give a lower bound; the index
		// supplies the true oldest day, including days it skipped entirely.
		for id, ts := range pending {
			if oldestDay(idx, id, ts).After(day) {
				s := query.SessionsFromTurns(ts)[0]
				delete(pending, id)
				if cur.after(s) && filter.Match(s) {
					complete = append(complete, s)
				}
			}
		}

		// Stop as soon as enough complete sessions exist to fill the page.
		if len(complete) > limit {
			exhausted = false
			break
		}
	}

	// Anything still pending at the end of the window is complete by
	// definition — there are no older days left to read.
	if exhausted {
		for _, ts := range pending {
			s := query.SessionsFromTurns(ts)[0]
			if cur.after(s) && filter.Match(s) {
				complete = append(complete, s)
			}
		}
	}

	sort.Slice(complete, func(i, j int) bool {
		if complete[i].EndedAt.Equal(complete[j].EndedAt) {
			return complete[i].ID > complete[j].ID
		}
		return complete[i].EndedAt.After(complete[j].EndedAt)
	})

	page.DaysScanned = scanned
	if len(complete) > limit {
		page.Sessions = complete[:limit]
		page.NextCursor = encodeCursor(complete[limit-1])
	} else {
		page.Sessions = complete
	}
	if page.Sessions == nil {
		page.Sessions = []query.Session{}
	}
	return page, nil
}

// oldestDay is the earliest day a session is known to touch: the earliest of
// its turns seen so far, pulled back to whatever the index knows. Reading only
// the turns would place a session's start at the newest side of an idle gap.
func oldestDay(idx *index, id string, seen []query.Turn) time.Time {
	oldest := earliestDay(seen)
	if indexed, ok := idx.oldestDay(id); ok && indexed.Before(oldest) {
		return indexed
	}
	return oldest
}

func earliestDay(turns []query.Turn) time.Time {
	earliest := turns[0].StartedAt
	for _, t := range turns[1:] {
		if t.StartedAt.Before(earliest) {
			earliest = t.StartedAt
		}
	}
	return truncateDay(earliest)
}

// SessionDetail returns one session with its turns, oldest turn first, reading
// only the days that session touches.
//
// Those days are not contiguous. A conversation resumed the following morning
// has turns either side of an idle day, and a scan that stopped at the first
// empty day returned only the newest fragment — the session looked shorter and
// cheaper than it was, and its earlier days vanished. The session-day index
// names the days to read instead of inferring them.
func (c *Compactor) SessionDetail(from, to time.Time, id string) (query.SessionDetail, bool, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}

	idx, err := c.loadIndex()
	if err != nil {
		return query.SessionDetail{}, false, err
	}

	var found []query.Turn
	for _, day := range c.daysHolding(idx, id, from, to) {
		turns, err := c.turnsForDay(day)
		if err != nil {
			return query.SessionDetail{}, false, err
		}
		for _, t := range turns {
			if t.SessionID != id || t.StartedAt.Before(from) || t.StartedAt.After(to) {
				continue
			}
			found = append(found, t)
		}
	}

	if len(found) == 0 {
		return query.SessionDetail{}, false, nil
	}
	// Days are read oldest first and each day's turns are already ordered, but
	// the sort is what makes chronological order a property of the result
	// rather than of the read order. The UI marks day boundaries from these
	// timestamps, so an out-of-order turn would mark a boundary that never
	// happened.
	sort.SliceStable(found, func(i, j int) bool { return found[i].StartedAt.Before(found[j].StartedAt) })
	detail, ok := query.SessionDetailFromTurns(found, id)
	return detail, ok, nil
}

// daysHolding lists the days in the window that may hold turns for a session,
// oldest first.
//
// A compacted day is read only when the index places the session on it. An
// uncompacted day — today, or one not yet rolled up — is always read, since the
// index cannot know about it yet. With no index at all there is nothing to
// narrow by, so every day in the window is read.
func (c *Compactor) daysHolding(idx *index, id string, from, to time.Time) []time.Time {
	indexed := map[string]bool{}
	days, known := idx.daysFor(id)
	for _, d := range days {
		indexed[d.Format("2006-01-02")] = true
	}

	var out []time.Time
	for day := truncateDay(from); !day.After(truncateDay(to)); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		switch {
		case idx == nil, !c.IsCompacted(day):
			out = append(out, day)
		case !idx.covers(day):
			// Compacted but not yet in the index: the index cannot rule this
			// day out, so it is read rather than trusted.
			out = append(out, day)
		case known && indexed[key]:
			out = append(out, day)
		}
	}
	return out
}
