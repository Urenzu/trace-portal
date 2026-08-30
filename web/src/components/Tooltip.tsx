import type { ReactNode } from "react";

export interface TooltipRow {
  label: string;
  value: string;
  /** CSS color for the identity swatch; omitted rows show no swatch. */
  color?: string;
}

interface Props {
  x: number;
  y: number;
  title: string;
  subtitle?: string;
  rows: TooltipRow[];
  footer?: ReactNode;
}

/**
 * A fixed-position tooltip that flips before it runs off the viewport, so a
 * mark near the right edge is still readable.
 */
export function Tooltip({ x, y, title, subtitle, rows, footer }: Props) {
  const width = 230;
  const flip = x + width + 24 > window.innerWidth;
  const style = {
    left: flip ? undefined : x + 14,
    right: flip ? window.innerWidth - x + 14 : undefined,
    top: Math.min(y + 14, window.innerHeight - 200),
  };

  return (
    <div className="tooltip" style={style} role="tooltip">
      <div className="tooltip-title">{title}</div>
      {subtitle && (
        <div
          className="muted"
          style={{ marginTop: -4, marginBottom: 6, fontSize: 11.5 }}
        >
          {subtitle}
        </div>
      )}
      {rows.map((row) => (
        <div className="tooltip-row" key={row.label}>
          <span className="k">
            {row.color && (
              <span className="swatch" style={{ background: row.color }} />
            )}
            {row.label}
          </span>
          <span className="num" style={{ color: "var(--text-primary)" }}>
            {row.value}
          </span>
        </div>
      ))}
      {footer}
    </div>
  );
}
