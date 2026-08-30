package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Urenzu/trace-portal/internal/trace"
)

func writeLines(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func collect(t *testing.T, parse func(Emit) (int64, error)) ([]trace.Event, int64) {
	t.Helper()
	var got []trace.Event
	off, err := parse(func(ev trace.Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return got, off
}

const ccAssistant = `{"type":"assistant","timestamp":"2026-08-29T02:46:44.498Z","sessionId":"sess-1",` +
	`"requestId":"req_1","uuid":"u1","cwd":"C:\\work\\proj","gitBranch":"main","effort":"high",` +
	`"isSidechain":false,"message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"tool_use",` +
	`"content":[{"type":"thinking"},{"type":"tool_use","id":"tu_1","name":"Bash"}],` +
	`"usage":{"input_tokens":12,"output_tokens":219,"cache_creation_input_tokens":30173,` +
	`"cache_read_input_tokens":15000,"cache_creation":{"ephemeral_5m_input_tokens":173,` +
	`"ephemeral_1h_input_tokens":30000},"output_tokens_details":{"thinking_tokens":88},` +
	`"service_tier":"standard","speed":"standard"}}}`

func TestClaudeCodeParsesAssistantTurn(t *testing.T) {
	dir := t.TempDir()
	path := writeLines(t, dir, "proj/sess.jsonl", ccAssistant)

	src := NewClaudeCode(dir)
	got, off := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Source != ClaudeCodeName || ev.Type != trace.EventResponse {
		t.Errorf("source/type: %+v", ev)
	}
	if ev.MessageID != "msg_1" || ev.RequestID != "req_1" || ev.SessionID != "sess-1" {
		t.Errorf("identity: %+v", ev)
	}
	// The turn is keyed by the API message id so another source observing the
	// same call lines up with it.
	if ev.TurnID != "msg_1" {
		t.Errorf("TurnID = %q, want the message id", ev.TurnID)
	}
	if ev.Model != "claude-opus-5" || ev.StopReason != "tool_use" {
		t.Errorf("model/stop: %+v", ev)
	}
	// The absolute path is reduced to a readable name plus a one-way id; the
	// path itself, which names the operator and every sibling project, is gone.
	if ev.Project != "proj" {
		t.Errorf("Project = %q, want %q", ev.Project, "proj")
	}
	if ev.ProjectID == "" {
		t.Error("ProjectID not derived")
	}
	if ev.GitBranch != "main" || ev.Effort != "high" {
		t.Errorf("branch=%q effort=%q", ev.GitBranch, ev.Effort)
	}
	if ev.Usage == nil {
		t.Fatal("no usage")
	}
	// The cache TTL split is what makes the cost right; it must survive.
	w5, w1h := ev.Usage.CacheWrites()
	if w5 != 173 || w1h != 30000 {
		t.Errorf("cache writes = (%d, %d), want (173, 30000)", w5, w1h)
	}
	if ev.Usage.CacheReadInputTokens != 15000 || ev.Usage.OutputTokens != 219 {
		t.Errorf("usage = %+v", ev.Usage)
	}
	if ev.Usage.ReasoningTokens == nil || *ev.Usage.ReasoningTokens != 88 {
		t.Errorf("ReasoningTokens = %v, want 88", ev.Usage.ReasoningTokens)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "Bash" {
		t.Errorf("tool calls = %+v", ev.ToolCalls)
	}
	if off <= 0 {
		t.Errorf("offset = %d, want the consumed length", off)
	}
}

// Locally generated messages never hit the API. Counting them would inflate
// the turn count and show as unpriced traffic.
func TestClaudeCodeSkipsSyntheticMessages(t *testing.T) {
	dir := t.TempDir()
	synthetic := `{"type":"assistant","timestamp":"2026-08-29T02:46:44Z","sessionId":"s",` +
		`"uuid":"u2","message":{"id":"msg_2","model":"<synthetic>","usage":` +
		`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	path := writeLines(t, dir, "p/s.jsonl", ccAssistant, synthetic)

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (synthetic must be skipped)", len(got))
	}
}

// User, system and other record types are not billed API calls.
func TestClaudeCodeIgnoresNonAssistantRecords(t *testing.T) {
	dir := t.TempDir()
	path := writeLines(t, dir, "p/s.jsonl",
		`{"type":"user","timestamp":"2026-08-29T02:00:00Z","sessionId":"s"}`,
		`{"type":"system","timestamp":"2026-08-29T02:00:01Z"}`,
		`{"type":"file-history-snapshot"}`,
		ccAssistant,
	)
	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
}

// A transcript is appended to while it is read, so the final line is often
// incomplete. It must not be consumed, or the record is lost when it finishes.
func TestClaudeCodeLeavesPartialLineForNextPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p", "s.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o755)

	partial := ccAssistant[:len(ccAssistant)/2]
	if err := os.WriteFile(path, []byte(ccAssistant+"\n"+partial), 0o644); err != nil {
		t.Fatal(err)
	}

	src := NewClaudeCode(dir)
	got, off := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if off != int64(len(ccAssistant)+1) {
		t.Errorf("offset = %d, want %d (partial line not consumed)", off, len(ccAssistant)+1)
	}

	// Completing the record makes it appear, exactly once.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(ccAssistant[len(ccAssistant)/2:] + "\n")
	f.Close()

	more, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, off, e) })
	if len(more) != 1 {
		t.Fatalf("resumed pass got %d events, want 1", len(more))
	}
}

// Resuming from a recorded offset must not replay records already ingested.
func TestClaudeCodeResumesWithoutDuplicating(t *testing.T) {
	dir := t.TempDir()
	path := writeLines(t, dir, "p/s.jsonl", ccAssistant)
	src := NewClaudeCode(dir)

	first, off := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })
	second, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, off, e) })

	if len(first) != 1 || len(second) != 0 {
		t.Errorf("first=%d second=%d, want 1 then 0", len(first), len(second))
	}
}

func TestClaudeCodeMissingDirectoryIsNotAnError(t *testing.T) {
	src := NewClaudeCode(filepath.Join(t.TempDir(), "does-not-exist"))
	files, err := src.Files()
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files", len(files))
	}
}

// A build that predates output_tokens_details reports nothing about thinking.
// That must stay distinguishable from a turn that thought for zero tokens, or
// every user on an older build silently reads as "never thinks".
func TestMissingFieldIsNotZero(t *testing.T) {
	dir := t.TempDir()
	older := `{"type":"assistant","timestamp":"2026-08-10T02:46:44Z","sessionId":"s","uuid":"u",` +
		`"version":"2.1.221","message":{"id":"msg_old","model":"claude-opus-5","stop_reason":"end_turn",` +
		`"content":[],"usage":{"input_tokens":10,"output_tokens":500,` +
		`"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	path := writeLines(t, dir, "p/s.jsonl", older)

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Usage.ReasoningTokens != nil {
		t.Errorf("absent thinking tokens read as a measured value: %v", *got[0].Usage.ReasoningTokens)
	}
	if got[0].ProducerVersion != "2.1.221" {
		t.Errorf("ProducerVersion = %q", got[0].ProducerVersion)
	}

	cov := src.Coverage()
	if cov.MissingField["thinking_tokens"] != 1 {
		t.Errorf("missing field not counted: %v", cov.MissingField)
	}
	if cov.ByVersion["2.1.221"] != 1 {
		t.Errorf("version not counted: %v", cov.ByVersion)
	}
}

// A key this build does not read is how a format change first shows up. It has
// to be counted, or new data goes on the floor unnoticed.
func TestUnknownFieldsAreCounted(t *testing.T) {
	dir := t.TempDir()
	future := `{"type":"assistant","timestamp":"2026-09-01T02:46:44Z","sessionId":"s","uuid":"u",` +
		`"version":"2.9.999","brandNewField":{"a":1},"anotherNewThing":42,` +
		`"message":{"id":"msg_new","model":"claude-opus-5","stop_reason":"end_turn","content":[],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"output_tokens_details":{"thinking_tokens":3}}}}`
	path := writeLines(t, dir, "p/s.jsonl", future)

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	// The turn still parses: an unknown field degrades knowledge, not ingestion.
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 — an unknown field must not drop the turn", len(got))
	}
	cov := src.Coverage()
	if cov.UnknownField["brandNewField"] != 1 || cov.UnknownField["anotherNewThing"] != 1 {
		t.Errorf("unknown fields not reported: %v", cov.UnknownField)
	}
	// Known keys must not be reported as unknown, or the signal is worthless.
	for _, known := range []string{"type", "message", "version", "sessionId"} {
		if cov.UnknownField[known] != 0 {
			t.Errorf("known key %q reported as unknown", known)
		}
	}
}

func TestCoverageCountsOutcomes(t *testing.T) {
	dir := t.TempDir()
	path := writeLines(t, dir, "p/s.jsonl",
		ccAssistant,
		`{"type":"user","timestamp":"2026-08-29T02:00:00Z"}`,
		`not json at all`,
	)

	src := NewClaudeCode(dir)
	collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	cov := src.Coverage()
	if cov.Records != 3 {
		t.Errorf("Records = %d, want 3", cov.Records)
	}
	if cov.Parsed != 1 {
		t.Errorf("Parsed = %d, want 1", cov.Parsed)
	}
	if cov.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", cov.Unreadable)
	}
}

// message.content is an array on assistant records but a bare string on some
// user records. Typing it concretely made every one of those fail to
// unmarshal, taking the whole record with it — and a shape change on an
// assistant record would have silently dropped a billed turn.
func TestVaryingContentShapeDoesNotFailTheRecord(t *testing.T) {
	dir := t.TempDir()
	stringContent := `{"type":"user","timestamp":"2026-08-29T02:00:00Z","sessionId":"s","uuid":"u0",` +
		`"message":{"role":"user","content":"just a string"}}`
	path := writeLines(t, dir, "p/s.jsonl", stringContent, ccAssistant)

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if src.Coverage().Unreadable != 0 {
		t.Errorf("a legitimate shape variation was counted as unreadable: %d", src.Coverage().Unreadable)
	}
}

// A rate-limited call is an assistant record with an error and no usage.
// Dropping it reports a throttled run as entirely healthy.
func TestAPIErrorsBecomeErrorEvents(t *testing.T) {
	dir := t.TempDir()
	rateLimited := `{"type":"assistant","timestamp":"2026-08-21T17:50:36.561Z","sessionId":"s",` +
		`"uuid":"u1","requestId":"req_x","version":"2.1.228","cwd":"/w/proj",` +
		`"isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":"429",` +
		`"message":{"model":"claude-opus-5","content":[]}}`
	path := writeLines(t, dir, "p/s.jsonl", rateLimited, ccAssistant)

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (one error, one turn)", len(got))
	}
	var errEvent *trace.Event
	for i := range got {
		if got[i].Type == trace.EventError {
			errEvent = &got[i]
		}
	}
	if errEvent == nil {
		t.Fatal("the rate-limited call produced no error event")
	}
	if errEvent.Error != "rate_limit" {
		t.Errorf("Error = %q", errEvent.Error)
	}
	if errEvent.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", errEvent.StatusCode)
	}
	if errEvent.Project != "proj" {
		t.Errorf("Project = %q", errEvent.Project)
	}
	if errEvent.ProducerVersion != "2.1.228" {
		t.Errorf("ProducerVersion = %q", errEvent.ProducerVersion)
	}
}

// Claude Code writes one transcript record per content block of an assistant
// message, and every one of those records repeats the whole message's usage.
// A message with thinking, text and a tool call therefore appears three times
// carrying identical token counts — a 1.9x inflation on real data.
//
// Each record must keep the message id, because that is what collapses them
// downstream. Summing the records instead would roughly double every total.
func TestRepeatedContentBlockRecordsShareAMessageID(t *testing.T) {
	dir := t.TempDir()
	// The same message, as Claude Code writes it: one record per block.
	record := func(blockType string) string {
		return `{"type":"assistant","timestamp":"2026-08-21T10:00:00Z","sessionId":"s","uuid":"u-` +
			blockType + `","requestId":"req_1","version":"2.1.238","message":{"id":"msg_same",` +
			`"model":"claude-opus-5","stop_reason":"tool_use","content":[{"type":"` + blockType + `"}],` +
			`"usage":{"input_tokens":2,"output_tokens":277,"cache_creation_input_tokens":1968,` +
			`"cache_read_input_tokens":30173}}}`
	}
	path := writeLines(t, dir, "p/s.jsonl", record("thinking"), record("text"), record("tool_use"))

	src := NewClaudeCode(dir)
	got, _ := collect(t, func(e Emit) (int64, error) { return src.Parse(path, 0, e) })

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 — one per record", len(got))
	}
	for i, ev := range got {
		if ev.MessageID != "msg_same" {
			t.Fatalf("event %d has message id %q; without it duplicates cannot be collapsed", i, ev.MessageID)
		}
		if ev.TurnID != "msg_same" {
			t.Errorf("event %d TurnID = %q, want the message id", i, ev.TurnID)
		}
	}
}

// The last path segment is not the project. Agents are routinely run from a
// subdirectory, and taking the basename splits one repository's cost across
// several rows while colliding unrelated directories that share a name.
func TestProjectResolvesToRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "fin-agentic")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(repo, "apps", "web")
	api := filepath.Join(repo, "services", "api")
	os.MkdirAll(web, 0o755)
	os.MkdirAll(api, 0o755)

	r := newProjectResolver()

	repoName, repoID, inRepo := r.Resolve(repo)
	webName, webID, _ := r.Resolve(web)
	apiName, apiID, _ := r.Resolve(api)

	if !inRepo {
		t.Error("a repository root was not recognised as one")
	}
	for _, got := range []string{repoName, webName, apiName} {
		if got != "fin-agentic" {
			t.Errorf("resolved to %q, want fin-agentic", got)
		}
	}
	// All three must share one id, or the cost splits across rows.
	if webID != repoID || apiID != repoID {
		t.Errorf("ids differ: repo=%s web=%s api=%s", repoID, webID, apiID)
	}
}

// Two unrelated directories that happen to share a folder name must not merge.
func TestProjectDistinguishesSameNamedDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		os.MkdirAll(filepath.Join(root, name, ".git"), 0o755)
		os.MkdirAll(filepath.Join(root, name, "src", "maps"), 0o755)
	}

	r := newProjectResolver()
	_, aID, _ := r.Resolve(filepath.Join(root, "alpha", "src", "maps"))
	_, bID, _ := r.Resolve(filepath.Join(root, "beta", "src", "maps"))

	if aID == bID {
		t.Error("directories named maps in different repositories collided")
	}
}

// A directory outside any repository keeps its own name: it is not a project,
// and inventing one would be worse than reporting what was actually there.
func TestProjectWithoutRepositoryKeepsItsName(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "Downloads", "config")
	os.MkdirAll(plain, 0o755)

	r := newProjectResolver()
	name, id, inRepo := r.Resolve(plain)
	if name != "config" {
		t.Errorf("name = %q, want config", name)
	}
	if id == "" {
		t.Error("no id derived")
	}
	// It is a directory, not a project; the distinction is what lets the UI
	// avoid presenting it as one.
	if inRepo {
		t.Error("a directory outside any repository was reported as one")
	}
}

// A subdirectory that no longer exists still belongs to its repository, as long
// as some sibling path revealed where the root was.
func TestDeletedSubdirectoryStillAttributedToItsRepository(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "apex-analysis")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)

	r := newProjectResolver()
	// Seen while it existed.
	wantName, wantID, _ := r.Resolve(repo)

	// A path beneath it that was deleted before ingestion ran.
	goneName, goneID, _ := r.Resolve(filepath.Join(repo, "public", "maps"))

	if goneName != wantName || goneID != wantID {
		t.Errorf("deleted subdirectory resolved to %s/%s, want %s/%s",
			goneName, goneID, wantName, wantID)
	}
}

func TestProjectResolutionIsCached(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "proj")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)

	r := newProjectResolver()
	n1, i1, _ := r.Resolve(repo)

	// Removing the marker must not change an already-resolved answer.
	os.RemoveAll(filepath.Join(repo, ".git"))
	n2, i2, _ := r.Resolve(repo)

	if n1 != n2 || i1 != i2 {
		t.Errorf("cached resolution changed: %s/%s then %s/%s", n1, i1, n2, i2)
	}
}

// A deleted directory cannot be checked for a repository, so it keeps its own
// name. Two of them may then share a name — they must still be distinct
// entities, because merging unrelated work is worse than a repeated label.
func TestUnverifiableDirectoriesStayDistinct(t *testing.T) {
	root := t.TempDir()
	r := newProjectResolver()

	aName, aID, _ := r.Resolve(filepath.Join(root, "apex-analysis", "public", "maps"))
	bName, bID, _ := r.Resolve(filepath.Join(root, "apex-analysis", "src", "data", "maps"))

	if aName != "maps" || bName != "maps" {
		t.Errorf("names = %q, %q; want the directory name unchanged", aName, bName)
	}
	if aID == bID {
		t.Error("distinct directories shared an id, so their costs would merge")
	}
	for _, n := range []string{aName, bName} {
		if strings.Contains(n, root) || strings.Contains(n, ":") {
			t.Errorf("name leaked a path: %q", n)
		}
	}
}
