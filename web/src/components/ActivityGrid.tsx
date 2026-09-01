import { useState, type CSSProperties, type MouseEvent } from "react";

import type { DayActivity } from "../api";
import { usd } from "../format";
import { Tooltip } from "./Tooltip";

/**
 * A calendar of what the selected window holds, one cell per day.
 *
 * A first and last date read as continuous coverage, and they rarely are. The
 * gap between the days a window spans and the days in it that hold anything is
 * the most concrete thing this tool can say about how the agent gets used, and
 * no total can show it — a run of quiet days between two heavy ones is only
 * visible if the days are drawn individually.
 *
 * Two layouts, chosen by span. Up to a fortnight the days run left to right
 * with their dates on them; beyond that weeks run down the columns, so the
 * block keeps its height from a month to a year and the weekday rhythm reads
 * straight off the rows.
 */

const WEEKDAYS = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const MONTHS = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

/** Day keys are UTC calendar days, so they are read back in UTC. Going through
 *  the local timezone would shift a day for anyone west of Greenwich. */
function parseDay(key: string): Date {
  const [y, m, d] = key.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d));
}

function keyOf(date: Date): string {
  return date.toISOString().slice(0, 10);
}

function label(date: Date): string {
  return `${WEEKDAYS[date.getUTCDay()]}, ${MONTHS[date.getUTCMonth()]} ${date.getUTCDate()}`;
}

/**
 * Four steps, on a square-root scale.
 *
 * Turn counts per day span two orders of magnitude — six turns one day, 780 the
 * next. Linear steps would leave every ordinary day in the lightest bin and say
 * only "one day was busy"; the square root keeps a 150-turn day visibly
 * different from a 6-turn one while the busiest still reads as the darkest.
 */
function level(turns: number, max: number): number {
  if (turns <= 0) return 0;
  return Math.min(4, Math.max(1, Math.ceil(Math.sqrt(turns / max) * 4)));
}

interface Hover {
  x: number;
  y: number;
  date: Date;
  day?: DayActivity;
}

export function ActivityGrid({
  days,
  from,
  to,
  since,
}: {
  days: DayActivity[];
  /** First and last day of the span to draw, as YYYY-MM-DD. The span comes from
   *  the selected window rather than from the data, so a week whose last three
   *  days are empty still draws as a week, and a year draws as a year. */
  from: string;
  to: string;
  /** First day the archive covers. Anything before it had no chance of being
   *  recorded, which is a different claim from a day that simply holds nothing,
   *  so those cells get their own state. */
  since?: string;
}) {
  // The per-day figures are the whole reason to draw this, so they get the same
  // tooltip the timeline uses rather than a native title: a second's hover delay
  // on a small square is long enough that most people never see it.
  const [hover, setHover] = useState<Hover | null>(null);

  const byDay = new Map(days.map((d) => [d.day, d]));
  const first = parseDay(from);
  const last = parseDay(to);
  if (last < first) return null;

  const max = Math.max(...days.map((d) => d.turns), 1);
  const total = Math.max(
    days.reduce((sum, d) => sum + d.turns, 0),
    1,
  );

  // One place decides what a cell is, so the two layouts cannot disagree.
  const covered = (key: string) => since === undefined || key >= since;
  const classOf = (key: string, turns: number) =>
    covered(key) ? `cell l${level(turns, max)}` : "cell none";
  const spanDays =
    Math.round((last.getTime() - first.getTime()) / 86_400_000) + 1;

  // Enter opens the tooltip; move keeps it under the pointer and re-reads the
  // day, so a fast drag across a row cannot leave the wrong date on screen.
  const shower =
    (date: Date, day?: DayActivity) => (e: MouseEvent<HTMLElement>) =>
      setHover({ x: e.clientX, y: e.clientY, date, day });

  const tooltip = hover && (
    <Tooltip
      x={hover.x}
      y={hover.y}
      title={label(hover.date)}
      subtitle={
        hover.day
          ? undefined
          : covered(keyOf(hover.date))
            ? "nothing recorded"
            : "before this archive begins"
      }
      rows={
        hover.day
          ? [
              { label: "Turns", value: hover.day.turns.toLocaleString() },
              { label: "Spend", value: usd(hover.day.cost_usd) },
              {
                label: "Share of window",
                value: `${((hover.day.turns / total) * 100).toFixed(1)}%`,
              },
            ]
          : []
      }
    />
  );

  const key = (
    <div className="activity-key">
      <span>Quiet</span>
      {[0, 1, 2, 3, 4].map((l) => (
        <span key={l} className={`cell l${l}`} />
      ))}
      <span>{max.toLocaleString()} turns</span>
      {since !== undefined && from < since && (
        <>
          <span className="cell none" style={{ marginLeft: 12 }} />
          <span>none before {label(parseDay(since))}</span>
        </>
      )}
    </div>
  );

  if (spanDays <= 14) {
    const strip: Date[] = [];
    for (
      let d = new Date(first);
      d <= last;
      d = new Date(d.getTime() + 86_400_000)
    ) {
      strip.push(d);
    }

    return (
      <div className="activity" style={{ "--cell": "34px" } as CSSProperties}>
        <div
          className="activity-strip"
          role="img"
          aria-label={`Turns per day, ${label(first)} to ${label(last)}`}
          onMouseLeave={() => setHover(null)}
        >
          {strip.map((date) => {
            const day = byDay.get(keyOf(date));
            const show = shower(date, day);
            return (
              <div className="strip-day" key={keyOf(date)}>
                <span
                  className={classOf(keyOf(date), day?.turns ?? 0)}
                  onMouseEnter={show}
                  onMouseMove={show}
                />
                <span className="strip-label">
                  {WEEKDAYS[date.getUTCDay()][0]}
                  <em>{date.getUTCDate()}</em>
                </span>
              </div>
            );
          })}
        </div>
        {key}
        {tooltip}
      </div>
    );
  }

  // Pad out to whole weeks so every column is a full one and the weekday rows
  // stay aligned; the padding cells are drawn as holes, not as empty days.
  const start = new Date(first);
  start.setUTCDate(start.getUTCDate() - start.getUTCDay());
  const end = new Date(last);
  end.setUTCDate(end.getUTCDate() + (6 - end.getUTCDay()));

  const cells: { key: string; date: Date; inSpan: boolean }[] = [];
  for (
    let d = new Date(start);
    d <= end;
    d = new Date(d.getTime() + 86_400_000)
  ) {
    cells.push({ key: keyOf(d), date: d, inSpan: d >= first && d <= last });
  }

  const weeks = cells.length / 7;

  // A month drawn at a year's cell size is a postage stamp in a wide panel, and
  // nobody reads a postage stamp. The cell grows when there is room and shrinks
  // once a long history needs it.
  const cell = weeks <= 10 ? 24 : weeks <= 26 ? 16 : 12;

  // One label per month, over the column its first day in span falls in.
  const months: { column: number; name: string }[] = [];
  cells.forEach((c, i) => {
    if (!c.inSpan) return;
    const name = MONTHS[c.date.getUTCMonth()];
    if (months.length === 0 || months[months.length - 1].name !== name) {
      months.push({ column: Math.floor(i / 7) + 1, name });
    }
  });

  return (
    <div className="activity" style={{ "--cell": `${cell}px` } as CSSProperties}>
      <div className="activity-scroll">
        <div
          className="activity-months"
          style={{ gridTemplateColumns: `repeat(${weeks}, var(--cell))` }}
        >
          {months.map((m) => (
            <span key={m.name + m.column} style={{ gridColumn: m.column }}>
              {m.name}
            </span>
          ))}
        </div>

        <div className="activity-body">
          <div className="activity-weekdays" aria-hidden="true">
            {WEEKDAYS.map((name, i) => (
              <span key={name}>{i % 2 === 1 ? name : ""}</span>
            ))}
          </div>

          <div
            className="activity-cells"
            style={{ gridTemplateColumns: `repeat(${weeks}, var(--cell))` }}
            role="img"
            aria-label={`Turns per day, ${label(first)} to ${label(last)}`}
            onMouseLeave={() => setHover(null)}
          >
            {cells.map((c) => {
              if (!c.inSpan) {
                return <span key={c.key} className="cell out" />;
              }
              const day = byDay.get(c.key);
              const show = shower(c.date, day);
              return (
                <span
                  key={c.key}
                  className={classOf(c.key, day?.turns ?? 0)}
                  onMouseEnter={show}
                  onMouseMove={show}
                />
              );
            })}
          </div>
        </div>
      </div>

      {key}
      {tooltip}
    </div>
  );
}
