package source

import (
	"encoding/json"
	"testing"
)

// A Coverage that came back from JSON has nil maps wherever a key was absent,
// and the tags are omitempty — so a report persisted while a counter happened
// to be empty writes no key at all. Writing into one of those maps panics.
//
// This was not hypothetical. A real checkpoint on a machine in daily use held
// {"records":22585,"parsed":7457,"skipped":15128,"unreadable":4} and nothing
// else, because the counts were persisted before any producing version had been
// recorded. On the next start, the first parsed record killed the ingest
// goroutine. The panic is recovered and logged, so the process stayed up and
// the UI kept serving — the archive simply stopped growing, which for a tool
// whose source is pruned after a month is unrecoverable data loss.
func TestCoverageDecodedFromJSONIsWritable(t *testing.T) {
	var c Coverage
	if err := json.Unmarshal([]byte(`{"records":10,"parsed":5,"skipped":5}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.ByVersion != nil || c.MissingField != nil || c.UnknownField != nil {
		t.Fatal("this test is meaningless unless the decoded maps are nil")
	}

	// Each of these used to panic.
	c.record("2.1.252")
	c.missing("thinking_tokens")
	c.noteUnknown([]byte(`{"sessionKind":"x"}`), map[string]bool{})
	c.seen()
	c.skip(true)

	if c.ByVersion["2.1.252"] != 1 {
		t.Errorf("version not counted: %v", c.ByVersion)
	}
	if c.MissingField["thinking_tokens"] != 1 {
		t.Errorf("missing field not counted: %v", c.MissingField)
	}
	if c.UnknownField["sessionKind"] != 1 {
		t.Errorf("unknown field not counted: %v", c.UnknownField)
	}
}

// The same nil maps reached Merge, which is the path the ingest checkpoint
// actually took: load the persisted report, fold this run's counts into it.
func TestMergeIntoCoverageDecodedFromJSON(t *testing.T) {
	var persisted Coverage
	if err := json.Unmarshal([]byte(`{"records":22585,"parsed":7457,"skipped":15128,"unreadable":4}`), &persisted); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	live := NewCoverage()
	live.record("2.1.252")
	live.missing("thinking_tokens")

	persisted.Merge(live)

	if persisted.Parsed != 7458 {
		t.Errorf("parsed = %d, want 7458", persisted.Parsed)
	}
	if persisted.ByVersion["2.1.252"] != 1 {
		t.Errorf("merge lost the version counts: %v", persisted.ByVersion)
	}
	if persisted.MissingField["thinking_tokens"] != 1 {
		t.Errorf("merge lost the missing-field counts: %v", persisted.MissingField)
	}
}
