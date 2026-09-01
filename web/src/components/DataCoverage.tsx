import { useState } from "react";

import type { Coverage, DayActivity, Health } from "../api";
import { usd } from "../format";
import { ActivityGrid } from "./ActivityGrid";
import { CardHead, Disclosure, Stat } from "./ui";

/** Field labels, for the gaps a reader might otherwise read as zeros. */
const FIELDS: Record<string, string> = {
  thinking_tokens: "Thinking tokens",
  cache_creation_ttl_split: "Cache TTL split",
  tool_calls: "Tool calls",
};

function fieldLabel(name: string) {
  return FIELDS[name] ?? name.replace(/_/g, " ");
}

/** A share, phrased so small ones read as small. */
function share(part: number, whole: number): string {
  if (whole === 0) return "—";
  const pct = (part / whole) * 100;
  if (pct === 0) return "0%";
  if (pct < 0.1) return "<0.1%";
  if (part < whole && pct > 99.9) return ">99.9%";
  return `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
}

/** Day keys are UTC calendar days; reading them back in local time would move
 *  the boundary for anyone not on Greenwich. */
function parseDay(key: string): Date {
  const [y, m, d] = key.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d));
}

function todayKey(): string {
  return new Date().toISOString().slice(0, 10);
}

function shortDate(key: string, withYear = false): string {
  return parseDay(key).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: withYear ? "numeric" : undefined,
    timeZone: "UTC",
  });
}

/** A range, carrying years only when it crosses one — "Sep 2 – Sep 1" is a
 *  year-long window that reads like a typo. */
function dateRange(from: string, to: string): string {
  const crossesYear = from.slice(0, 4) !== to.slice(0, 4);
  return `${shortDate(from, crossesYear)} – ${shortDate(to, crossesYear)}`;
}

/** Whole calendar days between two day keys, inclusive of both ends. */
function spanOf(from: string, to: string): number {
  return Math.round((Date.parse(to) - Date.parse(from)) / 86_400_000) + 1;
}

/**
 * What the selected window actually holds.
 *
 * The dashboard totals a window; it cannot show that the window has holes in
 * it. A run of quiet days between two heavy ones is a fact about how the agent
 * was used, and it only appears if the days are drawn one at a time — so this
 * panel is the calendar plus the figures that describe it, and the coverage
 * caveats move behind a disclosure where they belong.
 */
export function DataCoverage({
  health,
  days: windowDays,
}: {
  health: Health;
  /** The selected window, in days. The calendar follows it. */
  days: number;
}) {
  const [open, setOpen] = useState(false);

  const all = health.days ?? [];
  const sources = Object.entries(health.coverage ?? {}).filter(
    ([, c]) => c.parsed > 0,
  );
  if (all.length === 0 && sources.length === 0) return null;

  // The window runs back from today, not from the last captured day: a week
  // with nothing in its last three days is still a week, and shifting the
  // window to end at the newest data would hide exactly that.
  const today = todayKey();
  const earliest = all.length > 0 ? all[0].day : today;
  const windowStart = new Date(Date.parse(today) - (windowDays - 1) * 86_400_000)
    .toISOString()
    .slice(0, 10);

  // The window is drawn in full, not trimmed to where the data happens to
  // start: a year view that renders as a month is not a year view. Days before
  // the archive begins are drawn as their own state rather than as quiet ones —
  // "nothing was recorded" and "nothing could have been recorded" are different
  // claims, and only the first says anything about how the agent was used.
  const from = windowStart;
  const to = today < earliest ? earliest : today;

  const shown: DayActivity[] = all.filter((d) => d.day >= from && d.day <= to);
  const turns = shown.reduce((sum, d) => sum + d.turns, 0);
  const cost = shown.reduce((sum, d) => sum + d.cost_usd, 0);
  const busiest = shown.reduce<DayActivity | null>(
    (best, d) => (best === null || d.turns > best.turns ? d : best),
    null,
  );
  const spanDays = spanOf(from, to);
  const clamped = windowStart < earliest;

  // Coverage counters describe the whole archive rather than the window: they
  // are a running total of everything ever ingested, so they are reported as
  // records, kept out of the headline figures, and left behind the disclosure.
  const missing = new Map<string, number>();
  const unknown = new Map<string, number>();
  const versions = new Map<string, number>();
  let parsed = 0;
  let unreadable = 0;

  for (const [, c] of sources as [string, Coverage][]) {
    parsed += c.parsed;
    unreadable += c.unreadable;
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
  const considered = parsed + unreadable;
  const hasDetail =
    versions.size > 0 || missing.size > 0 || unknown.size > 0 || unreadable > 0;

  return (
    <div className="card" style={{ marginBottom: 14 }}>
      <CardHead
        title="What's captured"
        meta={`${dateRange(from, to)}${clamped ? ` · captured from ${shortDate(earliest)}` : ""}`}
        action={
          hasDetail ? (
            <Disclosure
              open={open}
              onToggle={() => setOpen(!open)}
              controls="coverage-detail"
              label={open ? "Less" : "Coverage"}
            />
          ) : undefined
        }
      />

      <div className="stats">
        <Stat label="Turns" value={turns.toLocaleString()} />
        <Stat label="Spend" value={usd(cost)} />
        <Stat
          label="Active days"
          value={shown.length.toLocaleString()}
          note={
            clamped ? `of ${spanOf(earliest, to)} captured` : `of ${spanDays}`
          }
        />
        <Stat
          label="Busiest day"
          value={busiest ? shortDate(busiest.day) : "—"}
          note={busiest ? `${busiest.turns.toLocaleString()} turns` : undefined}
        />
        <Stat
          label="Tool builds"
          value={versions.size ? versions.size.toLocaleString() : "—"}
        />
      </div>

      {shown.length > 0 && (
        <ActivityGrid days={shown} from={from} to={to} since={earliest} />
      )}

      {open && (
        <div className="cov-detail" id="coverage-detail">
          {versions.size > 0 && (
            <div className="cov-row">
              <div className="cov-row-head">
                <strong>Records by tool build</strong>
                <span className="muted">
                  {versions.size} builds · whole archive
                </span>
              </div>
              <div className="cov-versions">
                {[...versions.entries()]
                  .sort((a, b) =>
                    a[0].localeCompare(b[0], undefined, { numeric: true }),
                  )
                  .map(([v, n]) => (
                    <span className="pill" key={v} title={`${n} records`}>
                      {v} · {n.toLocaleString()}
                    </span>
                  ))}
              </div>
            </div>
          )}

          {[...missing.entries()]
            .sort((a, b) => b[1] - a[1])
            .map(([name, n]) => (
              <div className="cov-row" key={name}>
                <div className="cov-row-head">
                  <strong>{fieldLabel(name)} not reported</strong>
                  <span className="muted">
                    {n.toLocaleString()} of {parsed.toLocaleString()} records (
                    {share(n, parsed)}) · stored absent, not zero
                  </span>
                </div>
              </div>
            ))}

          {unknown.size > 0 && (
            <div className="cov-row">
              <div className="cov-row-head">
                <strong>Unread fields in the logs</strong>
                <span className="muted">{[...unknown.keys()].join(", ")}</span>
              </div>
            </div>
          )}

          {unreadable > 0 && (
            <div className="cov-row">
              <div className="cov-row-head">
                <strong>Unreadable records</strong>
                <span className="muted">
                  {unreadable.toLocaleString()} of {considered.toLocaleString()}{" "}
                  ({share(unreadable, considered)}) · counted nowhere
                </span>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
