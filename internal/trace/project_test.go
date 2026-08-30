package trace

import (
	"strings"
	"testing"
)

// The absolute working directory names the operator and every project beside
// it. Only the last segment may survive.
func TestProjectDropsThePath(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{`C:\Users\levir\dev\projects\apex-analysis`, "apex-analysis"},
		{"/home/someone/work/api", "api"},
		{"/home/someone/work/api/", "api"},
		{"", ""},
		{"/", ""},
		{`C:\`, ""},
	}
	for _, tc := range tests {
		got := Project(tc.dir)
		if got != tc.want {
			t.Errorf("Project(%q) = %q, want %q", tc.dir, got, tc.want)
		}
		if strings.Contains(got, "levir") || strings.Contains(got, "someone") {
			t.Errorf("Project(%q) leaked a username: %q", tc.dir, got)
		}
	}
}

// Two projects sharing a folder name must stay distinct, and the id must be
// stable across the separator and case differences the same machine produces.
func TestProjectIDDisambiguatesAndIsStable(t *testing.T) {
	a := ProjectID(`C:\work\acme\api`)
	b := ProjectID(`C:\work\other\api`)
	if a == b {
		t.Error("two different projects sharing a folder name collided")
	}

	if ProjectID(`C:\Users\levir\dev\proj`) != ProjectID("C:/Users/levir/dev/proj") {
		t.Error("separator style changed the id")
	}
	if ProjectID(`C:\Work\Proj`) != ProjectID(`c:\work\proj`) {
		t.Error("case changed the id")
	}
	if ProjectID("") != "" {
		t.Error("empty dir should yield no id")
	}
}

// The id must not disclose the path it came from.
func TestProjectIDIsNotReversible(t *testing.T) {
	dir := `C:\Users\levir\dev\projects\secret-client-work`
	id := ProjectID(dir)

	for _, leak := range []string{"levir", "secret", "client", "Users", "dev", `\`, "/"} {
		if strings.Contains(strings.ToLower(id), strings.ToLower(leak)) {
			t.Errorf("ProjectID leaked %q: %s", leak, id)
		}
	}
	if len(id) != projectIDLength {
		t.Errorf("id length = %d, want %d", len(id), projectIDLength)
	}
}
