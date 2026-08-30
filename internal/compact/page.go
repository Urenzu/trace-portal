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
// Days are walked backwards from the newest. A session's turns are contiguous
// in time, so once a whole day older than a session's first turn has been read,
// that session cannot gain more turns and is safe to emit. Sessions still open
// at the scan frontier are held back until they are complete, which is what
// keeps a paged list identical to an unpaged one.
func (c *Compactor) SessionsPage(from, to time.Time, limit int, encodedCursor string) (SessionPage, error) {
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

		// Every session whose earliest turn falls on a later day than the one
		// just read is now complete: no older day can contain more of it.
		for id, ts := range pending {
			if earliestDay(ts).After(day) {
				s := query.SessionsFromTurns(ts)[0]
				delete(pending, id)
				if cur.after(s) {
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
			if cur.after(s) {
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

func earliestDay(turns []query.Turn) time.Time {
	earliest := turns[0].StartedAt
	for _, t := range turns[1:] {
		if t.StartedAt.Before(earliest) {
			earliest = t.StartedAt
		}
	}
	return truncateDay(earliest)
}

// SessionDetail returns one session with its turns, reading only the days that
// session touches.
//
// A session's turns are contiguous in time, so scanning backwards can stop at
// the first day that contains none of them once some have been found. Looking
// up a session in a year-long window therefore costs a couple of partitions
// rather than all of them.
func (c *Compactor) SessionDetail(from, to time.Time, id string) (query.SessionDetail, bool, error) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		from, to = to, from
	}

	var found []query.Turn
	for day := truncateDay(to); !day.Before(truncateDay(from)); day = day.AddDate(0, 0, -1) {
		turns, err := c.turnsForDay(day)
		if err != nil {
			return query.SessionDetail{}, false, err
		}

		var onThisDay int
		for _, t := range turns {
			if t.SessionID != id || t.StartedAt.Before(from) || t.StartedAt.After(to) {
				continue
			}
			found = append(found, t)
			onThisDay++
		}

		// Nothing on this day and something already found: the session began
		// later than this, so no older day can hold more of it.
		if onThisDay == 0 && len(found) > 0 {
			break
		}
	}

	if len(found) == 0 {
		return query.SessionDetail{}, false, nil
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].StartedAt.Before(found[j].StartedAt) })
	detail, ok := query.SessionDetailFromTurns(found, id)
	return detail, ok, nil
}
