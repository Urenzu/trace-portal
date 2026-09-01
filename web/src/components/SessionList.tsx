import { useCallback, useEffect, useRef, useState } from "react";

import { api, type Session } from "../api";
import {
  tokens,
  usd,
  pct,
  dateTime,
  durationOrUnknown,
  shortId,
} from "../format";
import { CardHead, SearchBox } from "./ui";

/** How much of the list is on screen before it starts scrolling internally. */
const LIST_MAX_HEIGHT = 460;
/** Distance from the bottom at which the next page is fetched. */
const PREFETCH_PX = 240;
/** Typing pause before a search is sent. */
const DEBOUNCE_MS = 220;

/**
 * Recency is first because it is the default and the cheap one — the listing
 * walks days backwards and stops when the page is full. The others have to read
 * the whole window before they know what comes first, which is a fair price for
 * "what did I spend the most on", but not one to pay by accident.
 */
const ORDERS = [
  { key: "recent", label: "Most recent" },
  { key: "cost", label: "Most expensive" },
  { key: "turns", label: "Most turns" },
  { key: "errors", label: "Most errors" },
] as const;

const SEARCH_HINT = [
  "Search project, branch, model, tool, or session id.",
  "",
  "  fin-agentic           any of those columns",
  "  project:trace-portal",
  "  branch:main",
  "  model:opus",
  "  tool:PowerShell",
  "  has:errors",
  "  cost:>5   turns:>100",
].join("\n");

interface Props {
  days: number;
  onOpen: (id: string) => void;
  /**
   * A filter term the list is permanently narrowed by — how the project view
   * reuses this list. It is combined with whatever the reader types rather
   * than replaced by it, so searching inside a project stays inside it.
   */
  scope?: string;
  title?: string;
  /**
   * A query pushed in from outside — the errors tile aiming the list at
   * `has:errors`. It seeds the box rather than replacing it, so the reader can
   * see what was searched and edit or clear it like anything they typed.
   */
  seed?: { query: string; nonce: number };
}

export function SessionList({
  days,
  onOpen,
  scope,
  title = "Sessions",
  seed,
}: Props) {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [daysScanned, setDaysScanned] = useState(0);

  // Two pieces of state, deliberately. `input` is what is on screen and has to
  // update on every keystroke; `query` is what has actually been sent, and lags
  // behind it so a request is not fired per character.
  const [input, setInput] = useState("");
  const [query, setQuery] = useState("");
  const [order, setOrder] = useState<string>("recent");

  const scroller = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const timer = setTimeout(() => setQuery(input.trim()), DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [input]);

  // Keyed on the nonce, not the text: clicking the same tile twice has to
  // re-aim a list the reader has since edited, and comparing the query alone
  // would make the second click do nothing.
  const seedQuery = seed?.query;
  const seedNonce = seed?.nonce;
  useEffect(() => {
    if (seedNonce !== undefined && seedQuery !== undefined) setInput(seedQuery);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [seedNonce]);

  const sent = [scope, query].filter(Boolean).join(" ");

  // Changing the window or the search restarts the list rather than appending
  // to results from a different range.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);

    api
      .sessions(days, 50, undefined, sent, order)
      .then((page) => {
        if (cancelled) return;
        setSessions(page.sessions);
        setCursor(page.next_cursor);
        setDaysScanned(page.days_scanned);
        // A new result set starts at the top. Leaving the scroll position where
        // it was would drop the reader into the middle of a list they have not
        // seen the beginning of.
        scroller.current?.scrollTo({ top: 0 });
      })
      .catch(
        (e) =>
          !cancelled && setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => !cancelled && setLoading(false));

    return () => {
      cancelled = true;
    };
  }, [days, sent, order]);

  const loadMore = useCallback(async () => {
    if (!cursor || loading) return;
    setLoading(true);
    try {
      const page = await api.sessions(days, 50, cursor, sent, order);
      setSessions((prev) => [...prev, ...page.sessions]);
      setCursor(page.next_cursor);
      setDaysScanned((prev) => prev + page.days_scanned);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [cursor, days, loading, order, sent]);

  // Paging on scroll rather than on a click. Opening the full history should
  // not drop hundreds of rows onto the page at once, and it should not make
  // someone click through them either.
  function onScroll(e: React.UIEvent<HTMLDivElement>) {
    const el = e.currentTarget;
    if (el.scrollHeight - el.scrollTop - el.clientHeight < PREFETCH_PX) {
      void loadMore();
    }
  }

  const searching = query !== "";
  const shown = `${sessions.length}${cursor ? "+" : ""}`;

  return (
    <div className="card" id="session-list">
      <CardHead
        title={title}
        /* A narrow search walks the window from the newest day back, so how far
           it actually reached belongs beside the count: the answer to "why is
           this not here" is usually the window, not the query. */
        meta={
          searching
            ? `${shown} matching · searched ${Math.min(daysScanned, days)} of ${days} days`
            : `${shown} loaded`
        }
        action={
          <>
            <SearchBox
              value={input}
              onChange={setInput}
              placeholder="Search sessions…"
              hint={SEARCH_HINT}
            />
            <select
              className="select"
              value={order}
              onChange={(e) => setOrder(e.target.value)}
              aria-label="Sort sessions"
            >
              {ORDERS.map((o) => (
                <option key={o.key} value={o.key}>
                  {o.label}
                </option>
              ))}
            </select>
          </>
        }
      />
      {error && <div className="error-banner">{error}</div>}

      {sessions.length === 0 && loading && (
        <div className="empty">Loading sessions…</div>
      )}

      {sessions.length === 0 && !loading && !error && (
        <div className="empty">
          {searching ? (
            <>
              Nothing matches <span className="mono">{query}</span> in the last{" "}
              {days} days.
              <div style={{ marginTop: 8, fontSize: 13 }}>
                Widen the window above, or search by{" "}
                <span className="mono">project:</span>,{" "}
                <span className="mono">branch:</span>,{" "}
                <span className="mono">model:</span>,{" "}
                <span className="mono">tool:</span>, or{" "}
                <span className="mono">has:errors</span>.
              </div>
            </>
          ) : (
            <>
              No sessions in the last {days} days.
              <div style={{ marginTop: 8, fontSize: 13 }}>
                Point an app at the proxy with{" "}
                <span className="mono">
                  ANTHROPIC_BASE_URL=http://127.0.0.1:8317
                </span>{" "}
                and traces will appear here.
              </div>
            </>
          )}
        </div>
      )}

      {sessions.length > 0 && (
        <div
          className="table-wrap card-body scroll-list"
          ref={scroller}
          onScroll={onScroll}
          style={{ maxHeight: LIST_MAX_HEIGHT }}
        >
          <table>
            <thead>
              <tr>
                <th>Project</th>
                <th>Started</th>
                <th>Model</th>
                <th className="right">Turns</th>
                <th className="right">Duration</th>
                <th className="right">Cache hit</th>
                <th className="right">Tokens</th>
                <th className="right">Cost</th>
                <th>Tools</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => {
                const u = s.usage;
                const total =
                  u.input_tokens +
                  u.output_tokens +
                  u.cache_creation_input_tokens +
                  u.cache_read_input_tokens;
                return (
                  <tr
                    key={s.id}
                    className="clickable"
                    onClick={() => onOpen(s.id)}
                  >
                    <td>
                      {/* The project is what a person recognises; the session id
                          is a uuid and only useful for looking one up. */}
                      {s.project ? (
                        <>
                          <span className="row-name">{s.project}</span>
                          {s.git_branch && s.git_branch !== "HEAD" && (
                            <span
                              className="muted mono"
                              style={{ marginLeft: 6, fontSize: 11.5 }}
                            >
                              {s.git_branch}
                            </span>
                          )}
                        </>
                      ) : (
                        <span className="mono muted">{shortId(s.id)}</span>
                      )}
                      {s.errors ? (
                        <span
                          className="pill err"
                          style={{ marginLeft: 6 }}
                          title="Failed API calls in this session"
                        >
                          {s.errors} failed
                        </span>
                      ) : null}
                    </td>
                    <td className="muted">{dateTime(s.started_at)}</td>
                    <td className="muted">
                      {s.model || "—"}
                      {s.models && s.models.length > 1 && (
                        <span className="pill" style={{ marginLeft: 6 }}>
                          +{s.models.length - 1}
                        </span>
                      )}
                    </td>
                    <td className="right num">{s.turns}</td>
                    <td className="right num muted">
                      {durationOrUnknown(s.started_at, s.ended_at)}
                    </td>
                    <td className="right num">{pct(s.cache_hit_rate)}</td>
                    <td className="right num muted">{tokens(total)}</td>
                    <td className="right num">{usd(s.cost_usd)}</td>
                    <td>
                      {s.tool_names?.slice(0, 3).map((name) => (
                        <span
                          className="pill"
                          key={name}
                          style={{ marginRight: 4 }}
                        >
                          {name}
                        </span>
                      ))}
                      {s.tool_names && s.tool_names.length > 3 && (
                        <span className="muted">
                          +{s.tool_names.length - 3}
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {/* Scrolling fetches the next page on its own; the button is the keyboard
          path to the same thing. Nothing is shown once the list is complete —
          running out of rows is already visible. */}
      {sessions.length > 0 && cursor && (
        <div className="list-foot">
          <button className="icon-btn" onClick={loadMore} disabled={loading}>
            {loading ? "Loading…" : "Load more"}
          </button>
        </div>
      )}
    </div>
  );
}
