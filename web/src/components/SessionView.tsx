import { useEffect, useState } from "react";

import { api, type SessionDetail, type Turn } from "../api";
import { tokens, usd, pct, dateTime, duration } from "../format";
import { daySegments, idleLabel } from "./days";
import { SessionTimeline } from "./SessionTimeline";
import { TurnTable } from "./TurnTable";
import { CardHead } from "./ui";

interface Props {
  id: string;
  days: number;
  onBack: () => void;
}

export function SessionView({ id, days, onBack }: Props) {
  const [detail, setDetail] = useState<SessionDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Turn | null>(null);

  const [widened, setWidened] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(null);
    setWidened(false);

    // A link to a session carries no time window, so opening one older than the
    // current filter would report it missing even though the link is good.
    // Fall back to the full archive before calling it an error.
    async function load() {
      try {
        setDetail(await api.session(id, days));
      } catch {
        if (cancelled) return;
        try {
          const full = await api.session(id, 3650);
          if (cancelled) return;
          setDetail(full);
          setWidened(true);
        } catch (e) {
          if (!cancelled) setError(e instanceof Error ? e.message : String(e));
        }
      }
    }
    load();

    return () => {
      cancelled = true;
    };
  }, [id, days]);

  if (error) {
    return (
      <>
        <button
          className="icon-btn"
          onClick={onBack}
          style={{ marginBottom: 14 }}
        >
          ← All sessions
        </button>
        <div className="error-banner">{error}</div>
      </>
    );
  }
  if (!detail) return <div className="empty">Loading session…</div>;

  // A resumed session's wall-clock span counts the nights it was not running,
  // so the day breakdown is what makes that duration mean anything.
  const segments = daySegments(detail.turn_list);
  // Said once, above the table, rather than as a column of em-dashes in it.
  const timed = detail.turn_list.some(
    (t) => t.ttfb_ms > 0 || t.duration_ms > 0,
  );
  const u = detail.usage;
  const totalInput =
    u.input_tokens + u.cache_creation_input_tokens + u.cache_read_input_tokens;

  return (
    <>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 12,
          marginBottom: 14,
        }}
      >
        <button className="icon-btn" onClick={onBack}>
          ← All sessions
        </button>
        <span className="mono muted">{detail.id}</span>
        {segments.length > 1 && (
          <span
            className="pill"
            title={segments
              .map(
                (seg, i) =>
                  `${seg.from.toLocaleDateString()}: ${seg.turns.length} turns` +
                  (i > 0
                    ? ` (resumed after ${idleLabel(seg.gapMS)} idle)`
                    : ""),
              )
              .join("\n")}
          >
            resumed across {segments.length} days
          </span>
        )}
        {widened && (
          <span
            className="pill"
            title="This session predates the selected window"
          >
            outside the {days}d window
          </span>
        )}
      </div>

      <div className="tiles">
        <div className="tile">
          <div className="tile-label">Cost</div>
          <div className="tile-value num">{usd(detail.cost_usd)}</div>
          <div className="tile-note">{detail.turns} turns</div>
        </div>
        <div className="tile">
          <div className="tile-label">Cache hit rate</div>
          <div className="tile-value num">{pct(detail.cache_hit_rate)}</div>
          <div className="tile-note">
            {tokens(u.cache_read_input_tokens)} of {tokens(totalInput)} input
          </div>
        </div>
        <div className="tile">
          <div className="tile-label">Duration</div>
          <div className="tile-value num">
            {duration(detail.started_at, detail.ended_at)}
          </div>
          <div className="tile-note">
            {segments.length > 1
              ? `across ${segments.length} days, from ${dateTime(detail.started_at)}`
              : `from ${dateTime(detail.started_at)}`}
          </div>
        </div>
        <div className="tile">
          <div className="tile-label">Tool calls</div>
          <div className="tile-value num">{detail.tool_calls}</div>
          <div className="tile-note" title={detail.tool_names?.join(", ")}>
            {/* A session can touch every tool available; listing them all turns
                one tile into a wall and stretches the whole row to match. */}
            {detail.tool_names?.length
              ? detail.tool_names.slice(0, 3).join(", ") +
                (detail.tool_names.length > 3
                  ? ` +${detail.tool_names.length - 3} more`
                  : "")
              : "none"}
          </div>
        </div>
      </div>

      <div className="card">
        <CardHead
          title="Turn timeline"
          meta={`${detail.turn_list.length} turns`}
        />
        <p className="card-sub">
          Each bar is one turn, positioned by when it started and stacked by how
          its tokens were billed. Wide gaps between bars are where a five-minute
          cache window can lapse — the orange that follows is the reload being
          paid for.
          {segments.length > 1 &&
            " Dashed rules mark where the session was resumed on a later day."}
        </p>
        <div className="card-body" />
        <SessionTimeline
          turns={detail.turn_list}
          selectedTurnId={selected?.turn_id}
          onSelect={(t) =>
            setSelected((prev) => (prev?.turn_id === t.turn_id ? null : t))
          }
        />
      </div>

      <div className="card">
        <CardHead title="Turns" />
        <p className="card-sub">
          Newest first — scrolling down moves backwards in time. Select a row to
          see context composition and stored payloads.
          {!timed &&
            " Per-request latency is absent: transcripts record the conversation, not the request, so nothing observed how long each call took."}
        </p>
        <div className="card-body">
          <TurnTable turns={detail.turn_list} />
        </div>
      </div>
    </>
  );
}
