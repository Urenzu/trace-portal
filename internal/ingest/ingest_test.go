package ingest

import (
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Urenzu/trace-portal/internal/compact"
	"github.com/Urenzu/trace-portal/internal/query"
	"github.com/Urenzu/trace-portal/internal/source"
	"github.com/Urenzu/trace-portal/internal/store"
	"github.com/Urenzu/trace-portal/internal/trace"
)

func assistantLine(msgID string) string {
	return `{"type":"assistant","timestamp":"` + time.Now().UTC().Format(time.RFC3339) +
		`","sessionId":"s1","uuid":"u-` + msgID + `","message":{"id":"` + msgID +
		`","model":"claude-opus-5","stop_reason":"end_turn","content":[],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":100}}}` + "\n"
}

func newIngester(t *testing.T) (*Ingester, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	os.MkdirAll(filepath.Join(logDir, "proj"), 0o755)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ing := New(st, dir, quiet, source.NewClaudeCode(logDir))
	return ing, st, filepath.Join(logDir, "proj", "session.jsonl")
}

func TestIngestBackfillsThenFollows(t *testing.T) {
	ing, st, path := newIngester(t)

	if err := os.WriteFile(path, []byte(assistantLine("msg_1")+assistantLine("msg_2")), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ing.Pass()
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if res.Events != 2 {
		t.Fatalf("first pass ingested %d, want 2", res.Events)
	}

	// A pass with nothing new must ingest nothing, or every poll would
	// duplicate the whole history.
	res, err = ing.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 0 {
		t.Errorf("idle pass ingested %d events, want 0", res.Events)
	}

	// Appending a turn, as an agent does while it works, is picked up.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(assistantLine("msg_3"))
	f.Close()

	res, err = ing.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Errorf("follow-up pass ingested %d, want 1", res.Events)
	}

	events, err := st.Events(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("store holds %d events, want 3", len(events))
	}
	seen := map[string]bool{}
	for _, ev := range events {
		if seen[ev.MessageID] {
			t.Errorf("message %s ingested twice", ev.MessageID)
		}
		seen[ev.MessageID] = true
	}
}

// A restart must resume where it stopped rather than replaying months of
// transcripts into the store.
func TestIngestResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "proj")
	os.MkdirAll(logDir, 0o755)
	path := filepath.Join(logDir, "s.jsonl")
	os.WriteFile(path, []byte(assistantLine("msg_1")), 0o644)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := source.NewClaudeCode(filepath.Join(dir, "logs"))

	st1, _ := store.Open(dir)
	ing1 := New(st1, dir, quiet, src)
	if res, _ := ing1.Pass(); res.Events != 1 {
		t.Fatalf("first run ingested %d, want 1", res.Events)
	}
	st1.Close()

	// A fresh process, same data directory.
	st2, _ := store.Open(dir)
	defer st2.Close()
	ing2 := New(st2, dir, quiet, src)
	if err := ing2.loadCheckpoint(); err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	res, err := ing2.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 0 {
		t.Errorf("restart re-ingested %d events, want 0", res.Events)
	}
}

// A file replaced by a shorter one was rotated; reading from the old offset
// would return garbage, so the scan restarts.
func TestIngestHandlesTruncatedFile(t *testing.T) {
	ing, _, path := newIngester(t)

	os.WriteFile(path, []byte(assistantLine("msg_1")+assistantLine("msg_2")), 0o644)
	if res, _ := ing.Pass(); res.Events != 2 {
		t.Fatal("setup pass did not ingest 2")
	}

	os.WriteFile(path, []byte(assistantLine("msg_9")), 0o644)
	res, err := ing.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Errorf("after truncation ingested %d, want 1", res.Events)
	}
}

func TestIngestMissingSourceDirectoryIsHarmless(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(dir)
	defer st.Close()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ing := New(st, dir, quiet, source.NewClaudeCode(filepath.Join(dir, "nope")))

	res, err := ing.Pass()
	if err != nil {
		t.Errorf("missing source dir errored: %v", err)
	}
	if res.Events != 0 {
		t.Errorf("ingested %d events from nothing", res.Events)
	}
}

// Claude Code prunes transcripts after roughly a month. Everything ingested
// must outlive them, or multi-month analysis is impossible by construction.
func TestArchiveSurvivesTranscriptDeletion(t *testing.T) {
	ing, st, path := newIngester(t)
	os.WriteFile(path, []byte(assistantLine("msg_1")+assistantLine("msg_2")), 0o644)

	if res, _ := ing.Pass(); res.Events != 2 {
		t.Fatal("setup did not ingest 2 events")
	}

	// The agent tool deletes its own log.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	events, err := st.Events(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("archive holds %d events after the transcript was deleted, want 2", len(events))
	}
	turns := query.BuildTurns(events)
	if len(turns) != 2 {
		t.Errorf("got %d turns, want 2", len(turns))
	}
	for _, turn := range turns {
		if turn.CostUSD <= 0 {
			t.Errorf("turn %s lost its cost", turn.TurnID)
		}
	}

	// A later pass must not erase anything just because the file is gone.
	if res, err := ing.Pass(); err != nil || res.Events != 0 {
		t.Errorf("pass over a deleted file: events=%d err=%v", res.Events, err)
	}
	if after, _ := st.Events(time.Now().UTC()); len(after) != 2 {
		t.Errorf("archive changed after the source vanished: %d events", len(after))
	}
}

// If the checkpoint is lost the whole history is re-read. Raw events may then
// appear twice, but analysis must not double-count: turns are keyed by the
// API's message id, so duplicates collapse.
func TestLostCheckpointDoesNotDoubleCount(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "proj")
	os.MkdirAll(logDir, 0o755)
	path := filepath.Join(logDir, "s.jsonl")
	os.WriteFile(path, []byte(assistantLine("msg_1")+assistantLine("msg_2")+assistantLine("msg_3")), 0o644)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := source.NewClaudeCode(filepath.Join(dir, "logs"))

	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ing := New(st, dir, quiet, src)
	if res, _ := ing.Pass(); res.Events != 3 {
		t.Fatal("first pass did not ingest 3")
	}

	before, _ := st.Events(time.Now().UTC())
	beforeTurns := query.BuildTurns(before)
	beforeCost := 0.0
	for _, turn := range beforeTurns {
		beforeCost += turn.CostUSD
	}

	// Simulate a lost checkpoint: a fresh ingester with no memory of offsets.
	if err := os.Remove(filepath.Join(dir, checkpointFile)); err != nil {
		t.Fatal(err)
	}
	replay := New(st, dir, quiet, src)
	if res, _ := replay.Pass(); res.Events != 3 {
		t.Fatalf("replay ingested %d raw events, want 3", res.Events)
	}

	after, _ := st.Events(time.Now().UTC())
	if len(after) <= len(before) {
		t.Fatalf("setup wrong: replay should have appended raw events (%d -> %d)", len(before), len(after))
	}

	afterTurns := query.BuildTurns(after)
	if len(afterTurns) != len(beforeTurns) {
		t.Errorf("turns after replay = %d, want %d — duplicates were counted", len(afterTurns), len(beforeTurns))
	}
	afterCost := 0.0
	for _, turn := range afterTurns {
		afterCost += turn.CostUSD
	}
	if math.Abs(afterCost-beforeCost) > 1e-12 {
		t.Errorf("cost doubled on replay: before=%.10f after=%.10f", beforeCost, afterCost)
	}
}

// Compaction is the durable form, so it must resolve duplicates permanently
// rather than carrying them into the archive.
func TestCompactionCollapsesDuplicates(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Two identical observations of one call, as a lost checkpoint or a second
	// source would produce, on a completed day so compaction will touch it.
	day := time.Now().UTC().AddDate(0, 0, -1).Truncate(time.Hour)
	ev := trace.Event{
		Type: trace.EventResponse, Timestamp: day, SessionID: "s1",
		TurnID: "msg_dup", MessageID: "msg_dup", Source: "claude-code",
		Model: "claude-opus-5", StatusCode: 200, StopReason: "end_turn",
		Usage: &trace.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 900},
	}
	for i := 0; i < 2; i++ {
		if err := st.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	c, err := compact.New(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompactDay(day, false); err != nil {
		t.Fatal(err)
	}

	turns, ok, err := c.ReadTurns(day)
	if err != nil || !ok {
		t.Fatalf("read compacted turns: ok=%v err=%v", ok, err)
	}
	if len(turns) != 1 {
		t.Fatalf("compacted %d turns, want 1 — duplicates entered the archive", len(turns))
	}
	if turns[0].Usage.InputTokens != 100 {
		t.Errorf("usage summed instead of deduped: %+v", turns[0].Usage)
	}
}

// Coverage describes the archive, not one run. A restart that finds nothing new
// to parse must still be able to report what the stored data was missing —
// otherwise the panel explaining the gaps disappears the moment it matters.
func TestCoverageSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "proj")
	os.MkdirAll(logDir, 0o755)

	// A turn from a build that predates thinking tokens.
	older := `{"type":"assistant","timestamp":"` + time.Now().UTC().Format(time.RFC3339) +
		`","sessionId":"s","uuid":"u","version":"2.1.221","message":{"id":"msg_old",` +
		`"model":"claude-opus-5","stop_reason":"end_turn","content":[],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":100}}}` + "\n"
	os.WriteFile(filepath.Join(logDir, "s.jsonl"), []byte(older), 0o644)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first := New(st, dir, quiet, source.NewClaudeCode(filepath.Join(dir, "logs")))
	if res, _ := first.Pass(); res.Events != 1 {
		t.Fatalf("first pass ingested %d, want 1", res.Events)
	}
	before := first.Coverage()["claude-code"]
	if before == nil || before.MissingField["thinking_tokens"] != 1 {
		t.Fatalf("setup: coverage not recorded: %+v", before)
	}

	// A fresh process over the same directory, with nothing new to read.
	second := New(st, dir, quiet, source.NewClaudeCode(filepath.Join(dir, "logs")))
	if err := second.loadCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if res, _ := second.Pass(); res.Events != 0 {
		t.Fatalf("restart re-ingested %d events", res.Events)
	}

	after := second.Coverage()["claude-code"]
	if after == nil {
		t.Fatal("coverage vanished after restart")
	}
	if after.MissingField["thinking_tokens"] != 1 {
		t.Errorf("missing-field count lost across restart: %+v", after.MissingField)
	}
	if after.ByVersion["2.1.221"] != 1 {
		t.Errorf("version counts lost across restart: %+v", after.ByVersion)
	}
	if after.Parsed != 1 {
		t.Errorf("Parsed = %d, want 1", after.Parsed)
	}
}

// Folding a run's counts into the persisted total must not count them twice.
func TestCoverageIsNotDoubleCountedAcrossSaves(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs", "proj")
	os.MkdirAll(logDir, 0o755)
	os.WriteFile(filepath.Join(logDir, "s.jsonl"), []byte(assistantLine("msg_1")), 0o644)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := store.Open(dir)
	defer st.Close()

	ing := New(st, dir, quiet, source.NewClaudeCode(filepath.Join(dir, "logs")))
	ing.Pass()

	// Several saves with no new records in between.
	for i := 0; i < 3; i++ {
		if err := ing.saveCheckpoint(); err != nil {
			t.Fatal(err)
		}
	}
	if got := ing.Coverage()["claude-code"].Parsed; got != 1 {
		t.Errorf("Parsed = %d after repeated saves, want 1", got)
	}
}
