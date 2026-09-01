// Package query folds the append-only event stream into the session and turn
// views the UI reads. It works directly off JSONL today; when compaction into
// Parquet lands, it becomes the interface that fronts it.
package query

import (
	"sort"
	"time"

	"github.com/Urenzu/trace-portal/internal/pricing"
	"github.com/Urenzu/trace-portal/internal/trace"
)

// Turn is one request/response exchange, assembled from the paired events.
type Turn struct {
	TurnID    string `json:"turn_id"`
	SessionID string `json:"session_id"`

	// Identity is who produced this turn, stamped at capture. Inline in the
	// JSON so a client reads user_id beside project rather than under a nested
	// object, matching how the field is stored.
	trace.Identity

	MessageID  string    `json:"message_id,omitempty"`
	Source     string    `json:"source,omitempty"`
	Project    string    `json:"project,omitempty"`
	ProjectID  string    `json:"project_id,omitempty"`
	InRepo     bool      `json:"in_repo,omitempty"`
	GitBranch  string    `json:"git_branch,omitempty"`
	Effort     string    `json:"effort,omitempty"`
	Subagent   bool      `json:"subagent,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Model      string    `json:"model"`
	Stream     bool      `json:"stream"`
	StatusCode int       `json:"status_code,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	TTFBMS     int64     `json:"ttfb_ms"`

	MessageCount int      `json:"message_count"`
	SystemBlocks int      `json:"system_blocks"`
	ToolsOffered []string `json:"tools_offered,omitempty"`

	Usage     trace.Usage      `json:"usage"`
	CostUSD   float64          `json:"cost_usd"`
	Priced    bool             `json:"priced"`
	ToolCalls []trace.ToolCall `json:"tool_calls,omitempty"`

	RequestBlob  string `json:"request_blob,omitempty"`
	ResponseBlob string `json:"response_blob,omitempty"`

	Error string `json:"error,omitempty"`
	// Pending is true when a request was recorded but no response or error
	// followed — an in-flight turn, or one lost to a crash.
	Pending bool `json:"pending"`
}

// CacheHitRate is the share of this turn's input tokens served from cache.
func (t Turn) CacheHitRate() float64 { return cacheHitRate(t.Usage) }

func setIfEmpty(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

// Session is the rolled-up view of one conversation.
type Session struct {
	ID string `json:"id"`

	// Identity comes from the session's turns, which all share one: a
	// conversation happens on one machine, belonging to one person. A session
	// whose turns disagreed would mean two people resumed the same session id,
	// which the collector split makes impossible — ids are minted per
	// enrollment.
	trace.Identity

	Model   string `json:"model"`
	Project string `json:"project,omitempty"`
	// ProjectID is the stable digest of the working directory. Two projects can
	// share a display name, so drilling into one has to key on this rather than
	// on what is written on screen.
	ProjectID string    `json:"project_id,omitempty"`
	GitBranch string    `json:"git_branch,omitempty"`
	Models    []string  `json:"models,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Turns     int       `json:"turns"`

	Usage        trace.Usage `json:"usage"`
	CostUSD      float64     `json:"cost_usd"`
	Unpriced     int         `json:"unpriced_turns,omitempty"`
	CacheHitRate float64     `json:"cache_hit_rate"`

	ToolCalls int      `json:"tool_calls"`
	ToolNames []string `json:"tool_names,omitempty"`
	Errors    int      `json:"errors,omitempty"`
}

// Duration is the wall-clock span from the session's first turn to its last.
func (s Session) Duration() time.Duration { return s.EndedAt.Sub(s.StartedAt) }

// SessionDetail is a session together with its turns, oldest first.
//
// The field is TurnList rather than Turns because the embedded Session already
// has a Turns int; naming both the same would silently shadow the count with
// the slice.
type SessionDetail struct {
	Session
	TurnList []Turn `json:"turn_list"`
}

// BuildTurns pairs request events with their response or error counterpart.
// Events may arrive in any order within the stream, so pairing is by key rather
// than adjacency.
//
// Several sources can observe the same call — a tailed agent log and the proxy
// both see it — so the key is the API's own message id where one exists. That
// makes the same exchange collapse into one turn no matter how many sources
// recorded it, and lets each contribute the fields only it knows.
func BuildTurns(events []trace.Event) []Turn {
	byTurn := make(map[string]*Turn, len(events))
	order := make([]string, 0, len(events))

	for _, ev := range events {
		key := turnKey(ev)
		t, seen := byTurn[key]
		if !seen {
			t = &Turn{TurnID: ev.TurnID, Pending: true}
			byTurn[key] = t
			order = append(order, key)
		}
		applyEvent(t, ev)
	}

	turns := make([]Turn, 0, len(order))
	for _, id := range order {
		t := byTurn[id]
		if cost, ok := pricing.Cost(t.Model, t.Usage); ok {
			t.CostUSD, t.Priced = cost, true
		}
		turns = append(turns, *t)
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].StartedAt.Before(turns[j].StartedAt) })
	return turns
}

// turnKey identifies the exchange an event belongs to. The API's message id is
// preferred because it is assigned upstream and every observer sees the same
// value; the proxy's locally generated TurnID is the fallback for events
// recorded before a message id was known, such as a request or a failure.
func turnKey(ev trace.Event) string {
	if ev.MessageID != "" {
		return "msg:" + ev.MessageID
	}
	return "turn:" + ev.TurnID
}

func applyEvent(t *Turn, ev trace.Event) {
	if ev.SessionID != "" {
		t.SessionID = ev.SessionID
	}
	// Merge, never overwrite with emptiness: each source knows a different
	// subset, and whichever observed a field should keep it.
	t.Identity.Merge(ev.Identity)
	setIfEmpty(&t.Source, ev.Source)
	setIfEmpty(&t.MessageID, ev.MessageID)
	setIfEmpty(&t.Project, ev.Project)
	setIfEmpty(&t.ProjectID, ev.ProjectID)
	if ev.InRepo {
		t.InRepo = true
	}
	setIfEmpty(&t.GitBranch, ev.GitBranch)
	setIfEmpty(&t.Effort, ev.Effort)
	if ev.Subagent {
		t.Subagent = true
	}
	if t.TurnID == "" {
		t.TurnID = ev.TurnID
	}
	switch ev.Type {
	case trace.EventRequest:
		t.StartedAt = ev.Timestamp
		setIfEmpty(&t.Model, ev.Model)
		setIfEmpty(&t.RequestBlob, ev.RequestBlob)
		if ev.Stream {
			t.Stream = true
		}
		if ev.MessageCount != 0 {
			t.MessageCount = ev.MessageCount
		}
		if ev.SystemBlocks != 0 {
			t.SystemBlocks = ev.SystemBlocks
		}
		if len(ev.ToolsOffered) > 0 {
			t.ToolsOffered = ev.ToolsOffered
		}

	case trace.EventResponse:
		t.Pending = false
		// Merge rather than overwrite. When two sources describe the same call
		// one of them will lack a field, and assigning unconditionally would
		// let whichever arrived last erase what the other observed.
		if ev.StatusCode != 0 {
			t.StatusCode = ev.StatusCode
		}
		setIfEmpty(&t.StopReason, ev.StopReason)
		setIfEmpty(&t.ResponseBlob, ev.ResponseBlob)
		if ev.DurationMS != 0 {
			t.DurationMS = ev.DurationMS
		}
		if ev.TTFBMS != 0 {
			t.TTFBMS = ev.TTFBMS
		}
		if len(ev.ToolCalls) > 0 {
			t.ToolCalls = ev.ToolCalls
		}
		if ev.Usage != nil {
			t.Usage = *ev.Usage
		}
		// A response event carries the model the API actually served, which
		// wins over the requested alias when the two differ.
		if ev.Model != "" {
			t.Model = ev.Model
		}
		if t.StartedAt.IsZero() {
			t.StartedAt = ev.Timestamp
		}

	case trace.EventError:
		t.Pending = false
		t.Error = ev.Error
		t.DurationMS = ev.DurationMS
		if t.StartedAt.IsZero() {
			t.StartedAt = ev.Timestamp
		}
	}
}

// BuildSessions rolls turns up into sessions, most recently active first.
func BuildSessions(events []trace.Event) []Session {
	return SessionsFromTurns(BuildTurns(events))
}

// SessionsFromTurns rolls already-assembled turns up into sessions. Compacted
// Parquet partitions store turns directly, so they enter here without ever
// being re-expanded into events.
func SessionsFromTurns(turns []Turn) []Session {
	bySession := make(map[string][]Turn)
	for _, t := range turns {
		bySession[t.SessionID] = append(bySession[t.SessionID], t)
	}

	sessions := make([]Session, 0, len(bySession))
	for id, turns := range bySession {
		sessions = append(sessions, summarize(id, turns))
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].EndedAt.After(sessions[j].EndedAt) })
	return sessions
}

// BuildSessionDetail returns one session with its turns, or false if the
// session has no events in the supplied stream.
func BuildSessionDetail(events []trace.Event, id string) (SessionDetail, bool) {
	return SessionDetailFromTurns(BuildTurns(events), id)
}

// SessionDetailFromTurns picks one session out of already-assembled turns.
func SessionDetailFromTurns(all []Turn, id string) (SessionDetail, bool) {
	var turns []Turn
	for _, t := range all {
		if t.SessionID == id {
			turns = append(turns, t)
		}
	}
	if len(turns) == 0 {
		return SessionDetail{}, false
	}
	return SessionDetail{Session: summarize(id, turns), TurnList: turns}, true
}

func summarize(id string, turns []Turn) Session {
	s := Session{ID: id, Turns: len(turns)}
	models := map[string]bool{}
	tools := map[string]bool{}

	for i, t := range turns {
		if i == 0 || t.StartedAt.Before(s.StartedAt) {
			s.StartedAt = t.StartedAt
		}
		end := t.StartedAt.Add(time.Duration(t.DurationMS) * time.Millisecond)
		if end.After(s.EndedAt) {
			s.EndedAt = end
		}

		s.Identity.Merge(t.Identity)
		s.Usage.Add(t.Usage)
		s.CostUSD += t.CostUSD
		if !t.Priced && t.Model != "" {
			s.Unpriced++
		}
		if t.Error != "" || (t.StatusCode >= 400 && t.StatusCode != 0) {
			s.Errors++
		}
		s.ToolCalls += len(t.ToolCalls)

		if t.Model != "" {
			models[t.Model] = true
			s.Model = t.Model // last turn's model is the session's current one
		}
		// A session belongs to whichever project it ran in; the first turn that
		// names one settles it.
		if s.Project == "" {
			s.Project = t.Project
			s.ProjectID = t.ProjectID
		}
		if s.GitBranch == "" {
			s.GitBranch = t.GitBranch
		}
		for _, tc := range t.ToolCalls {
			tools[tc.Name] = true
		}
	}

	s.Models = sortedKeys(models)
	s.ToolNames = sortedKeys(tools)
	s.CacheHitRate = cacheHitRate(s.Usage)
	return s
}

// cacheHitRate is cached input tokens over all input tokens. Output tokens are
// excluded — they are never served from cache, and including them would make
// the rate look worse purely because a turn generated a long answer.
func cacheHitRate(u trace.Usage) float64 {
	totalInput := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if totalInput == 0 {
		return 0
	}
	return float64(u.CacheReadInputTokens) / float64(totalInput)
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
