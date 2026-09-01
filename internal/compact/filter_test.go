package compact

import (
	"testing"

	"github.com/Urenzu/trace-portal/internal/query"
)

func sample() query.Session {
	return query.Session{
		ID:        "9f2c-abc",
		Project:   "fin-agentic",
		GitBranch: "feature/timeline-fix",
		Model:     "claude-opus-5",
		Models:    []string{"claude-opus-5", "claude-haiku-4-5-20251001"},
		ToolNames: []string{"Bash", "PowerShell", "Write"},
		Turns:     430,
		CostUSD:   109.51,
		Errors:    1,
	}
}

func TestFilterMatching(t *testing.T) {
	s := sample()
	cases := []struct {
		query string
		want  bool
	}{
		{"", true},
		{"fin", true},
		{"FIN-AGENTIC", true}, // case-insensitive
		{"agentic", true},     // substring, not prefix
		{"grid-sim", false},
		{"project:fin", true},
		{"project:bash", false}, // a tool name is not a project
		{"branch:timeline", true},
		{"model:opus", true},
		{"model:haiku", true}, // a session's other models count too
		{"model:gpt", false},
		{"tool:powershell", true},
		{"tool:git", false},
		{"id:9f2c", true},
		{"has:errors", true},
		{"has:unpriced", false},
		{"cost:>100", true},
		{"cost:>500", false},
		{"cost:<200", true},
		{"turns:>1000", false},
		{"fin main", false},           // every term must hold
		{"fin branch:timeline", true}, // and they can mix kinds
		{"project:fin has:errors", true},
	}
	for _, tc := range cases {
		if got := ParseFilter(tc.query).Match(s); got != tc.want {
			t.Errorf("%q matched %v, want %v", tc.query, got, tc.want)
		}
	}
}

// A query typed halfway must narrow, never error out from under the person
// typing it.
func TestFilterToleratesPartialInput(t *testing.T) {
	s := sample()
	for _, q := range []string{"project:", "cost:>", "cost:abc", ":", "has:", `"`} {
		f := ParseFilter(q)
		if f.Match(s) && q == "cost:abc" {
			t.Errorf("%q should not match: it is not a number and not in any column", q)
		}
		_ = f.Empty()
	}
}

// A project name with a space has to be searchable as one term.
func TestFilterQuotedPhrase(t *testing.T) {
	s := sample()
	s.Project = "my notes"
	if !ParseFilter(`project:"my notes"`).Match(s) {
		t.Error("quoted phrase did not match")
	}
	if ParseFilter("project:my project:notes").Match(s) == false {
		t.Error("both words are in the project name, so both terms hold")
	}
	if ParseFilter("my notes").Match(s) == false {
		t.Error("unquoted words each match the project name")
	}
}

func TestEmptyFilterAdmitsEverything(t *testing.T) {
	if !ParseFilter("   ").Empty() {
		t.Error("whitespace-only query should be empty")
	}
	if !ParseFilter("").Match(query.Session{}) {
		t.Error("an empty filter must admit a bare session")
	}
}

// Searching must cover the window, not just the first page. A match on the
// oldest day has to be found even though an unfiltered page would have stopped
// after the newest one.
func TestSessionsPageSearchesPastTheFirstPage(t *testing.T) {
	c, from, to := buildDays(t, 6, 4, 2)

	all, err := c.SessionsPage(from, to, 500, "", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Sessions) < 8 {
		t.Fatalf("expected a multi-day list to search through, got %d", len(all.Sessions))
	}
	// buildDays names sessions d<day>-s<n>; the oldest day is d0.
	oldest := "d00-s00"

	page, err := c.SessionsPage(from, to, 3, "", ParseFilter("id:"+oldest))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].ID != oldest {
		t.Fatalf("search returned %+v, want just %s", page.Sessions, oldest)
	}
	if page.DaysScanned < 2 {
		t.Errorf("days scanned = %d; a search for the oldest day must read past the newest", page.DaysScanned)
	}
}

// A filter narrows the same list rather than producing a different one.
func TestSessionsPageFilterIsASubset(t *testing.T) {
	c, from, to := buildDays(t, 4, 3, 2)

	all, err := c.SessionsPage(from, to, 500, "", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := c.SessionsPage(from, to, 500, "", ParseFilter("s00"))
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Sessions) == 0 || len(filtered.Sessions) >= len(all.Sessions) {
		t.Fatalf("filtered %d of %d sessions", len(filtered.Sessions), len(all.Sessions))
	}

	inAll := map[string]bool{}
	for _, s := range all.Sessions {
		inAll[s.ID] = true
	}
	for _, s := range filtered.Sessions {
		if !inAll[s.ID] {
			t.Errorf("search produced %s, which the unfiltered list does not contain", s.ID)
		}
	}
}

// A drill-in link keys on the project digest, which must select exactly one
// project even when two share a display name.
func TestFilterProjectIDIsExact(t *testing.T) {
	a := sample()
	a.Project, a.ProjectID = "maps", "0b12ab34cd56"
	b := sample()
	b.Project, b.ProjectID = "maps", "7ef8ab34cd56"

	f := ParseFilter("projectid:0b12ab34cd56")
	if !f.Match(a) {
		t.Error("did not match its own project")
	}
	if f.Match(b) {
		t.Error("matched a different project with the same name")
	}
	if ParseFilter("projectid:0b12").Match(a) {
		t.Error("a partial digest must not match; the term is generated, not typed")
	}
	// The display name still matches both, which is why the link cannot use it.
	if !ParseFilter("project:maps").Match(a) || !ParseFilter("project:maps").Match(b) {
		t.Error("the display name should match both projects")
	}
}
