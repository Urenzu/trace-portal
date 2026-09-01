import { useEffect, useState } from "react";

import { api, type ProjectDetail } from "../api";
import { dateTime, pct, shortId, tokens, usd } from "../format";
import { SessionList } from "./SessionList";
import { TrendChart } from "./TrendChart";
import { BarRow, CardHead, Stat } from "./ui";

interface Props {
  projectId: string;
  days: number;
  windowFrom: string;
  windowTo: string;
  onBack: () => void;
  onOpenSession: (id: string) => void;
}

/**
 * One project's page.
 *
 * The dashboard answers which repository cost the most. The questions that
 * follow are all about a single repository over time, and none of them are
 * answerable from a row: is this getting more expensive, which branch is the
 * money going to, is the cache working, what is a normal session here and which
 * one was not.
 *
 * Branch is the cut that matters most. A person recognises their own branches,
 * a reviewer recognises the work they name, and a branch whose cache hit rate
 * sits forty points below the others is a concrete thing to go and look at.
 */
export function ProjectView({
  projectId,
  days,
  windowFrom,
  windowTo,
  onBack,
  onOpenSession,
}: Props) {
  const [detail, setDetail] = useState<ProjectDetail | null>(null);
  const [seed, setSeed] = useState<{ query: string; nonce: number }>();
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    api
      .project(projectId, days)
      .then((d) => !cancelled && setDetail(d))
      .catch(
        (e) =>
          !cancelled && setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [projectId, days]);

  const usage = detail?.usage;
  const totalInput = usage
    ? usage.input_tokens +
      usage.cache_creation_input_tokens +
      usage.cache_read_input_tokens
    : 0;

  return (
    <>
      <div className="page-head">
        <button className="icon-btn" onClick={onBack}>
          ← Dashboard
        </button>
        <span style={{ fontWeight: 600 }}>
          {detail?.project || "Project"}
        </span>
        {detail && !detail.in_repo && (
          <span
            className="pill"
            title="Not a git repository — work done in a plain directory"
          >
            directory
          </span>
        )}
        <span className="mono muted" style={{ fontSize: 11.5 }}>
          {projectId}
        </span>
      </div>

      {error && <div className="error-banner">{error}</div>}
      {loading && !detail && <div className="empty">Loading project…</div>}

      {detail && !detail.found && (
        <div className="empty" style={{ marginBottom: 14 }}>
          No activity for this project in the last {days} days. Widen the window
          above to see its history.
        </div>
      )}

      {detail && detail.found && (
        <>
          <div className="tiles">
            <div className="tile">
              <div className="tile-label">Spend</div>
              <div className="tile-value num">{usd(detail.cost_usd)}</div>
              <div className="tile-note">
                {detail.turns.toLocaleString()} turns
              </div>
            </div>
            <div className="tile">
              <div className="tile-label">Typical session</div>
              <div className="tile-value num">
                {usd(detail.median_session_cost)}
              </div>
              {/* Median, not mean: one runaway session drags a mean well above
                  anything that actually happened here. */}
              <div className="tile-note">
                median of {detail.sessions.toLocaleString()}
              </div>
            </div>
            <div className="tile">
              <div className="tile-label">Cache hit rate</div>
              <div className="tile-value num">{pct(detail.cache_hit_rate)}</div>
              <div className="tile-note">of input tokens</div>
            </div>
            <div className="tile">
              <div className="tile-label">Tokens</div>
              <div className="tile-value num">{tokens(totalInput)}</div>
              <div className="tile-note">input, all classes</div>
            </div>
            <div className="tile">
              <div className="tile-label">Errors</div>
              <div className="tile-value num">{detail.errors}</div>
              <div className="tile-note">
                {detail.errors > 0 ? "failed or 4xx/5xx turns" : "none"}
              </div>
            </div>
          </div>

          {detail.by_day && detail.by_day.length > 0 && (
            <div className="card">
              <CardHead
                title="Spend over time"
                meta={`${detail.by_day.length} active ${detail.by_day.length === 1 ? "day" : "days"}`}
              />
              <div className="card-body">
                <TrendChart
                  points={detail.by_day}
                  from={windowFrom}
                  to={windowTo}
                />
              </div>
            </div>
          )}

          {detail.by_branch && detail.by_branch.length > 0 && (
            <div className="card">
              <CardHead
                title="By branch"
                meta={`${detail.by_branch.length} ${detail.by_branch.length === 1 ? "branch" : "branches"}`}
              />
              <div className="card-body table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Branch</th>
                      <th className="right">Cost</th>
                      <th className="right">Sessions</th>
                      <th className="right">Turns</th>
                      <th className="right">Cache hit</th>
                      <th className="right">Last active</th>
                    </tr>
                  </thead>
                  <tbody>
                    {detail.by_branch.map((b) => (
                      <tr
                        key={b.branch}
                        className="clickable"
                        title={`Show sessions on ${b.branch}`}
                        /* A branch row is a filter, not a readout: the obvious
                           next question is which sessions made up that cost. */
                        onClick={() => {
                          setSeed({
                            query: `branch:${b.branch}`,
                            nonce: Date.now(),
                          });
                          document
                            .getElementById("session-list")
                            ?.scrollIntoView({ behavior: "smooth" });
                        }}
                      >
                        <td>
                          <span className="row-name mono">{b.branch}</span>
                          {b.errors ? (
                            <span
                              className="pill err"
                              style={{ marginLeft: 6 }}
                            >
                              {b.errors} err
                            </span>
                          ) : null}
                        </td>
                        <td className="right num">{usd(b.cost_usd)}</td>
                        <td className="right num muted">{b.sessions}</td>
                        <td className="right num muted">
                          {b.turns.toLocaleString()}
                        </td>
                        <td className="right num">{pct(b.cache_hit_rate)}</td>
                        <td className="right muted">
                          {b.last_active ? dateTime(b.last_active) : "—"}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {(detail.costliest_session || detail.longest_session) && (
            <div className="card">
              <CardHead title="Outliers" meta="the two worth opening" />
              <div className="stats" style={{ marginTop: 14 }}>
                {detail.costliest_session && (
                  <Stat
                    label="Most expensive session"
                    value={usd(detail.costliest_session.cost_usd)}
                    note={`${detail.costliest_session.turns.toLocaleString()} turns · ${dateTime(detail.costliest_session.started_at)}`}
                  />
                )}
                {detail.longest_session && (
                  <Stat
                    label="Longest session"
                    value={`${detail.longest_session.turns.toLocaleString()} turns`}
                    note={`${usd(detail.longest_session.cost_usd)} · ${dateTime(detail.longest_session.started_at)}`}
                  />
                )}
              </div>
              <div className="card-body">
                {detail.costliest_session && (
                  <button
                    className="icon-btn"
                    onClick={() =>
                      onOpenSession(detail.costliest_session!.id)
                    }
                  >
                    Open {shortId(detail.costliest_session.id)} →
                  </button>
                )}
              </div>
            </div>
          )}

          {detail.tools && detail.tools.length > 0 && (
            <div className="card">
              <CardHead
                title="Tool calls"
                meta={`${detail.tools
                  .reduce((sum, t) => sum + t.count, 0)
                  .toLocaleString()} calls · ${detail.tools.length} distinct`}
              />
              <div className="card-body">
                {detail.tools.slice(0, 8).map((t) => (
                  <BarRow
                    key={t.name}
                    label={t.name}
                    value={t.count}
                    max={detail.tools![0].count}
                    display={t.count.toLocaleString()}
                  />
                ))}
              </div>
            </div>
          )}

          {detail.by_model && detail.by_model.length > 1 && (
            <div className="card">
              <CardHead title="Turns by model" meta={`${detail.by_model.length} distinct`} />
              <div className="card-body">
                {detail.by_model.map((m) => (
                  <BarRow
                    key={m.name}
                    label={m.name}
                    value={m.count}
                    max={detail.by_model![0].count}
                    display={m.count.toLocaleString()}
                  />
                ))}
              </div>
            </div>
          )}
        </>
      )}

      <SessionList
        days={days}
        onOpen={onOpenSession}
        scope={`projectid:${projectId}`}
        title="Sessions in this project"
        seed={seed}
      />
    </>
  );
}
