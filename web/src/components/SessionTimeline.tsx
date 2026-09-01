import { useEffect, useMemo, useRef, useState } from "react";
import { scaleLinear, scaleTime } from "d3-scale";

import type { Turn } from "../api";
import { tokens, usd, clockTime, msOrUnknown, pct } from "../format";
import { Tooltip } from "./Tooltip";
import { daySegments, dayLabel, idleLabel } from "./days";
import {
  bucketTurns,
  bucketHitRate,
  bucketTotal,
  BUCKET_THRESHOLD,
  type Bucket,
} from "./buckets";

/**
 * The four token classes a turn is billed for, bottom to top. Order is fixed:
 * colors follow the class, never its rank in a given session.
 */
const SERIES = [
  { key: "cacheRead", label: "Cache read", color: "var(--series-1)" },
  { key: "cacheWrite", label: "Cache write", color: "var(--series-2)" },
  { key: "input", label: "Fresh input", color: "var(--series-3)" },
  { key: "output", label: "Output", color: "var(--series-4)" },
] as const;

const MARGIN = { top: 10, right: 16, bottom: 28, left: 54 };
const HEIGHT = 260;
/** Thin marks: columns stay narrow so time position stays readable. */
const MAX_BAR = 16;
const MIN_BAR = 2;
/** Surface gap between stacked segments, per the mark spec. */
const GAP = 2;

interface Props {
  turns: Turn[];
  onSelect?: (turn: Turn) => void;
  selectedTurnId?: string;
}

export function SessionTimeline({ turns, onSelect, selectedTurnId }: Props) {
  const [hover, setHover] = useState<{
    bucket: Bucket;
    x: number;
    y: number;
  } | null>(null);
  const [width, setWidth] = useState(880);
  const containerRef = useRef<HTMLDivElement>(null);

  // Measure after commit via ResizeObserver rather than from a ref callback:
  // setting state inside a ref callback runs during commit and re-triggers the
  // callback, which spins.
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

  const plotWidth = Math.max(240, width - MARGIN.left - MARGIN.right);
  const plotHeight = HEIGHT - MARGIN.top - MARGIN.bottom;
  const aggregated = turns.length > BUCKET_THRESHOLD;

  const buckets = useMemo(() => bucketTurns(turns), [turns]);

  // A resumed session is one session with an idle night in the middle of it.
  // The x-axis is time, so that night is drawn to scale and the turns either
  // side of it would otherwise read as one continuous run of work.
  const segments = useMemo(() => daySegments(turns), [turns]);
  const resumes = segments.slice(1);

  const { xScale, yScale, barWidth, yTicks, xTicks } = useMemo(() => {
    const times = buckets.map((b) => b.at.getTime());
    const minT = Math.min(...times);
    const maxT = Math.max(...times);
    // A session with one column, or several at the same instant, still needs a
    // non-zero domain or every mark collapses onto one pixel.
    const pad = Math.max((maxT - minT) * 0.04, 30_000);

    const x = scaleTime()
      .domain([minT - pad, maxT + pad])
      .range([0, plotWidth]);
    const maxTotal = Math.max(1, ...buckets.map(bucketTotal));
    const y = scaleLinear().domain([0, maxTotal]).nice().range([plotHeight, 0]);

    // Size columns off the tightest gap between neighbours so they never
    // overlap, then clamp to the thin-mark range.
    const sorted = [...times].sort((a, b) => a - b);
    let tightest = Infinity;
    for (let i = 1; i < sorted.length; i++) {
      const gap = x(sorted[i]) - x(sorted[i - 1]);
      if (gap > 0.5) tightest = Math.min(tightest, gap);
    }
    const bar = Math.max(
      MIN_BAR,
      Math.min(MAX_BAR, Number.isFinite(tightest) ? tightest * 0.72 : MAX_BAR),
    );

    return {
      xScale: x,
      yScale: y,
      barWidth: bar,
      yTicks: y.ticks(4),
      xTicks: x.ticks(Math.max(2, Math.min(6, Math.floor(plotWidth / 130)))),
    };
  }, [buckets, plotWidth, plotHeight]);

  if (turns.length === 0) {
    return <div className="empty">No turns in this session.</div>;
  }

  return (
    <>
      <div className="legend">
        {SERIES.map((s) => (
          <span className="legend-item" key={s.key}>
            <span className="swatch" style={{ background: s.color }} />
            {s.label}
          </span>
        ))}
        {aggregated && (
          <span className="legend-item muted" style={{ marginLeft: "auto" }}>
            {turns.length.toLocaleString()} turns grouped into {buckets.length}{" "}
            columns
          </span>
        )}
      </div>

      <div ref={containerRef}>
        <svg
          className="chart"
          viewBox={`0 0 ${width} ${HEIGHT}`}
          height={HEIGHT}
          role="img"
          aria-label="Tokens per turn over the session, split by how each token was billed"
          onMouseLeave={() => setHover(null)}
        >
          <g transform={`translate(${MARGIN.left},${MARGIN.top})`}>
            {yTicks.map((tick) => (
              <g key={tick} transform={`translate(0,${yScale(tick)})`}>
                <line className="gridline" x1={0} x2={plotWidth} />
                <text
                  className="axis-label"
                  x={-8}
                  dy="0.32em"
                  textAnchor="end"
                >
                  {tokens(tick)}
                </text>
              </g>
            ))}

            {xTicks.map((tick) => (
              <text
                key={tick.getTime()}
                className="axis-label"
                x={xScale(tick)}
                y={plotHeight + 18}
                textAnchor="middle"
              >
                {/* A bare clock time is ambiguous once the session crosses a
                    day, and the axis is the only thing that says which day a
                    column belongs to. */}
                {resumes.length > 0
                  ? shortDate(tick)
                  : clockTime(tick.toISOString())}
              </text>
            ))}

            {resumes.map((segment) => {
              const at = xScale(segment.from);
              // A resume late in the session sits near the right edge, where a
              // label reading rightwards is clipped by the viewBox. Flip it to
              // the inside rather than letting it run off.
              const flip = at > plotWidth - 96;
              return (
                <g key={segment.key} className="day-break">
                  <line x1={at} x2={at} y1={-4} y2={plotHeight} />
                  <text
                    className="axis-label"
                    x={flip ? at - 4 : at + 4}
                    y={4}
                    textAnchor={flip ? "end" : "start"}
                  >
                    {dayLabel(segment)} · +{idleLabel(segment.gapMS)}
                  </text>
                </g>
              );
            })}

            {buckets.map((bucket) => {
              const cx = xScale(bucket.at);
              const selected =
                selectedTurnId !== undefined &&
                bucket.turns.some((t) => t.turn_id === selectedTurnId);
              const dim = hover !== null && hover.bucket.key !== bucket.key;
              const total = bucketTotal(bucket);

              let cursor = 0; // running total from the baseline up
              return (
                <g
                  key={bucket.key}
                  opacity={dim ? 0.45 : 1}
                  style={{ cursor: onSelect ? "pointer" : "default" }}
                  /* Anchored to the column, set once on enter. Tracking the
                     cursor instead would set state on every mousemove and
                     re-render the whole chart per pixel. */
                  onMouseEnter={(e) => {
                    const box = e.currentTarget.getBoundingClientRect();
                    setHover({
                      bucket,
                      x: box.right,
                      y: box.top + box.height / 3,
                    });
                  }}
                  onClick={() => {
                    // A grouped column has no single turn to select; only an
                    // ungrouped one drills in.
                    if (bucket.turns.length === 1) onSelect?.(bucket.turns[0]);
                  }}
                >
                  {/* A hit target wider than the mark, so thin columns stay easy
                      to hover. Invisible, but must not be display:none. */}
                  <rect
                    x={cx - Math.max(barWidth, 8) / 2}
                    y={0}
                    width={Math.max(barWidth, 8)}
                    height={plotHeight}
                    fill="transparent"
                  />

                  {SERIES.map((s) => {
                    const value = bucket[s.key];
                    if (value <= 0) return null;

                    const yTop = yScale(cursor + value);
                    const yBottom = yScale(cursor);
                    cursor += value;

                    const height = Math.max(1, yBottom - yTop - GAP);
                    return (
                      <rect
                        key={s.key}
                        x={cx - barWidth / 2}
                        y={yTop}
                        width={barWidth}
                        height={height}
                        rx={Math.min(2, barWidth / 3)}
                        fill={s.color}
                      />
                    );
                  })}

                  {selected && (
                    <rect
                      x={cx - barWidth / 2 - 2}
                      y={yScale(total) - 3}
                      width={barWidth + 4}
                      height={plotHeight - yScale(total) + 3}
                      fill="none"
                      stroke="var(--text-primary)"
                      strokeWidth={1.5}
                      rx={3}
                    />
                  )}

                  {bucket.errors > 0 && (
                    <circle
                      cx={cx}
                      cy={plotHeight + 6}
                      r={3}
                      fill="var(--status-critical)"
                    />
                  )}
                </g>
              );
            })}

            <line
              className="gridline"
              x1={0}
              x2={plotWidth}
              y1={plotHeight}
              y2={plotHeight}
            />
          </g>
        </svg>
      </div>

      {hover && (
        <BucketTooltip
          bucket={hover.bucket}
          x={hover.x}
          y={hover.y}
          withDate={resumes.length > 0}
        />
      )}
    </>
  );
}

function stamp(at: Date, withDate: boolean): string {
  return withDate ? shortDate(at) : clockTime(at.toISOString());
}

/** Day and time together, for an axis that spans more than one day. */
function shortDate(at: Date): string {
  return at.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function BucketTooltip({
  bucket,
  x,
  y,
  withDate,
}: {
  bucket: Bucket;
  x: number;
  y: number;
  withDate: boolean;
}) {
  const grouped = bucket.turns.length > 1;
  const turn = bucket.turns[0];

  const rows = [
    ...SERIES.map((s) => ({
      label: s.label,
      value: tokens(bucket[s.key]),
      color: s.color,
    })),
    { label: "Cache hit", value: pct(bucketHitRate(bucket)) },
    { label: "Cost", value: usd(bucket.costUSD) },
  ];
  if (!grouped) {
    rows.push({
      label: "Latency",
      value: `${msOrUnknown(turn.ttfb_ms)} → ${msOrUnknown(turn.duration_ms)}`,
    });
  }

  const toolNames = [
    ...new Set(
      bucket.turns.flatMap((t) => t.tool_calls?.map((c) => c.name) ?? []),
    ),
  ];

  return (
    <Tooltip
      x={x}
      y={y}
      title={
        grouped
          ? `${stamp(bucket.from, withDate)} – ${stamp(bucket.to, withDate)}`
          : stamp(bucket.at, withDate)
      }
      subtitle={
        grouped
          ? `${bucket.turns.length} turns`
          : `${turn.model}${turn.stop_reason ? ` · ${turn.stop_reason}` : ""}`
      }
      rows={rows}
      footer={
        toolNames.length > 0 ? (
          <div
            style={{
              marginTop: 7,
              fontSize: 11.5,
              color: "var(--text-secondary)",
            }}
          >
            Tools: {toolNames.slice(0, 6).join(", ")}
            {toolNames.length > 6 ? ` +${toolNames.length - 6}` : ""}
          </div>
        ) : bucket.errors > 0 ? (
          <div
            style={{
              marginTop: 7,
              fontSize: 11.5,
              color: "var(--status-critical)",
            }}
          >
            {bucket.errors} failed {bucket.errors === 1 ? "turn" : "turns"}
          </div>
        ) : null
      }
    />
  );
}
