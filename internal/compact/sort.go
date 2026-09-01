package compact

import (
	"sort"
	"strconv"
	"time"

	"github.com/Urenzu/trace-portal/internal/query"
)

// Order names how a session listing is ranked.
type Order string

const (
	// OrderRecent is the default and the only one that can be paged lazily:
	// walking days backwards yields sessions already in this order, so a page
	// costs the days it takes to fill and no more.
	OrderRecent Order = "recent"
	OrderCost   Order = "cost"
	OrderTurns  Order = "turns"
	OrderErrors Order = "errors"
)

// ParseOrder maps a query parameter to an order, defaulting to recent.
func ParseOrder(s string) Order {
	switch Order(s) {
	case OrderCost, OrderTurns, OrderErrors:
		return Order(s)
	default:
		return OrderRecent
	}
}

// Lazy reports whether the order can be answered by the paging scan.
func (o Order) Lazy() bool { return o == OrderRecent }

// SessionsRanked lists sessions in a window under a non-recent order.
//
// "Most expensive first" cannot be answered by walking days backwards: the
// costliest session may be the oldest one in the window, so the answer is not
// known until every day has been read. That makes this the expensive listing
// by construction, and it is why recency stays the default and keeps its own
// lazy path — this one is what a reader asks for deliberately.
//
// Paging is by offset rather than by a value cursor. The whole window is
// already in memory to have been sorted at all, and an offset cannot skip or
// repeat a row the way a value cursor does when two sessions tie.
func (c *Compactor) SessionsRanked(from, to time.Time, limit int, encodedCursor string, filter Filter, order Order) (SessionPage, error) {
	if limit <= 0 {
		limit = 50
	}
	offset := 0
	if encodedCursor != "" {
		n, err := strconv.Atoi(encodedCursor)
		if err != nil || n < 0 {
			return SessionPage{}, errBadCursor
		}
		offset = n
	}

	sessions, err := c.SessionsRange(from, to)
	if err != nil {
		return SessionPage{}, err
	}

	matched := sessions[:0]
	for _, s := range sessions {
		if filter.Match(s) {
			matched = append(matched, s)
		}
	}

	// A total order in every case: ties fall back to recency and then to the
	// id, so a page boundary lands in the same place on every request.
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		switch order {
		case OrderCost:
			if a.CostUSD != b.CostUSD {
				return a.CostUSD > b.CostUSD
			}
		case OrderTurns:
			if a.Turns != b.Turns {
				return a.Turns > b.Turns
			}
		case OrderErrors:
			if a.Errors != b.Errors {
				return a.Errors > b.Errors
			}
		}
		if !a.EndedAt.Equal(b.EndedAt) {
			return a.EndedAt.After(b.EndedAt)
		}
		return a.ID > b.ID
	})

	page := SessionPage{
		DaysScanned: daysBetween(from, to),
		Sessions:    []query.Session{},
	}
	if offset < len(matched) {
		end := offset + limit
		if end > len(matched) {
			end = len(matched)
		}
		page.Sessions = matched[offset:end]
		if end < len(matched) {
			page.NextCursor = strconv.Itoa(end)
		}
	}
	return page, nil
}

func daysBetween(from, to time.Time) int {
	from, to = truncateDay(from), truncateDay(to)
	if to.Before(from) {
		return 0
	}
	return int(to.Sub(from).Hours()/24) + 1
}
