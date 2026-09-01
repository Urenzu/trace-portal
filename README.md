# trace-portal

Observability for LLM agent sessions. It reads the session logs your agent tools
already write and turns them into a timeline of what each turn cost: tokens,
cache reads and writes, tool calls, spend per project and branch.

It does not sit between your agents and the API. Agent CLIs keep local session
logs for resume and context management, and token usage rides along because the
tool needs it to manage its own context window. Reading those logs gets the same
numbers a proxy would without being in the request path — so if trace-portal is
broken, stopped, or mid-upgrade, your agents keep working.

## Quick start

Requires Go 1.25+.

```sh
go build -o trace-portal ./cmd/trace-portal
./trace-portal
```

That is the whole setup. It finds your agent logs, backfills the history already
on disk, and follows them for new turns. Open <http://127.0.0.1:8317>.

The frontend is compiled into the binary, so there is nothing to serve
separately and no Node required to build it.

| Source | Log directory |
| --- | --- |
| Claude Code | `~/.claude/projects/**/*.jsonl` |

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8317` | Address to listen on |
| `-data` | `~/.trace-portal` | Where traces are written |
| `-poll` | `2s` | How often to check agent logs (`0` disables) |
| `-compact-every` | `1h` | How often to compact completed days (`0` disables) |
| `-proxy` | `false` | Also accept proxied API traffic |
| `-claude-dir` | `~/.claude/projects` | Claude Code transcript directory |

Server-mode flags — sign-in, Postgres, object storage — are covered under
[Running it as a server](#running-it-as-a-server).

### The optional proxy

For a tool that keeps no local log, start with `-proxy` and point it at the
listener:

```sh
./trace-portal -proxy
ANTHROPIC_BASE_URL=http://127.0.0.1:8317 your-app
```

The proxy forwards credentials untouched and never writes them to disk, but it
*is* in the request path: if trace-portal stops, anything pointed at it stops
too. That is why it is off by default.

## What is recorded

Only measurements. No prompts, no responses, no code, no tool arguments — the
tailer reads transcripts for their usage numbers and never copies their content.
A month of heavy use compacts to about 7 KB.

| Recorded | Not recorded |
| --- | --- |
| token counts, cache split, cost | message content |
| model, effort, stop reason, speed | tool arguments and results |
| tool names invoked | file contents or diffs |
| project name and digest, git branch | absolute paths, usernames |
| session and message ids, timings | API keys or tokens |

Absolute working directories are reduced before they are stored. A path like
`C:\Users\<name>\dev\projects\apex-analysis` names the operator and every project
beside it, so only the project name is kept, alongside a one-way digest of the
path. A test walks the whole archive to prove the path itself is never written.

Git branch names are kept as written, so avoid putting anything sensitive in one.

## The UI

- **Activity** — a calendar of the window, one cell per day, beside the turns,
  spend and active days it holds. Fifteen active days inside a twenty-nine day
  span is the shape no total can show. Days before the archive begins are drawn
  as their own state, because "nothing was recorded" and "nothing could have
  been recorded" are different claims.
- **Dashboard** — spend, what caching saved, cache hit rate, where the tokens
  went, spend over time, and a breakdown per project. The error tile is a way in
  rather than a readout: clicking it aims the session list at `has:errors`.
- **Project** — one repository over time, with a per-branch table. Branch is the
  cut that earns its place: on real data one repository showed `main` at a 45%
  cache hit rate against 99% on its feature branches.
- **Session timeline** — one column per turn, positioned by when it started and
  stacked by how its tokens were billed. Wide gaps are where a five-minute cache
  window lapses, and the cache-write band that follows is the reload being paid
  for. That relationship is why the x-axis is time, not turn index.
- **Turn table** — every value the chart encodes, as text, plus context
  composition and stored payloads fetched on demand.

Every view is linkable: the selected window rides in the hash alongside the
route, so a shared link opens on the window it was read on.

Colour choices were validated for colour-vision deficiency and for contrast in
both themes, and no value is ever encoded by colour alone.

## Query API

| Endpoint | Description |
| --- | --- |
| `GET /api/health` | Status, days captured, per-day volume, ingest coverage |
| `GET /api/sessions` | Sessions in the window (paged, searchable, sortable) |
| `GET /api/sessions/{id}` | One session with its turns |
| `GET /api/stats` | Totals plus a daily series |
| `GET /api/projects/{id}` | One project: totals, daily series, branches, tools |
| `GET /api/blobs/{ref}` | A stored payload, fetched on demand |

All accept `?from=` and `?to=` (RFC3339 or `YYYY-MM-DD`) or `?days=N`; the window
defaults to 7 days and is clamped to ten years. `/api/sessions` pages with
`?limit=` and `?cursor=`, searches with `?q=`, and orders with `?sort=` —
`recent` (default), `cost`, `turns` or `errors`.

Recency is the only order the paged scan produces for free: it walks days
backwards and stops when the page is full. The others cannot terminate early,
because the most expensive session in a window may be its oldest.

Search is a typed predicate over the columns a listing already reads — there is
no text index, because nothing here records prompts or tool arguments, so there
is no corpus to index:

```
fin-agentic            any column contains it
project:trace-portal   that column
branch:main model:opus several terms, all of which must hold
tool:PowerShell   has:errors   cost:>5   turns:>100
```

Costs come from first-party Anthropic list prices including cache multipliers.
**On a subscription none of this is billed** — the figures are what the usage
*would* cost at list price, which is still the right number for comparing
sessions or spotting a cache regression.

## How it works

```
cmd/trace-portal   entrypoint
internal/source    readers for agent session logs
internal/ingest    backfills and follows those logs into the store
internal/trace     the event model every stage shares
internal/store     append-only JSONL log and blob store (local)
internal/postgres  the same, for a server
internal/eventstore the interface those two share
internal/compact   JSONL to partitioned Parquet, rollups, and the read path
internal/objectstore where compacted history lives: a directory, or S3/R2/MinIO
internal/query     folds events into sessions and turns
internal/pricing   model rates and cost computation
internal/api       the read API
internal/auth      OIDC sign-in and the CLI device flow
internal/collect   shipping to a server, and receiving from a collector
internal/tenant    resolves a tenant to its storage
internal/web       the built frontend, embedded
internal/proxy     optional reverse proxy
web/               React + TypeScript + Vite source
```

Every reader normalizes into one event model, so adding a tool is one file
rather than a new pipeline. Several sources can observe the same call, so turns
are keyed by the API's own message id (`msg_…`) and collapse into one turn no
matter how many sources recorded it. That key matters for a subtler reason too:
Claude Code writes one record per content block, each repeating the whole
message's usage, so summing records would nearly double every total.

### Storage and compaction

The hot path appends, because appending is fast and crash-safe. Reading it back
is not: a year of heavy use is hundreds of megabytes of JSON that must be parsed
row-wise to answer any question.

A background job rewrites each completed day into columnar Parquet, partitioned
by date, alongside small pre-aggregated rollups. Today is never compacted, since
it is still being appended to.

```
<data>/events/2026-08-29.jsonl        raw append-only log
<data>/compact/2026-08-28/            one partition per completed day
<data>/compact/rollup/                pre-aggregated, spans every day
```

Measured over 365 days of heavy use (1,000 turns/day, 365k turns):

| Query | JSONL | Parquet | |
| --- | --- | --- | --- |
| Aggregate stats, 365 days | 4,345 ms | **3 ms** | 1,473x |
| Session list, first page | 7,628 ms | **22 ms** | 347x |
| On disk | 298 MB | **6.2 MB** | 48x smaller |

The dashboard win comes from the rollups rather than the file format: stats read
one row per day, so cost is O(days) and stays flat as history grows. They live
in a single file spanning every day, because opening one small file per day
costs milliseconds each and that overhead dominates a wide window — a fact that
gets roughly twenty times more expensive when those files are in a bucket.

Two properties follow from that, and both are load-bearing once storage is
remote. A day covered by neither the rollup nor the write-ahead window is known
to be empty without reading anything: an archive with 15 active days in a year
would otherwise spend most of a query proving 350 days were empty. And the
rollup is held in memory, invalidated by the one place that writes it.

Partitions are ordinary Parquet with no engine lock-in:

```sql
SELECT model, sum(cost_usd) FROM 'compact/*/turns.parquet' GROUP BY model
```

Listing and lookup terminate early. A session's turns are *not* contiguous — an
agent CLI reuses the session id when a conversation is resumed — so a rollup of
one row per session per day records the days each session actually touched.
Inferring it instead split one conversation into several listed sessions and
truncated its detail at the newest fragment.

### Why this keeps its own archive

Claude Code prunes transcripts after roughly a month. Everything older is gone,
so multi-month analysis is only possible if something durable captured the data
while it existed. It follows that trace-portal has to run regularly: a gap longer
than the retention window is unrecoverable.

Nothing here ever deletes an ingested trace, and three properties keep the
archive trustworthy, each covered by a test: it outlives its source, re-reading
cannot double-count, and compaction resolves duplicates permanently.

### Schema drift

Agent log formats are internal and move constantly. One machine in daily use
went through fifteen Claude Code versions in twenty-six days, and the record
shape changed three times inside that span.

Drift is not usually loud — a renamed field starts reading as zero and the
dashboard keeps showing a confident number. Four rules prevent that:

- **Absent is not zero.** A value the producing build never reported is stored
  as absent. Thinking tokens are the live example: 1,815 of 6,236 real turns
  came from builds predating `output_tokens_details`.
- **Every event records the build that produced it.**
- **Unrecognised fields are counted, not ignored.** This is how the ignored
  API-error fields were found: transcripts had been recording rate limits all
  along, and a throttled run was reporting zero errors.
- **Shape variation must not fail a record.** `message.content` is an array on
  assistant records and a bare string on some user records.

The UI reports all of this rather than hiding it.

### Why cache reads are so large

A stateless API means every turn resends the whole conversation, so total cache
reads are roughly turns multiplied by mean context — which grows quadratically
with session length. One real 2,746-turn session read 942M cached tokens against
a mean context of 346K: at list prices, the difference between roughly $4,700 and
$470 for that session alone.

Fresh input tokens are not the new message. New content is written to cache, so
the uncached remainder is a constant 1–2 tokens per turn on 94% of real turns.

## Running it as a server

The same binary serves many people. Storage is chosen by configuration, not by a
different build:

- **Postgres** holds the days still being written to, because object storage
  cannot append and a server needs concurrent writers.
- **Object storage** — S3, R2, or MinIO — holds compacted Parquet, which is
  written once and then only read.
- Unset, both stay local files, which is what keeps the single-machine tool a
  single file with nothing to install.

```sh
docker compose up -d          # server + Postgres + MinIO
trace-portal login -server https://app.example.com
```

Sign-in is OpenID Connect against any issuer, plus RFC 8628 device authorization
for the CLI — served by this system rather than by the provider, since most
providers do not implement it and depending on theirs would make the CLI work
with only some of them. Sign-in is off unless an issuer is configured, and with
it off the ingest endpoint does not exist either.

A collector ships the local archive rather than the tailer, so a server that is
down delays delivery and can never cost a turn. Every tenant resolves to its own
storage root, and identity on an incoming batch comes from the credential rather
than the payload.

| Flag | Environment | Description |
| --- | --- | --- |
| `-postgres` | `TRACE_PORTAL_POSTGRES` | Postgres URL for the hot window |
| `-s3-endpoint` | `TRACE_PORTAL_S3_ENDPOINT` | S3-compatible endpoint |
| `-s3-bucket` | `TRACE_PORTAL_S3_BUCKET` | Bucket for compacted partitions |
| `-oidc-issuer` | `TRACE_PORTAL_OIDC_ISSUER` | Enables sign-in when set |
| `-oidc-client-id` | `TRACE_PORTAL_OIDC_CLIENT_ID` | OAuth client id |
| `-public-url` | `TRACE_PORTAL_PUBLIC_URL` | Externally reachable base URL |

Environment wins over flags, because a container is configured with environment
variables and a secret on a command line is visible in the process table.

## Development

```sh
go test ./...              # unit tests
go test -race ./...        # with the race detector

# Postgres and object storage tests skip unless a backend is reachable:
docker compose up -d postgres minio
TRACE_PORTAL_TEST_POSTGRES='postgres://trace:trace@localhost:5433/trace_portal?sslmode=disable' \
TRACE_PORTAL_TEST_S3=localhost:9000 go test ./...
```

Postgres is published on 5433, not 5432, so it cannot collide with an
already-installed one — when they collide, every connection lands on the other
database while the container still reports healthy.

Working on the frontend:

```sh
cd web
npm install
npm run dev     # Vite on :5173, proxying /api to a trace-portal on :8317
npm run build   # rebuilds the bundle embedded in the binary
```

The built bundle is committed under `internal/web/dist`, so `go build` works
without Node installed. Rebuild it whenever you change `web/`, or the binary
keeps serving the old UI and nothing tells you.

`scripts/dev.ps1` starts the server against local files, or `-Stack` against the
compose Postgres and MinIO.

## Known gaps

Transcripts record the conversation, not the request, so two things the proxy
sees are absent when tailing: the tool *catalogue* offered to the model (only
tools actually invoked appear), and per-request latency. Claude Code's
`turn_duration` records do not close that gap — they cover 252 of 6,719
assistant records, are emitted per user prompt rather than per API call, and
measure wall-clock time including tool execution.

Still open:

- **No CI.** Every check here is a thing someone remembers to do.
- **The read API has no tenant routing.** It serves the process's own tenant, so
  a signed-in user cannot yet be shown their own data on a shared server.
- **`SessionsRanked` holds a whole window in memory.** Ranking by cost cannot
  terminate early, so it loads every session in the window to sort it.
- **Handlers do not thread request context through the compactor.** A client
  that navigates away leaves its query running to completion.
- **No frontend tests and no linter.** The date arithmetic alone deserves them.
- **Binding beyond loopback has no authentication** unless an issuer is
  configured. `-addr 0.0.0.0:8317` exposes the whole archive.
- **Per-tenant compaction and retention.** A server compacts one tenant, and
  nothing yet drops days from Postgres once they are durable in Parquet.

## Status

The single-user tool is complete: log tailing, storage, compaction, the query
API, the embedded UI, and an optional proxy. The cloud path — identity, sign-in,
the collector/server split, tenant isolation, Postgres, object storage — is
built and tested but not deployed anywhere.

Deliberately out of scope: recording prompts or tool arguments, and the
mechanistic-interpretability layer.
