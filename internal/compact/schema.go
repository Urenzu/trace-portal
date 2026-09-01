// Package compact rolls the append-only JSONL log into columnar Parquet.
//
// The hot path stays JSONL because appending a line is cheap and crash-safe.
// Reading it back is not: a year of heavy use is hundreds of megabytes of JSON
// that must be parsed row-wise to answer any question. Compaction rewrites each
// completed day into Parquet partitioned by date, plus small pre-aggregated
// rollups so dashboard queries never touch per-turn data at all.
//
// The on-disk layout is deliberately engine-neutral. These are ordinary Parquet
// files, so DuckDB can query them directly:
//
//	SELECT model, sum(cost_usd) FROM 'compact/*/turns.parquet' GROUP BY model
package compact

// Column encodings are chosen for what this data actually looks like:
// session/model/stop_reason repeat enormously across a day and dictionary-encode
// to near nothing, while counters and timestamps rise monotonically and suit
// delta encoding. Blob refs are unique per turn, so they stay plain — a
// dictionary of unique values is pure overhead.

// TurnRow is one request/response exchange. This schema is the durable contract
// on disk; add fields rather than repurposing existing ones.
type TurnRow struct {
	TS        int64  `parquet:"ts,delta"` // unix milliseconds, UTC
	SessionID string `parquet:"session_id,dict"`
	TurnID    string `parquet:"turn_id"`
	Model     string `parquet:"model,dict"`
	Stream    bool   `parquet:"stream"`

	// Project is the readable folder name and ProjectID a one-way digest of the
	// full path; the path itself is never recorded. Both dictionary-encode to
	// almost nothing, since a person works in few projects.
	Project   string `parquet:"project,dict"`
	ProjectID string `parquet:"project_id,dict"`
	GitBranch string `parquet:"git_branch,dict"`
	Source    string `parquet:"source,dict"`
	MessageID string `parquet:"message_id"`

	StatusCode int32  `parquet:"status_code"`
	StopReason string `parquet:"stop_reason,dict"`
	DurationMS int64  `parquet:"duration_ms,delta"`
	TTFBMS     int64  `parquet:"ttfb_ms,delta"`

	MessageCount int32 `parquet:"message_count,delta"`
	SystemBlocks int32 `parquet:"system_blocks,delta"`

	InputTokens  int64 `parquet:"input_tokens,delta"`
	OutputTokens int64 `parquet:"output_tokens,delta"`
	CacheWrite5m int64 `parquet:"cache_write_5m,delta"`
	CacheWrite1h int64 `parquet:"cache_write_1h,delta"`
	CacheRead    int64 `parquet:"cache_read,delta"`

	CostUSD float64 `parquet:"cost_usd"`
	Priced  bool    `parquet:"priced"`

	ToolCalls    []string `parquet:"tool_calls"`
	ToolsOffered []string `parquet:"tools_offered"`

	RequestBlob  string `parquet:"request_blob"`
	ResponseBlob string `parquet:"response_blob"`
	Error        string `parquet:"error"`
	Pending      bool   `parquet:"pending"`
}

// DayRow is the single pre-aggregated row per day. The stats dashboard reads
// only these, so its cost is O(days) rather than O(turns).
type DayRow struct {
	Day      string `parquet:"day,dict"` // YYYY-MM-DD, UTC
	Sessions int64  `parquet:"sessions"`
	Turns    int64  `parquet:"turns"`
	Errors   int64  `parquet:"errors"`

	InputTokens  int64 `parquet:"input_tokens"`
	OutputTokens int64 `parquet:"output_tokens"`
	CacheWrite5m int64 `parquet:"cache_write_5m"`
	CacheWrite1h int64 `parquet:"cache_write_1h"`
	CacheRead    int64 `parquet:"cache_read"`

	CostUSD       float64 `parquet:"cost_usd"`
	SavingsUSD    float64 `parquet:"savings_usd"`
	UnpricedTurns int64   `parquet:"unpriced_turns"`
}

// ModelRow is the per-day, per-model breakdown.
type ModelRow struct {
	Day          string  `parquet:"day,dict"`
	Model        string  `parquet:"model,dict"`
	Turns        int64   `parquet:"turns"`
	InputTokens  int64   `parquet:"input_tokens"`
	OutputTokens int64   `parquet:"output_tokens"`
	CacheRead    int64   `parquet:"cache_read"`
	CostUSD      float64 `parquet:"cost_usd"`
}

// ProjectRow is the per-day, per-project breakdown. Attributing spend to a
// project is the question a person with several repositories actually asks.
type ProjectRow struct {
	Day          string  `parquet:"day,dict"`
	Project      string  `parquet:"project,dict"`
	ProjectID    string  `parquet:"project_id,dict"`
	InRepo       bool    `parquet:"in_repo"`
	Sessions     int64   `parquet:"sessions"`
	Turns        int64   `parquet:"turns"`
	InputTokens  int64   `parquet:"input_tokens"`
	OutputTokens int64   `parquet:"output_tokens"`
	CacheWrite   int64   `parquet:"cache_write"`
	CacheRead    int64   `parquet:"cache_read"`
	CostUSD      float64 `parquet:"cost_usd"`
	Errors       int64   `parquet:"errors"`
}

// ToolRow is the per-day tool-call histogram.
type ToolRow struct {
	Day   string `parquet:"day,dict"`
	Tool  string `parquet:"tool,dict"`
	Calls int64  `parquet:"calls"`
}

// SessionDayRow records which days a session touched, and how much of it fell
// on each. A session is not contiguous: one resumed after an idle day has turns
// on both sides of a gap, and without this index a reader walking days
// backwards cannot tell "this session has ended" from "this session is paused".
// Guessing that apart is what used to split one conversation into several
// listed sessions and truncate its detail view at the first idle day.
//
// It is one row per session per day — tiny beside the turn data it indexes, and
// dictionary-encoded on both key columns.
type SessionDayRow struct {
	Day       string `parquet:"day,dict"`
	SessionID string `parquet:"session_id,dict"`
	Turns     int64  `parquet:"turns"`
	FirstTS   int64  `parquet:"first_ts"` // unix milliseconds, UTC
	LastTS    int64  `parquet:"last_ts"`
}
