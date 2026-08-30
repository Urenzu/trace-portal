# trace-portal

Local-first observability for LLM agent sessions.

trace-portal reads the session logs your agent tools already write and turns
them into a timeline of what each turn cost: token usage, cache reads and
writes, tool calls, context composition.

It does not sit between your agents and the API. Agent CLIs keep local session
logs because they need them for resume and context management, and token usage
rides along because the tool needs it to manage its own context window. Reading
those logs gets the same numbers a proxy would, without being in the request
path — so if trace-portal is broken, stopped, or mid-upgrade, your agents keep
working.

Everything stays on your machine. Nothing is uploaded anywhere.

## Quick start

Requires Go 1.25+.

```sh
go build -o trace-portal ./cmd/trace-portal
./trace-portal
```

That is the whole setup. It finds your agent logs, backfills the history
already on disk, and follows them for new turns. Open <http://127.0.0.1:8317>.

| Source | Log directory |
| --- | --- |
| Claude Code | `~/.claude/projects/**/*.jsonl` |

Other agent CLIs keep comparable logs and will be added once Claude Code is
fully covered.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8317` | Address to listen on |
| `-data` | `~/.trace-portal` | Where traces are written |
| `-poll` | `2s` | How often to check agent logs (`0` disables) |
| `-compact-every` | `1h` | How often to compact completed days (`0` disables) |
| `-proxy` | `false` | Also accept proxied API traffic |
| `-upstream` | `https://api.anthropic.com` | Upstream for the proxy |
| `-claude-dir` | `~/.claude/projects` | Claude Code transcript directory |
| `-v` | `false` | Verbose logging |

### The optional proxy

For a tool that keeps no local log — an SDK app you wrote — start with `-proxy`
and point it at the listener:

```sh
./trace-portal -proxy
ANTHROPIC_BASE_URL=http://127.0.0.1:8317 your-app
```

The proxy forwards credentials untouched and never writes them to disk, but it
*is* in the request path: if trace-portal stops, anything pointed at it stops
too. That is why it is off by default.

## What is recorded

Only measurements. No prompts, no responses, no code, no tool arguments — the
tailer reads transcripts for their usage numbers and never copies their
content. A month of heavy use compacts to about 7 KB.

| Recorded | Not recorded |
| --- | --- |
| token counts, cache split, cost | message content |
| model, effort, stop reason, speed | tool arguments and results |
| tool names invoked | file contents or diffs |
| project name and digest, git branch | absolute paths, usernames |
| session and message ids, timings | API keys or tokens |

Absolute working directories are reduced before they are stored. A path like
`C:\Users\<name>\dev\projects\apex-analysis` names the operator and every
project beside it, so only the project name is kept, alongside a one-way digest
of the path. The path itself is never written, and a test walks the whole
archive to prove it.

Git branch names are kept as written, so avoid putting anything sensitive in a
branch name if you later sync this anywhere.

## The UI

The frontend is built into the binary, so there is nothing to serve separately.
One port carries all three surfaces: `/v1/…` is proxied upstream when `-proxy`
is set, `/api/…` is the query API, and everything else is the UI.

- **Dashboard** — spend, what caching saved, cache hit rate, where the tokens
  went, and a breakdown per project.
- **Session timeline** — one column per turn, positioned by when it started and
  stacked by how its tokens were billed. Wide gaps are where a five-minute
  cache window can lapse, and the cache-write band that follows is the reload
  being paid for. That relationship is why the x-axis is time, not turn index.
  Long sessions group into at most 120 columns of equal time width, so the gaps
  survive.
- **Turn table** — every value the chart encodes, as text, plus context
  composition and stored payloads fetched on demand.

Colour choices were validated for colour-vision deficiency and for contrast
against both the light and dark surfaces, and no value is ever encoded by
colour alone. Only one chart uses colour to mean something — the token classes;
rankings of nominal things are neutral, because length already carries the
magnitude there.

Working on the frontend:

```sh
cd web
npm install
npm run dev     # Vite on :5173, proxying /api to a trace-portal on :8317
npm run build   # rebuilds the bundle embedded in the binary
```

The built bundle is committed under `internal/web/dist`, so `go build` works
without Node installed. Rebuild it whenever you change `web/`.

## Query API

| Endpoint | Description |
| --- | --- |
| `GET /api/health` | Status, days captured, and ingest coverage |
| `GET /api/sessions` | Sessions in the window, newest first (paged) |
| `GET /api/sessions/{id}` | One session with its turns |
| `GET /api/stats` | Totals: cost, cache hit rate, projects, tools |
| `GET /api/blobs/{ref}` | A stored payload, fetched on demand |

All endpoints accept `?from=` and `?to=` (RFC3339 or `YYYY-MM-DD`) or `?days=N`;
the window defaults to 7 days. `/api/sessions` pages with `?limit=` (default 50,
max 500) and `?cursor=`.

Costs come from first-party Anthropic list prices, including the cache
multipliers — reads at 0.1x base input, writes at 1.25x (5-minute TTL) or 2x
(1-hour). Turns whose model has no known price are reported as `unpriced_turns`
rather than silently costed at zero.

**On a subscription, none of this is billed.** The figures are what the usage
*would* cost at list price. Still the right number for comparing sessions or
spotting a cache regression.

## How it works

```
cmd/trace-portal   entrypoint
internal/source    readers for agent session logs
internal/ingest    backfills and follows those logs into the store
internal/trace     the event model every stage shares
internal/store     append-only JSONL log and blob store
internal/compact   JSONL to partitioned Parquet, rollups, and the read path
internal/query     folds events into sessions and turns
internal/pricing   model rates and cost computation
internal/api       the read API
internal/web       the built frontend, embedded
internal/proxy     optional reverse proxy
internal/bench     scale and overhead measurements
web/               React + TypeScript + Vite source
```

Every reader normalizes into one event model, so adding a tool is one file
rather than a new pipeline. Several sources can observe the same call — a
tailed log and the proxy both see it — so turns are keyed by the API's own
message id (`msg_…`). The same exchange collapses into one turn no matter how
many sources recorded it, and each contributes the fields only it knows.

That key matters for a subtler reason too: Claude Code writes one record per
content block of an assistant message, each repeating the whole message's
usage. Real sessions average 1.9 records per message, so summing records would
nearly double every total.

### Storage and compaction

The hot path appends to JSONL because appending a line is fast and crash-safe.
Reading it back is not: a year of heavy use is hundreds of megabytes of JSON
that must be parsed row-wise to answer any question.

A background job rewrites each completed day into columnar Parquet, partitioned
by date, alongside small pre-aggregated rollups. Today is never compacted, since
its log is still being appended to.

```
<data>/events/2026-08-29.jsonl        raw append-only log
<data>/blobs/ab/cdef….json.gz         payloads, content-addressed
<data>/compact/2026-08-28/            one partition per completed day
<data>/compact/rollup/                pre-aggregated, spans every day
```

Measured over 365 days of heavy use (1,000 turns/day, 365k turns):

| Query | JSONL | Parquet | |
| --- | --- | --- | --- |
| Aggregate stats, 365 days | 4,345 ms | **3 ms** | 1,473x |
| Session list, first page of 365 days | 7,628 ms | **22 ms** | 347x |
| On disk | 298 MB | **6.2 MB** | 48x smaller |

The dashboard win comes from the rollups rather than the file format: stats read
one row per day, so cost is O(days) and stays flat as history grows. The rollups
live in a single file spanning every day, because opening one small file per day
costs milliseconds each and that overhead dominates a wide window.

Listing and lookup terminate early. A session's turns are contiguous in time, so
listing walks days backwards and stops once the page is full, and a session is
only emitted after a whole older day has been read without it — which is what
keeps a paged list identical to an unpaged one, including sessions that span
midnight.

Partitions are ordinary Parquet with no engine lock-in:

```sql
SELECT model, sum(cost_usd) FROM 'compact/*/turns.parquet' GROUP BY model
```

### Why this keeps its own archive

Claude Code prunes transcripts after roughly a month. Everything older is gone,
including which models were used and what they cost, so multi-month analysis is
only possible if something durable captured the data while it existed. It
follows that trace-portal has to run regularly: a gap longer than the retention
window is unrecoverable, because the source is gone.

Nothing here ever deletes an ingested trace, and three properties keep the
archive trustworthy, each covered by a test:

- **It outlives its source.** Deleting a transcript does not change what has
  already been ingested.
- **Re-reading cannot double-count.** If the checkpoint is lost the whole
  history is re-read, but turns are keyed by message id, so totals are
  unchanged.
- **Compaction resolves duplicates permanently**, so the durable form never
  carries them forward.

### What counts as a project

Cost is attributed to the repository the work happened in, not the directory the
agent was started from. Those differ constantly: a run in `fin-agentic/apps/web`
would otherwise become its own project, splitting one repository's spend across
several rows. On real data that was the difference between eighteen apparent
projects and ten actual ones, with one repository's cost split three ways.

Ingestion runs on the machine that produced the logs, so the enclosing working
tree is found by walking up for a `.git` entry, cached per directory. A
directory outside any repository keeps its own name and is marked as a
directory rather than presented as a project.

### Schema drift

Agent log formats are internal and move constantly. One machine in daily use
went through fifteen Claude Code versions in twenty-six days, and the record
shape changed three times inside that span: `output_tokens_details` appeared
partway through, `slug` disappeared, `isAbortedMidStream` was added.

Drift is not usually loud — a renamed field starts reading as zero and the
dashboard keeps showing a confident number. Four rules keep that from happening:

**Absent is not zero.** A value the producing build never reported is stored as
absent, not as a measured zero. Thinking tokens are the live example: 1,815 of
6,236 real turns came from builds predating `output_tokens_details`.

**Every event records the build that produced it,** so a gap can be attributed
to a version rather than guessed at.

**Unrecognised fields are counted, not ignored.** This is how the ignored
API-error fields were found: transcripts had been recording rate limits all
along, and a throttled run was reporting zero errors.

**Shape variation must not fail a record.** `message.content` is an array on
assistant records and a bare string on some user records; typing it concretely
made every one of those fail to decode.

The UI reports all of this rather than hiding it: which versions produced the
data, which fields their builds never emitted, and which fields this build does
not yet read.

### Why cache reads are so large

A stateless API means every turn resends the whole conversation, so the
accumulated context is re-read on each turn. Total cache reads are roughly turns
multiplied by mean context, which grows quadratically with session length: one
real 2,746-turn session read 942M cached tokens against a mean context of 346K.
At list prices that is the difference between roughly $4,700 and $470 for that
session alone.

Fresh input tokens are not the new message. New content is written to cache, so
the uncached remainder is a constant 1–2 tokens per turn on 94% of real turns,
regardless of context size.

## Development

```sh
go test ./...              # unit tests
go test -race ./...        # with the race detector
go test ./internal/bench -run TestScale -v -timeout 30m
```

The scale benchmark writes a few hundred megabytes and is skipped under
`-short`.

## Known gaps

Transcripts record the conversation, not the request, so two things the proxy
sees are absent when tailing: the tool *catalogue* offered to the model (only
tools actually invoked appear), and per-turn latency. Claude Code writes
`turn_duration` records separately, which would close the latency gap.

## Status

v1 is complete and single-user by design: log tailing, storage, compaction,
query API, the embedded UI, and an optional proxy.

Deliberately out of scope for v1: multi-user or org-wide analytics, and the
mechanistic-interpretability layer.
