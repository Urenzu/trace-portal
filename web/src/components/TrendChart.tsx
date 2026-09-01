import { useEffect, useMemo, useRef, useState } from "react";
import { scaleLinear, scaleTime } from "d3-scale";

import type { DayPoint } from "../api";
import { pct, tokens, usd } from "../format";
import { Tooltip } from "./Tooltip";

/**
 * Spend over time.
 *
 * Every other panel is a total, and a total cannot answer the question people
 * actually have about cost: is it going up. The day rollups already hold this
 * shape, so the trend costs one row per day rather than a scan of turns.
 *
 * One series, so no legend — the title names it — and one axis, so the cache
 * hit rate that explains a spike lives in the tooltip rather than on a second
 * y-scale. Height carries the magnitude, so the bars take the single hue that
 * means volume elsewhere in this interface rather than a colour of their own.
 */

const MARGIN = { top: 10, right: 16, bottom: 26, left: 56 };
const HEIGHT = 190;
const MAX_BAR = 30;
const MIN_BAR = 2;
/** Above this many days a bar is thinner than its own hover target. */
const WEEKLY_ABOVE = 120;

interface Bar {
  key: string;
  at: Date;
  from: Date;
  to: Date;
  days: number;
  costUSD: number;
  turns: number;
  sessions: number;
  errors: number;
  cacheRead: number;
  input: number;
  write: number;
}

function parseDay(key: string): Date {
  const [y, m, d] = key.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d));
}

function dayKey(date: Date): string {
  return date.toISOString().slice(0, 10);
}

function empty(at: Date, to: Date, key: string): Bar {
  return {
    key,
    at,
    from: at,
    to,
    days: 0,
    costUSD: 0,
    turns: 0,
    sessions: 0,
    errors: 0,
    cacheRead: 0,
    input: 0,
    write: 0,
  };
}

function fold(bar: Bar, p: DayPoint) {
  bar.days += 1;
  bar.costUSD += p.cost_usd;
  bar.turns += p.turns;
  bar.sessions += p.sessions;
  bar.errors += p.errors;
  bar.cacheRead += p.cache_read;
  bar.input += p.input;
  bar.write += p.write;
}

function hitRate(bar: Bar): number {
  const total = bar.cacheRead + bar.input + bar.write;
  return total > 0 ? bar.cacheRead / total : 0;
}

/**
 * Days, or weeks once there are too many days to hover.
 *
 * Empty spans are kept rather than dropped: a week with no spend is the fact
 * worth seeing, and a chart that closes its gaps turns an idle fortnight into a
 * continuous run of work.
 */
function toBars(points: DayPoint[], from: string, to: string): Bar[] {
  const byDay = new Map(points.map((p) => [p.day, p]));
  const start = parseDay(from);
  const end = parseDay(to);
  const span = Math.round((end.getTime() - start.getTime()) / 86_400_000) + 1;
  const weekly = span > WEEKLY_ABOVE;

  const bars: Bar[] = [];
  let current: Bar | null = null;

  for (let i = 0; i < span; i++) {
    const date = new Date(start.getTime() + i * 86_400_000);
    if (!weekly) {
      const bar = empty(date, date, dayKey(date));
      const point = byDay.get(dayKey(date));
      if (point) fold(bar, point);
      bars.push(bar);
      continue;
    }
    // Weeks break on Sunday so the buckets line up with the calendar above.
    if (current === null || date.getUTCDay() === 0) {
      current = empty(date, date, dayKey(date));
      bars.push(current);
    }
    current.to = date;
    const point = byDay.get(dayKey(date));
    if (point) fold(current, point);
  }
  return bars;
}

function spanLabel(bar: Bar, weekly: boolean): string {
  const one = (d: Date) =>
    d.toLocaleDateString(undefined, {
      weekday: weekly ? undefined : "short",
      month: "short",
      day: "numeric",
      timeZone: "UTC",
    });
  return weekly && bar.from.getTime() !== bar.to.getTime()
    ? `${one(bar.from)} – ${one(bar.to)}`
    : one(bar.from);
}

export function TrendChart({
  points,
  from,
  to,
}: {
  points: DayPoint[];
  /** The window to draw, as YYYY-MM-DD. Days with no spend are still days. */
  from: string;
  to: string;
}) {
  const [hover, setHover] = useState<{ bar: Bar; x: number; y: number } | null>(
    null,
  );
  const [width, setWidth] = useState(880);
  const containerRef = useRef<HTMLDivElement>(null);

  // Measured after commit rather than from a ref callback: setting state inside
  // a ref callback runs during commit and re-triggers the callback.
  useEffect(() => {
    const node = containerRef.current;
    if (!node) return;
    const observer = new ResizeObserver((entries) => {
      const measured = entries[0]?.contentRect.width;
      if (measured && measured > 0) {
        setWidth((prev) => (Math.abs(prev - measured) > 1 ? measured : prev));
      }
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const bars = useMemo(() => toBars(points, from, to), [points, from, to]);
  const weekly = bars.length > 0 && bars[0].days > 1;

  const plotWidth = Math.max(240, width - MARGIN.left - MARGIN.right);
  const plotHeight = HEIGHT - MARGIN.top - MARGIN.bottom;

  const { xScale, yScale, barWidth, yTicks, xTicks } = useMemo(() => {
    const first = bars.length > 0 ? bars[0].at.getTime() : Date.now();
    const last =
      bars.length > 0 ? bars[bars.length - 1].to.getTime() : Date.now();
    const step = Math.max(
      (last - first) / Math.max(bars.length - 1, 1),
      86_400_000,
    );

    const x = scaleTime()
      .domain([first - step / 2, last + step / 2])
      .range([0, plotWidth]);
    const maxCost = Math.max(...bars.map((b) => b.costUSD), 0.01);
    const y = scaleLinear().domain([0, maxCost]).nice().range([plotHeight, 0]);

    const slot = plotWidth / Math.max(bars.length, 1);

    // Labels are taken from the bars, not from the scale's own ticks. The days
    // are UTC and d3's time ticks are local, so tick positions drift by the
    // reader's offset — enough to sit a label visibly between two bars.
    const room = Math.max(2, Math.floor(plotWidth / 110));
    const every = Math.max(1, Math.ceil(bars.length / room));

    return {
      xScale: x,
      yScale: y,
      // A 2px surface gap between neighbours, then clamped to the thin-mark
      // range so a short window does not draw slabs.
      barWidth: Math.max(MIN_BAR, Math.min(MAX_BAR, slot - 2)),
      yTicks: y.ticks(3),
      xTicks: bars.filter((_, i) => i % every === 0),
    };
  }, [bars, plotWidth, plotHeight]);

  if (points.length === 0) {
    return <div className="empty">No spend in this window.</div>;
  }

  const total = bars.reduce((sum, b) => sum + b.costUSD, 0);

  return (
    <div ref={containerRef} className="chart">
      <svg
        width={width}
        height={HEIGHT}
        role="img"
        aria-label={`Spend per ${weekly ? "week" : "day"}, ${from} to ${to}, ${usd(total)} in total`}
        onMouseLeave={() => setHover(null)}
      >
        <g transform={`translate(${MARGIN.left},${MARGIN.top})`}>
          {yTicks.map((t) => (
            <g key={t} transform={`translate(0,${yScale(t)})`}>
              <line x2={plotWidth} className="gridline" />
              <text x={-10} dy="0.32em" className="axis-label" textAnchor="end">
                {usd(t)}
              </text>
            </g>
          ))}

          {bars.map((bar) => {
            const x = xScale(bar.at) - barWidth / 2;
            const top = yScale(bar.costUSD);
            const height = Math.max(0, plotHeight - top);
            return (
              <g key={bar.key}>
                {/* The hit target is the full column height, so a cheap day is
                    as easy to inspect as an expensive one. */}
                <rect
                  x={x}
                  y={0}
                  width={Math.max(barWidth, 4)}
                  height={plotHeight}
                  fill="transparent"
                  onMouseMove={(e) =>
                    setHover({ bar, x: e.clientX, y: e.clientY })
                  }
                  onMouseEnter={(e) =>
                    setHover({ bar, x: e.clientX, y: e.clientY })
                  }
                />
                {bar.costUSD > 0 && (
                  <rect
                    x={x}
                    y={top}
                    width={barWidth}
                    height={height}
                    rx={Math.min(4, barWidth / 2)}
                    className={
                      hover?.bar.key === bar.key ? "trend-bar on" : "trend-bar"
                    }
                    pointerEvents="none"
                  />
                )}
              </g>
            );
          })}

          <line
            y1={plotHeight}
            y2={plotHeight}
            x2={plotWidth}
            className="gridline"
          />
          {xTicks.map((bar) => (
            <text
              key={bar.key}
              x={xScale(bar.at)}
              y={plotHeight + 16}
              className="axis-label"
              textAnchor="middle"
            >
              {bar.from.toLocaleDateString(undefined, {
                month: "short",
                day: "numeric",
                timeZone: "UTC",
              })}
            </text>
          ))}
        </g>
      </svg>

      {hover && (
        <Tooltip
          x={hover.x}
          y={hover.y}
          title={spanLabel(hover.bar, weekly)}
          subtitle={hover.bar.turns === 0 ? "nothing recorded" : undefined}
          rows={
            hover.bar.turns === 0
              ? []
              : [
                  { label: "Spend", value: usd(hover.bar.costUSD) },
                  { label: "Turns", value: hover.bar.turns.toLocaleString() },
                  {
                    label: "Sessions",
                    value: hover.bar.sessions.toLocaleString(),
                  },
                  { label: "Cache hit", value: pct(hitRate(hover.bar)) },
                  {
                    label: "Tokens",
                    value: tokens(
                      hover.bar.cacheRead + hover.bar.input + hover.bar.write,
                    ),
                  },
                  ...(hover.bar.errors > 0
                    ? [
                        {
                          label: "Errors",
                          value: hover.bar.errors.toLocaleString(),
                        },
                      ]
                    : []),
                ]
          }
        />
      )}
    </div>
  );
}
