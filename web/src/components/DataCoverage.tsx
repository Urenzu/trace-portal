import { useState } from "react";

import type { Coverage, Health } from "../api";
import { CardHead, Disclosure } from "./ui";

/**
 * What a missing field actually costs the reader.
 *
 * A count on its own is not information. "1,815 turns without thinking tokens"
 * invites the question it does not answer — does that make the number above it
 * wrong? Each gap therefore carries the figure it touches and the direction it
 * moves, so the panel can be read as a verdict on the dashboard rather than as
 * trivia about the logs.
 */
const FIELDS: Record<string, { label: string; affects: string }> = {
  thinking_tokens: {
    label: "Thinking tokens",
    affects:
      "Cost and token totals are unaffected — the API bills thinking inside output tokens, which those builds did report.",
  },
  cache_creation_ttl_split: {
    label: "Cache TTL split",
    affects:
      "Cache writes still total correctly; only their split between the 5-minute and 1-hour rate is unknown, so cost for those turns is within 1.25x–2x of base input.",
  },
  tool_calls: {
    label: "Tool calls",
    affects: "Tool-call counts understate for those turns. Cost is unaffected.",
  },
};

function field(name: string) {
  return (
    FIELDS[name] ?? {
      label: name.replace(/_/g, " "),
      affects: "Stored as absent rather than zero, so no total counts it.",
    }
  );
}

/** A share, phrased so small ones read as small. */
function share(part: number, whole: number): string {
  if (whole === 0) return "—";
  const pct = (part / whole) * 100;
  if (pct === 0) return "0%";
  if (pct < 0.1) return "<0.1%";
  // Never round a near-miss to a clean 100%: the same panel says how many
  // records were lost, and "100% read" beside "4 unreadable" reads as a bug.
  if (part < whole && pct > 99.9) return ">99.9%";
  return `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
}

/**
 * Whether the figures on this dashboard can be trusted, and where they cannot.
 *
 * Agent log formats move constantly — one machine saw fifteen Claude Code
 * versions and three schema changes in under a month. A gap shown as zero reads
 * as a measurement and quietly understates whatever it touches, so every gap is
 * reported. The panel stays silent when there is nothing to report.
 */
export function DataCoverage({ health }: { health: Health }) {
  const [open, setOpen] = useState(false);

  const sources = Object.entries(health.coverage ?? {}).filter(
    ([, c]) => c.parsed > 0,
  );
  if (sources.length === 0) return null;

  const missing = new Map<string, number>();
  const unknown = new Map<string, number>();
  const versions = new Map<string, number>();
  let parsed = 0;
  let unreadable = 0;
  let considered = 0;

  for (const [, c] of sources as [string, Coverage][]) {
    parsed += c.parsed;
    unreadable += c.unreadable;
    // Skipped records are the ones that are not turns at all — user messages,
    // attachments, tool results. They are not a gap, so the denominator for
    // "did we read it" is what was actually a candidate.
    considered += c.parsed + c.unreadable;
    for (const [f, n] of Object.entries(c.missing_field ?? {})) {
      missing.set(f, (missing.get(f) ?? 0) + n);
    }
    for (const [f, n] of Object.entries(c.unknown_field ?? {})) {
      unknown.set(f, (unknown.get(f) ?? 0) + n);
    }
    for (const [v, n] of Object.entries(c.by_version ?? {})) {
      versions.set(v, (versions.get(v) ?? 0) + n);
    }
  }

  const hasGaps = missing.size > 0 || unknown.size > 0 || unreadable > 0;
  if (!hasGaps && versions.size <= 1) return null;

  // Anything above a percent of records lost is enough to move a total; below
  // it, saying so plainly is more useful than an alarm.
  const lossy = unreadable / Math.max(considered, 1) > 0.01;
  const costAffected = lossy;

  const span =
    health.first_day && health.last_day
      ? `${health.first_day} to ${health.last_day}`
      : `${health.days_captured} days`;

  return (
    <div className="card" style={{ marginBottom: 14 }}>
      <CardHead
        title="Data coverage"
        meta={`${parsed.toLocaleString()} turns · ${span}`}
        action={
          <Disclosure
            open={open}
            onToggle={() => setOpen(!open)}
            controls="coverage-detail"
            label={open ? "Less" : "What this affects"}
          />
        }
      />

      <p className="card-sub">
        {costAffected ? (
          <>
            <strong>{share(unreadable, considered)}</strong> of turn records
            could not be read, which is enough to make spend and token totals
            understate.
          </>
        ) : (
          <>
            Spend, token and cache figures on this page are complete:{" "}
            <strong>{share(parsed, considered)}</strong> of turn records were
            read, across{" "}
            <strong>
              {versions.size} tool {versions.size === 1 ? "build" : "builds"}
            </strong>
            .{" "}
            {hasGaps
              ? "The gaps below are in fields no total on this page depends on."
              : ""}
          </>
        )}
      </p>

      <div className="cov-facts">
        {[...missing.entries()]
          .sort((a, b) => b[1] - a[1])
          .map(([name, n]) => (
            <span className="fact warn" key={name} title={field(name).affects}>
              <span className="dot" />
              <span>
                {field(name).label} missing on{" "}
                <strong>{share(n, parsed)}</strong> of turns
              </span>
            </span>
          ))}
        {unreadable > 0 && (
          <span
            className={`fact${lossy ? " warn" : ""}`}
            title="These records could not be decoded and are counted nowhere."
          >
            <span className="dot" />
            <span>
              <strong>{unreadable.toLocaleString()}</strong> of{" "}
              {considered.toLocaleString()} records unreadable
            </span>
          </span>
        )}
        {unknown.size > 0 && (
          <span
            className="fact"
            title="Present in the logs and not read here. Nothing on this page depends on them."
          >
            <span className="dot" />
            <span>
              <strong>{unknown.size}</strong> new field
              {unknown.size === 1 ? "" : "s"} appeared in the logs
            </span>
          </span>
        )}
      </div>

      {open && (
        <div className="cov-detail" id="coverage-detail">
          {[...missing.entries()]
            .sort((a, b) => b[1] - a[1])
            .map(([name, n]) => (
              <div className="cov-row" key={name}>
                <div className="cov-row-head">
                  <strong>{field(name).label}</strong>
                  <span className="muted">
                    absent on {n.toLocaleString()} of {parsed.toLocaleString()}{" "}
                    turns ({share(n, parsed)})
                  </span>
                </div>
                <p>
                  The builds that produced those turns never reported it, so it
                  is stored as absent rather than zero. {field(name).affects}
                </p>
              </div>
            ))}

          {unknown.size > 0 && (
            <div className="cov-row">
              <div className="cov-row-head">
                <strong>New fields in the logs</strong>
                <span className="muted">{[...unknown.keys()].join(", ")}</span>
              </div>
              <p>
                These appear in the transcripts and are not read here. Nothing
                on this page depends on them, but the format has moved — this is
                where a new measurement would come from.
              </p>
            </div>
          )}

          {unreadable > 0 && (
            <div className="cov-row">
              <div className="cov-row-head">
                <strong>Unreadable records</strong>
                <span className="muted">
                  {unreadable.toLocaleString()} of {considered.toLocaleString()}{" "}
                  ({share(unreadable, considered)})
                </span>
              </div>
              <p>
                {lossy
                  ? "Enough to move the totals above. Every figure on this page understates by roughly this share."
                  : "Counted nowhere, and too few to move any figure on this page."}
              </p>
            </div>
          )}

          <div className="cov-row">
            <div className="cov-row-head">
              <strong>Turns by tool build</strong>
              <span className="muted">
                {versions.size} {versions.size === 1 ? "build" : "builds"} over{" "}
                {span}
              </span>
            </div>
            <div className="cov-versions">
              {[...versions.entries()]
                .sort((a, b) =>
                  a[0].localeCompare(b[0], undefined, { numeric: true }),
                )
                .map(([v, n]) => (
                  <span className="pill" key={v} title={`${n} turns`}>
                    {v} · {n.toLocaleString()}
                  </span>
                ))}
            </div>
            <p>
              Which build produced each turn is recorded, so a gap can be
              attributed to a version rather than guessed at.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}
