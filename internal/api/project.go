package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Named is a name with a count, already ranked. A map would be equivalent and
// smaller, but the ordering is the point — what the reader wants is the
// leaders, and a map hands the browser the job of finding them.
type Named struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// BranchStat is one branch's share of a project.
//
// Branch is the unit of work a person recognises. "Which repository costs most"
// is answered on the dashboard; inside a repository the next question is which
// piece of work, and a branch names that better than a date does.
type BranchStat struct {
	Branch       string  `json:"branch"`
	Sessions     int     `json:"sessions"`
	Turns        int     `json:"turns"`
	CostUSD      float64 `json:"cost_usd"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	Errors       int     `json:"errors,omitempty"`
	LastActive   string  `json:"last_active,omitempty"`

	// Token counts the hit rate is recomputed from once the whole branch has
	// been folded in. Averaging the sessions' own rates would weight a
	// three-turn session the same as a thousand-turn one.
	cacheRead, input, write int
}

// ProjectDetail is one project's page: what it cost, how that moved, and what
// it was spent on.
type ProjectDetail struct {
	Project   string `json:"project"`
	ProjectID string `json:"project_id"`
	InRepo    bool   `json:"in_repo"`
	Found     bool   `json:"found"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Turns        int         `json:"turns"`
	Sessions     int         `json:"sessions"`
	Errors       int         `json:"errors"`
	CostUSD      float64     `json:"cost_usd"`
	Usage        trace.Usage `json:"usage"`
	CacheHitRate float64     `json:"cache_hit_rate"`

	ByDay    []compact.DayPoint `json:"by_day,omitempty"`
	ByBranch []BranchStat       `json:"by_branch,omitempty"`
	ByModel  []Named            `json:"by_model,omitempty"`
	Tools    []Named            `json:"tools,omitempty"`

	// The typical session and the two that are not. A mean session cost is
	// dragged well above anything that actually happened by one runaway; the
	// median plus the extreme is what a person can act on.
	MedianCost float64        `json:"median_session_cost"`
	Costliest  *query.Session `json:"costliest_session,omitempty"`
	Longest    *query.Session `json:"longest_session,omitempty"`
}

// handleProject answers one project's page.
//
// Totals come from the same day rollups the dashboard row does, so the page
// agrees with the row that was clicked to reach it. The series comes from the
// per-project rollup, one row per day. Only the cuts by branch, model and tool
// need the sessions themselves — read once here rather than by the browser.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scope(w, r)
	if !ok {
		return
	}
	from, to := s.window(r)
	id := r.PathValue("id")

	agg, err := sc.Compact.AggregateRange(from, to)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	detail := ProjectDetail{ProjectID: id, From: from, To: to}
	if totals := agg.ByProject[id]; totals != nil {
		detail.Found = true
		detail.Project, detail.InRepo = totals.Project, totals.InRepo
		detail.Turns, detail.Errors, detail.CostUSD = totals.Turns, totals.Errors, totals.CostUSD
		detail.Usage = trace.Usage{
			InputTokens:              totals.Input,
			OutputTokens:             totals.Output,
			CacheCreationInputTokens: totals.Write,
			CacheReadInputTokens:     totals.CacheRead,
		}
		detail.CacheHitRate = inputCacheHitRate(detail.Usage)
	}

	if detail.ByDay, err = sc.Compact.ProjectDaily(from, to, id); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	// One read of the window's turns answers three questions the rollup cannot:
	// which branches, which models, and which tools. Rolling them up into
	// sessions discards the per-turn detail, so the turns come back here and the
	// roll-up happens after the histograms are taken.
	turns, err := sc.Compact.SessionTurnsRange(from, to)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}

	models := map[string]int{}
	tools := map[string]int{}
	mine := make([]query.Turn, 0, len(turns))
	for _, t := range turns {
		if t.ProjectID != id {
			continue
		}
		mine = append(mine, t)
		if t.Model != "" {
			models[t.Model]++
		}
		for _, call := range t.ToolCalls {
			tools[call.Name]++
		}
	}

	sessions := query.SessionsFromTurns(mine)
	branches := map[string]*BranchStat{}
	var costs []float64

	for i := range sessions {
		sess := &sessions[i]
		detail.Found = true
		if detail.Project == "" {
			detail.Project = sess.Project
		}
		detail.Sessions++
		costs = append(costs, sess.CostUSD)

		if detail.Costliest == nil || sess.CostUSD > detail.Costliest.CostUSD {
			detail.Costliest = sess
		}
		if detail.Longest == nil || sess.Turns > detail.Longest.Turns {
			detail.Longest = sess
		}

		name := sess.GitBranch
		if name == "" || name == "HEAD" {
			name = "(no branch)"
		}
		b := branches[name]
		if b == nil {
			b = &BranchStat{Branch: name}
			branches[name] = b
		}
		b.Sessions++
		b.Turns += sess.Turns
		b.CostUSD += sess.CostUSD
		b.Errors += sess.Errors
		b.cacheRead += sess.Usage.CacheReadInputTokens
		b.input += sess.Usage.InputTokens
		b.write += sess.Usage.CacheCreationInputTokens
		if ended := sess.EndedAt.Format(time.RFC3339); ended > b.LastActive {
			b.LastActive = ended
		}

	}

	detail.MedianCost = median(costs)
	detail.ByBranch = rankBranches(branches)
	detail.ByModel = rank(models)
	detail.Tools = rank(tools)

	writeJSON(w, http.StatusOK, detail)
}

func rank(counts map[string]int) []Named {
	out := make([]Named, 0, len(counts))
	for name, n := range counts {
		out = append(out, Named{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Name < out[j].Name
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func rankBranches(in map[string]*BranchStat) []BranchStat {
	out := make([]BranchStat, 0, len(in))
	for _, b := range in {
		if total := b.cacheRead + b.input + b.write; total > 0 {
			b.CacheHitRate = float64(b.cacheRead) / float64(total)
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out
}

// median is the middle value, which describes a typical session far better than
// the mean: one runaway session drags a mean above anything that happened.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
