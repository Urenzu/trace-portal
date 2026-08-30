import { useState } from "react";

import { api, type Turn } from "../api";
import { tokens, usd, clockTime, msOrUnknown, pct } from "../format";

/**
 * The per-turn table. It is also the accessible alternative to the timeline:
 * every value encoded by color in the chart is readable here as text, which is
 * what the light-mode contrast relief requires.
 */
export function TurnTable({ turns }: { turns: Turn[] }) {
  const [expanded, setExpanded] = useState<string | null>(null);

  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Model</th>
            <th className="right">Cache read</th>
            <th className="right">Cache write</th>
            <th className="right">Input</th>
            <th className="right">Output</th>
            <th className="right">Hit</th>
            <th className="right">Cost</th>
            <th className="right">TTFB</th>
            <th className="right">Total</th>
            <th>Tools</th>
            <th>Stop</th>
          </tr>
        </thead>
        <tbody>
          {turns.map((turn) => {
            const u = turn.usage;
            const totalInput =
              u.cache_read_input_tokens +
              u.cache_creation_input_tokens +
              u.input_tokens;
            const hit =
              totalInput > 0 ? u.cache_read_input_tokens / totalInput : 0;
            const open = expanded === turn.turn_id;

            return (
              <>
                <tr
                  key={turn.turn_id}
                  className="clickable"
                  onClick={() => setExpanded(open ? null : turn.turn_id)}
                >
                  <td className="num">{clockTime(turn.started_at)}</td>
                  <td className="muted">{turn.model || "—"}</td>
                  <td className="right num">
                    {tokens(u.cache_read_input_tokens)}
                  </td>
                  <td className="right num">
                    {tokens(u.cache_creation_input_tokens)}
                  </td>
                  <td className="right num">{tokens(u.input_tokens)}</td>
                  <td className="right num">{tokens(u.output_tokens)}</td>
                  <td className="right num">{pct(hit)}</td>
                  <td className="right num">
                    {turn.priced ? usd(turn.cost_usd) : "—"}
                  </td>
                  <td className="right num muted">
                    {msOrUnknown(turn.ttfb_ms)}
                  </td>
                  <td className="right num muted">
                    {msOrUnknown(turn.duration_ms)}
                  </td>
                  <td>
                    {turn.tool_calls?.length ? (
                      turn.tool_calls.map((t) => (
                        <span
                          className="pill"
                          key={t.id || t.name}
                          style={{ marginRight: 4 }}
                        >
                          {t.name}
                        </span>
                      ))
                    ) : (
                      <span className="muted">—</span>
                    )}
                  </td>
                  <td>
                    {turn.error ? (
                      /* Naming the failure matters: a rate limit and a server
                         error call for completely different responses. */
                      <span className="pill err" title={turn.error}>
                        {turn.status_code ? `${turn.status_code} ` : ""}
                        {turn.error}
                      </span>
                    ) : turn.pending ? (
                      <span className="pill">pending</span>
                    ) : (
                      <span className="muted">{turn.stop_reason || "—"}</span>
                    )}
                  </td>
                </tr>
                {open && (
                  <tr key={`${turn.turn_id}-detail`}>
                    <td
                      colSpan={12}
                      style={{
                        background: "var(--surface-2)",
                        whiteSpace: "normal",
                      }}
                    >
                      <TurnDetail turn={turn} />
                    </td>
                  </tr>
                )}
              </>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function TurnDetail({ turn }: { turn: Turn }) {
  const [payload, setPayload] = useState<{
    label: string;
    body: string;
  } | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Payloads live in the blob store and are fetched only on demand — the whole
  // reason the event records stay narrow.
  async function load(ref: string, label: string) {
    setLoading(true);
    setError(null);
    try {
      const body = await api.blob(ref);
      setPayload({ label, body: JSON.stringify(body, null, 2) });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }

  return (
    <div style={{ padding: "4px 0 8px" }}>
      <div
        style={{
          display: "flex",
          gap: 18,
          flexWrap: "wrap",
          marginBottom: 8,
          fontSize: 12.5,
        }}
      >
        <span className="muted">
          turn <span className="mono">{turn.turn_id}</span>
        </span>
        <span className="muted">
          messages sent:{" "}
          <strong style={{ color: "var(--text-primary)" }}>
            {turn.message_count}
          </strong>
        </span>
        <span className="muted">
          system blocks:{" "}
          <strong style={{ color: "var(--text-primary)" }}>
            {turn.system_blocks}
          </strong>
        </span>
        <span className="muted">{turn.stream ? "streamed" : "buffered"}</span>
        {turn.tools_offered?.length ? (
          <span className="muted">
            tools offered: {turn.tools_offered.join(", ")}
          </span>
        ) : null}
      </div>

      {turn.error && (
        <div className="error-banner" style={{ marginBottom: 8 }}>
          {turn.error}
        </div>
      )}

      <div style={{ display: "flex", gap: 8 }}>
        {turn.request_blob && (
          <button
            className="icon-btn"
            onClick={() => load(turn.request_blob!, "Request")}
          >
            View request
          </button>
        )}
        {turn.response_blob && (
          <button
            className="icon-btn"
            onClick={() => load(turn.response_blob!, "Response")}
          >
            View response
          </button>
        )}
        {!turn.request_blob && !turn.response_blob && (
          <span className="muted" style={{ fontSize: 12.5 }}>
            No payload stored for this turn.
          </span>
        )}
      </div>

      {loading && (
        <div className="muted" style={{ marginTop: 8 }}>
          Loading payload…
        </div>
      )}
      {error && (
        <div className="error-banner" style={{ marginTop: 8 }}>
          {error}
        </div>
      )}
      {payload && (
        <>
          <div style={{ marginTop: 10, fontSize: 12, fontWeight: 600 }}>
            {payload.label}
          </div>
          <pre className="payload mono">{payload.body}</pre>
        </>
      )}
    </div>
  );
}
