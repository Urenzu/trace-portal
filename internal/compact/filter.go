package compact

import (
	"strconv"
	"strings"

	"github.com/Urenzu/trace-portal/internal/query"
)

// Filter narrows a session listing.
//
// There is no full-text index here, and there should not be: trace-portal
// records no prompts, responses, or tool arguments, so there is no corpus to
// index. What a session has is a handful of columns — project, branch, model,
// the tools it invoked, its id — every one of them dictionary-encoded in the
// Parquet partitions the listing already reads. A typed predicate over those
// columns answers the questions people actually ask ("what did I spend on
// fin-agentic", "which sessions hit errors") and costs a string compare per
// candidate, against an index that would have to be built, stored, and kept
// consistent with the archive.
//
// The syntax is a bare word, which matches any of those columns, or a
// field-qualified term:
//
//	fin-agentic              any column contains it
//	project:trace-portal     that column contains it
//	branch:main model:opus   several terms, all of which must hold
//	tool:PowerShell
//	has:errors               sessions with a failed turn
//	cost:>5 turns:>100       numeric thresholds
//
// Matching is case-insensitive and substring-based. Nobody remembers whether
// the repository was "fin-agentic" or "fin_agentic", and a search that returns
// nothing for a near-miss is worse than one that returns a few extra rows.
type Filter struct {
	terms []term
}

type term struct {
	field string
	value string
	// num carries a parsed threshold for the numeric fields; op is "", ">" or
	// "<".
	op  string
	num float64
}

// ParseFilter builds a Filter from a query string. A malformed term is kept as
// a plain word rather than rejected: someone typing mid-query should get
// narrowing results, not an error.
func ParseFilter(q string) Filter {
	var f Filter
	for _, raw := range splitTerms(q) {
		field, value := "any", raw
		if name, rest, ok := strings.Cut(raw, ":"); ok && rest != "" {
			switch strings.ToLower(name) {
			case "project", "branch", "model", "tool", "id", "has", "cost", "turns":
				field, value = strings.ToLower(name), rest
			}
		}

		t := term{field: field, value: strings.ToLower(strings.Trim(value, `"`))}
		if field == "cost" || field == "turns" {
			t.op = ""
			body := t.value
			if len(body) > 0 && (body[0] == '>' || body[0] == '<') {
				t.op, body = string(body[0]), body[1:]
			}
			n, err := strconv.ParseFloat(strings.TrimPrefix(body, "="), 64)
			if err != nil {
				// Not a number after all — treat the whole thing as a word so
				// the query still does something sensible.
				t = term{field: "any", value: strings.ToLower(raw)}
			} else {
				t.num = n
			}
		}
		f.terms = append(f.terms, t)
	}
	return f
}

// splitTerms splits on whitespace, keeping quoted runs together so a project
// name with a space in it can be searched for.
func splitTerms(q string) []string {
	var (
		out     []string
		current strings.Builder
		quoted  bool
	)
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return out
}

// Empty reports whether the filter admits everything.
func (f Filter) Empty() bool { return len(f.terms) == 0 }

// Match reports whether a session satisfies every term.
func (f Filter) Match(s query.Session) bool {
	for _, t := range f.terms {
		if !t.match(s) {
			return false
		}
	}
	return true
}

func (t term) match(s query.Session) bool {
	switch t.field {
	case "project":
		return contains(s.Project, t.value)
	case "branch":
		return contains(s.GitBranch, t.value)
	case "model":
		return containsAny(append([]string{s.Model}, s.Models...), t.value)
	case "tool":
		return containsAny(s.ToolNames, t.value)
	case "id":
		return contains(s.ID, t.value)
	case "has":
		switch t.value {
		case "errors", "error", "failed":
			return s.Errors > 0
		case "tools":
			return len(s.ToolNames) > 0
		case "unpriced":
			return s.Unpriced > 0
		}
		return false
	case "cost":
		return compare(s.CostUSD, t.op, t.num)
	case "turns":
		return compare(float64(s.Turns), t.op, t.num)
	default:
		// A bare word searches everything a session is identified by. The id is
		// included so a link pasted back into the box finds its session.
		return contains(s.Project, t.value) ||
			contains(s.GitBranch, t.value) ||
			contains(s.ID, t.value) ||
			containsAny(append([]string{s.Model}, s.Models...), t.value) ||
			containsAny(s.ToolNames, t.value)
	}
}

func compare(have float64, op string, want float64) bool {
	switch op {
	case ">":
		return have > want
	case "<":
		return have < want
	default:
		return have == want
	}
}

func contains(haystack, needle string) bool {
	return needle != "" && strings.Contains(strings.ToLower(haystack), needle)
}

func containsAny(haystack []string, needle string) bool {
	for _, h := range haystack {
		if contains(h, needle) {
			return true
		}
	}
	return false
}
