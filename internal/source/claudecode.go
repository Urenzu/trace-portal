package source

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// ClaudeCodeName is the source label written onto every event it produces.
const ClaudeCodeName = "claude-code"

// syntheticModel marks a transcript message Claude Code generated locally
// rather than receiving from the API.
const syntheticModel = "<synthetic>"

// ccKnownKeys is every top-level transcript key this build understands or has
// deliberately decided to ignore. A key outside this set is the format moving,
// and gets counted so the drift is visible rather than silent.
var ccKnownKeys = map[string]bool{
	"type": true, "timestamp": true, "sessionId": true, "session_id": true,
	"requestId": true, "uuid": true, "parentUuid": true, "leafUuid": true,
	"cwd": true, "gitBranch": true, "version": true, "effort": true,
	"isSidechain": true, "isAbortedMidStream": true, "isMeta": true,
	"userType": true, "entrypoint": true, "message": true, "slug": true,
	"toolUseResult": true, "sourceToolAssistantUUID": true, "attachment": true,
	"permissionMode": true, "mode": true, "aiTitle": true, "lastPrompt": true,
	"promptId": true, "messageId": true, "subtype": true, "content": true,
	"level": true, "attributionSkill": true, "attributionMcpServer": true,
	"attributionMcpTool": true, "summary": true, "durationMs": true,
	"isApiErrorMessage": true, "error": true, "apiErrorStatus": true,
}

// ClaudeCode reads Claude Code session transcripts from ~/.claude/projects,
// where each project directory holds one JSONL file per session, appended to as
// the session runs.
type ClaudeCode struct {
	root     string
	coverage *Coverage
	projects *projectResolver
}

// NewClaudeCode reads from root, defaulting to ~/.claude/projects.
func NewClaudeCode(root string) *ClaudeCode {
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".claude", "projects")
		}
	}
	return &ClaudeCode{root: root, coverage: NewCoverage(), projects: newProjectResolver()}
}

// Coverage reports what parsing understood, and what it did not.
func (c *ClaudeCode) Coverage() *Coverage { return c.coverage }

func (c *ClaudeCode) Name() string { return ClaudeCodeName }
func (c *ClaudeCode) Root() string { return c.root }

// Files lists every session transcript, oldest first so a backfill replays in
// roughly the order the work happened.
func (c *ClaudeCode) Files() ([]string, error) {
	return jsonlFiles(c.root)
}

// ccRecord is the subset of a transcript line we read. Transcripts are an
// internal format that changes between releases, so every field is optional:
// an unfamiliar record degrades a turn rather than failing the file.
type ccRecord struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	RequestID   string `json:"requestId"`
	UUID        string `json:"uuid"`
	CWD         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Effort      string `json:"effort"`
	Version     string `json:"version"`
	IsSidechain bool   `json:"isSidechain"`
	Aborted     bool   `json:"isAbortedMidStream"`

	Message *struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		// Content is decoded lazily. It is an array of blocks on assistant
		// records but a bare string on some user records, and typing it
		// concretely made every one of those fail to unmarshal — taking the
		// whole record with it.
		Content json.RawMessage `json:"content"`
		Usage   *ccUsage        `json:"usage"`
	} `json:"message"`

	// Transcripts record API failures as assistant records carrying no usage.
	// Without these a rate-limited run reports zero errors.
	IsAPIError     bool   `json:"isApiErrorMessage"`
	APIError       string `json:"error"`
	APIErrorStatus string `json:"apiErrorStatus"`
}

// ccContentBlock is one entry of an assistant message's content array.
type ccContentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ccUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`

	CacheCreation *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`

	OutputTokensDetails *struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`

	ServiceTier string `json:"service_tier"`
	Speed       string `json:"speed"`
}

// Parse reads transcript records from an offset, emitting one response event
// per assistant message that carries usage. Assistant messages are the only
// records that correspond to a billed API call.
func (c *ClaudeCode) Parse(path string, from int64, emit Emit) (int64, error) {
	return scanJSONL(path, from, func(line []byte) error {
		c.coverage.seen()

		var rec ccRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// Not corruption: the record is valid JSON whose shape this build
			// does not model. Counting it separately keeps a real parse failure
			// from hiding among ordinary format drift.
			c.coverage.skip(true)
			return nil
		}
		if rec.Type != "assistant" {
			c.coverage.skip(false)
			return nil
		}
		c.coverage.noteUnknown(line, ccKnownKeys)

		// A failed call is an assistant record with an error and no usage.
		// These are real turns that cost latency and hit quota, so losing them
		// would report a rate-limited run as entirely healthy.
		if rec.IsAPIError || rec.APIError != "" {
			c.coverage.record(rec.Version)
			return emit(c.errorEvent(rec))
		}
		if rec.Message == nil || rec.Message.Usage == nil {
			c.coverage.skip(false)
			return nil
		}
		// Claude Code writes locally generated messages with the model
		// "<synthetic>" and all-zero usage. They never hit the API, so counting
		// them would inflate the turn count and show up as unpriced traffic.
		if rec.Message.Model == syntheticModel {
			c.coverage.skip(false)
			return nil
		}

		// A working directory is resolved to the repository that contains it,
		// so a run started in a subdirectory is not counted as its own project.
		projectName, projectID, inRepo := c.projects.Resolve(rec.CWD)

		ev := trace.Event{
			Type:            trace.EventResponse,
			Source:          ClaudeCodeName,
			Timestamp:       parseTimestamp(rec.Timestamp),
			SessionID:       rec.SessionID,
			MessageID:       rec.Message.ID,
			RequestID:       rec.RequestID,
			Model:           rec.Message.Model,
			StopReason:      rec.Message.StopReason,
			Project:         projectName,
			ProjectID:       projectID,
			InRepo:          inRepo,
			GitBranch:       rec.GitBranch,
			Effort:          rec.Effort,
			ProducerVersion: rec.Version,
			Subagent:        rec.IsSidechain,
			StatusCode:      200,
			ServiceTier:     rec.Message.Usage.ServiceTier,
			Speed:           rec.Message.Usage.Speed,
		}

		// The turn is keyed by the API's message id so the same call seen by
		// another source lines up. Records without one fall back to the
		// transcript's own uuid.
		ev.TurnID = rec.Message.ID
		if ev.TurnID == "" {
			ev.TurnID = rec.UUID
		}
		if ev.SessionID == "" {
			ev.SessionID = "unknown"
		}
		if rec.Aborted {
			ev.Error = "aborted mid-stream"
		}

		u := rec.Message.Usage
		usage := trace.Usage{
			InputTokens:              u.InputTokens,
			OutputTokens:             u.OutputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
		}
		if u.CacheCreation != nil {
			usage.CacheCreation = &trace.CacheCreation{
				Ephemeral5mInputTokens: u.CacheCreation.Ephemeral5m,
				Ephemeral1hInputTokens: u.CacheCreation.Ephemeral1h,
			}
		}
		// Absent details mean this build never reported thinking tokens, which
		// is different from a turn that did not think.
		if u.OutputTokensDetails != nil {
			thinking := u.OutputTokensDetails.ThinkingTokens
			usage.ReasoningTokens = &thinking
		} else {
			// This build predates output_tokens_details. Recording the absence
			// keeps the UI from presenting a missing value as a measured zero.
			c.coverage.missing("thinking_tokens")
		}
		if u.CacheCreation == nil {
			c.coverage.missing("cache_creation_ttl_split")
		}
		ev.Usage = &usage

		// Decode content only now, and only far enough to find tool calls. A
		// shape this build does not expect costs the tool list, not the turn.
		var blocks []ccContentBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err == nil {
			for _, block := range blocks {
				if block.Type == "tool_use" {
					ev.ToolCalls = append(ev.ToolCalls, trace.ToolCall{ID: block.ID, Name: block.Name})
				}
			}
		} else if len(rec.Message.Content) > 0 {
			c.coverage.missing("tool_calls")
		}
		c.coverage.record(rec.Version)
		return emit(ev)
	})
}

// errorEvent turns a failed call into a trace event. Status is parsed where the
// transcript gives one, so a 429 is distinguishable from any other failure.
func (c *ClaudeCode) errorEvent(rec ccRecord) trace.Event {
	projectName, projectID, inRepo := c.projects.Resolve(rec.CWD)
	ev := trace.Event{
		Type:            trace.EventError,
		Source:          ClaudeCodeName,
		Timestamp:       parseTimestamp(rec.Timestamp),
		SessionID:       rec.SessionID,
		TurnID:          rec.UUID,
		RequestID:       rec.RequestID,
		Project:         projectName,
		ProjectID:       projectID,
		InRepo:          inRepo,
		GitBranch:       rec.GitBranch,
		Effort:          rec.Effort,
		ProducerVersion: rec.Version,
		Subagent:        rec.IsSidechain,
		Error:           rec.APIError,
	}
	if ev.Error == "" {
		ev.Error = "api error"
	}
	if status, err := strconv.Atoi(rec.APIErrorStatus); err == nil {
		ev.StatusCode = status
	}
	if rec.Message != nil {
		ev.Model = rec.Message.Model
		if rec.Message.ID != "" {
			ev.MessageID = rec.Message.ID
			ev.TurnID = rec.Message.ID
		}
	}
	if ev.SessionID == "" {
		ev.SessionID = "unknown"
	}
	return ev
}

// jsonlFiles lists .jsonl files under root, oldest first.
func jsonlFiles(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var found []entry

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// A directory we cannot read is not fatal: keep scanning the rest.
			return nil
		}
		if !info.IsDir() && filepath.Ext(path) == ".jsonl" {
			found = append(found, entry{path, info.ModTime()})
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // tool not installed
		}
		return nil, err
	}

	sort.Slice(found, func(i, j int) bool { return found[i].mod.Before(found[j].mod) })
	paths := make([]string, 0, len(found))
	for _, e := range found {
		paths = append(paths, e.path)
	}
	return paths, nil
}

// scanJSONL reads whole lines from an offset and reports how far it consumed.
//
// A trailing partial line is deliberately not consumed: the file is being
// appended to while it is read, and resuming from the returned offset picks the
// record up once it is complete.
func scanJSONL(path string, from int64, handle func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return from, nil
		}
		return from, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return from, err
	}
	// A file that shrank was rotated or replaced; start over.
	if info.Size() < from {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from, err
	}

	reader := bufio.NewReaderSize(f, 256*1024)
	consumed := from
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// No newline: the record is still being written. Leave the offset
			// before it so the next pass sees the whole line.
			break
		}
		if handleErr := handle(line); handleErr != nil {
			return consumed, handleErr
		}
		consumed += int64(len(line))
	}
	return consumed, nil
}
