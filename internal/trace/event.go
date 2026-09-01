// Package trace defines the event model captured by the proxy. Events are the
// only contract between the hot path (proxy) and everything downstream
// (compaction, query API, UI), so keep this struct narrow and additive.
package trace

import "time"

// EventType distinguishes the records that appear in the JSONL stream.
type EventType string

const (
	// EventRequest is written before the upstream call is made.
	EventRequest EventType = "request"
	// EventResponse is written once the upstream response has fully drained.
	EventResponse EventType = "response"
	// EventError is written when the upstream call could not be completed.
	EventError EventType = "error"
)

// CacheCreation breaks cache writes down by TTL. The two tiers are billed
// differently (5-minute writes at 1.25x base input, 1-hour at 2x), so costing a
// trace accurately needs the split, not just the combined total.
type CacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

// Usage mirrors the Anthropic API usage block. Fields are pointer-free ints;
// absent counters are simply zero.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`

	// CacheCreation is absent on older API versions, in which case the whole
	// of CacheCreationInputTokens is treated as a 5-minute write.
	CacheCreation *CacheCreation `json:"cache_creation,omitempty"`

	// ReasoningTokens is the thinking portion of the output, already counted
	// inside OutputTokens and never added to it.
	//
	// It is a pointer because the producing tool may not report it at all:
	// Claude Code added output_tokens_details partway through 2.1.x, so a nil
	// here means "this version did not say" while a zero means "it thought for
	// nothing". Collapsing those two would quietly understate reasoning across
	// every user on an older build.
	ReasoningTokens *int `json:"reasoning_tokens,omitempty"`
}

// Add accumulates another usage block into u, for rolling turns up to sessions.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheCreationInputTokens += o.CacheCreationInputTokens
	u.CacheReadInputTokens += o.CacheReadInputTokens
	if o.CacheCreation != nil {
		if u.CacheCreation == nil {
			u.CacheCreation = &CacheCreation{}
		}
		u.CacheCreation.Ephemeral5mInputTokens += o.CacheCreation.Ephemeral5mInputTokens
		u.CacheCreation.Ephemeral1hInputTokens += o.CacheCreation.Ephemeral1hInputTokens
	}
}

// CacheWrites splits cache-creation tokens into (5-minute, 1-hour) tiers,
// falling back to the 5-minute tier when the API did not report a breakdown.
func (u Usage) CacheWrites() (fiveMin, oneHour int) {
	if u.CacheCreation == nil {
		return u.CacheCreationInputTokens, 0
	}
	return u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens
}

// Total returns every token the request was billed for, cached or not.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// ToolCall records a tool the model asked to invoke in its response.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Event is one append-only record. Request and response events for the same
// exchange share a TurnID; every turn of one conversation shares a SessionID.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"ts"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`

	// Identity is stamped by whatever captured the event, at the moment of
	// capture. Embedded so its fields sit inline in the JSONL rather than
	// nested, which keeps records written before identity existed decodable:
	// the keys are simply absent and the fields stay empty.
	Identity

	// Source names where this event came from: a tailed agent log, or the
	// proxy. Several sources can observe the same call, so it is only a label,
	// never an identity.
	Source string `json:"source,omitempty"`

	// ProducerVersion is the version of the tool that wrote the record this
	// event was read from. Log formats move weekly, so knowing which build
	// produced a turn is what separates a field that is genuinely zero from one
	// that build never emitted.
	ProducerVersion string `json:"producer_version,omitempty"`

	// MessageID is the API-assigned id of the response (msg_…). Every source
	// that sees a call sees the same value, which makes it the key for
	// recognising one exchange observed twice.
	MessageID string `json:"message_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`

	// Request-side fields.
	Method       string   `json:"method,omitempty"`
	Path         string   `json:"path,omitempty"`
	Model        string   `json:"model,omitempty"`
	Stream       bool     `json:"stream,omitempty"`
	MessageCount int      `json:"message_count,omitempty"`
	SystemBlocks int      `json:"system_blocks,omitempty"`
	ToolsOffered []string `json:"tools_offered,omitempty"`
	MaxTokens    int      `json:"max_tokens,omitempty"`

	// Response-side fields.
	StatusCode int        `json:"status_code,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	TTFBMS     int64      `json:"ttfb_ms,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`

	// RequestBlob and ResponseBlob reference the full payloads in the blob
	// store. They are fetched on demand so analytical queries stay narrow.
	RequestBlob  string `json:"request_blob,omitempty"`
	ResponseBlob string `json:"response_blob,omitempty"`

	// Where the work happened. Agent logs carry an absolute working directory,
	// which embeds the operating-system username and the name of every project
	// on the machine. That path is never stored: Project keeps the last segment,
	// which is what makes a cost breakdown readable, and ProjectID is a digest
	// of the full path, which separates two projects that share a name without
	// disclosing either location.
	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	// InRepo distinguishes work done inside a repository from work done in an
	// ordinary directory. Both cost money; only one is a project.
	InRepo    bool   `json:"in_repo,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	Effort    string `json:"effort,omitempty"`

	// Speed and ServiceTier affect billing on some models, so they are worth
	// keeping even though pricing does not use them yet.
	Speed       string `json:"speed,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`

	// Subagent marks a turn made by a delegated agent rather than the main
	// loop, so its cost can be attributed separately.
	Subagent bool `json:"subagent,omitempty"`

	// Error carries the transport-level failure for EventError.
	Error string `json:"error,omitempty"`
}
