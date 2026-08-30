import { useEffect, useState } from "react";

import { api, type Session } from "../api";
import {
  tokens,
  usd,
  pct,
  dateTime,
  durationOrUnknown,
  shortId,
} from "../format";
import { CardHead } from "./ui";

interface Props {
  days: number;
  onOpen: (id: string) => void;
}

export function SessionList({ days, onOpen }: Props) {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [cursor, setCursor] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Changing the window restarts the list rather than appending to results
  // from a different range.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);

    api
      .sessions(days, 50)
      .then((page) => {
        if (cancelled) return;
        setSessions(page.sessions);
        setCursor(page.next_cursor);
      })
      .catch(
        (e) =>
          !cancelled && setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => !cancelled && setLoading(false));

    return () => {
      cancelled = true;
    };
  }, [days]);

  async function loadMore() {
    if (!cursor) return;
    setLoading(true);
    try {
      const page = await api.sessions(days, 50, cursor);
      setSessions((prev) => [...prev, ...page.sessions]);
      setCursor(page.next_cursor);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }

  if (error) return <div className="error-banner">{error}</div>;
  if (loading && sessions.length === 0)
    return <div className="empty">Loading sessions…</div>;
  if (sessions.length === 0) {
    return (
      <div className="empty">
        No sessions in the last {days} days.
        <div style={{ marginTop: 8, fontSize: 13 }}>
          Point an app at the proxy with{" "}
          <span className="mono">ANTHROPIC_BASE_URL=http://127.0.0.1:8317</span>{" "}
          and traces will appear here.
        </div>
      </div>
    );
  }

  return (
    <div className="card">
      <CardHead title="Sessions" meta={`${sessions.length} loaded`} />
      <p className="card-sub">
        Most recently active first. Select one to see its timeline.
      </p>

      <div className="table-wrap card-body">
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
                        <span style={{ fontWeight: 550 }}>{s.project}</span>
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
                      <span className="muted">+{s.tool_names.length - 3}</span>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      {cursor && (
        <div style={{ marginTop: 12 }}>
          <button className="icon-btn" onClick={loadMore} disabled={loading}>
            {loading ? "Loading…" : "Load more"}
          </button>
        </div>
      )}
    </div>
  );
}
