import type { ReactNode } from "react";

/** A right-pointing chevron that rotates when its disclosure opens. */
export function Chevron() {
  return (
    <svg className="chev" viewBox="0 0 12 12" fill="none" aria-hidden="true">
      <path
        d="M4.5 2.5L8 6l-3.5 3.5"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

/**
 * A disclosure control. The chevron carries the open/closed state, so the label
 * stays a stable noun instead of flipping between "show" and "hide" — a moving
 * label makes the control feel like it does two different things.
 */
export function Disclosure({
  open,
  onToggle,
  label,
  controls,
}: {
  open: boolean;
  onToggle: () => void;
  label?: string;
  controls?: string;
}) {
  return (
    <button
      type="button"
      className={`disclosure${label ? "" : " icon-only"}`}
      aria-expanded={open}
      aria-controls={controls}
      onClick={onToggle}
    >
      {label && <span>{label}</span>}
      <Chevron />
    </button>
  );
}

/** The shared panel header: title, a quiet meta line, and an optional control. */
export function CardHead({
  title,
  meta,
  action,
}: {
  title: string;
  meta?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="card-head">
      <h2 className="card-title">{title}</h2>
      {meta && <span className="card-meta">{meta}</span>}
      <div className="spacer" />
      {action}
    </div>
  );
}

/**
 * One horizontal magnitude bar: label, track, value on a shared axis.
 *
 * The default fill is neutral. These bars rank nominal things — tool names,
 * model names — where colour carries no information, because length already
 * encodes the magnitude. Colouring them by value would double-encode it, and a
 * saturated hue would compete with the one chart on the page whose colour is
 * meaningful. Callers pass an explicit colour only where it means something.
 */
export function BarRow({
  label,
  value,
  max,
  display,
  color = "var(--bar-neutral)",
  title,
}: {
  label: string;
  value: number;
  max: number;
  display: string;
  color?: string;
  title?: string;
}) {
  // A non-zero value always shows a sliver, so "small" never reads as "none".
  const pct = max > 0 ? Math.max(value > 0 ? 1.5 : 0, (value / max) * 100) : 0;
  return (
    <div className="bar-row" title={title ?? label}>
      <div className="bar-label">{label}</div>
      <div className="bar-track">
        <div
          className="bar-fill"
          style={{ width: `${pct}%`, background: color }}
        />
      </div>
      <div className="bar-value">{display}</div>
    </div>
  );
}

/**
 * The search box used above a history list.
 *
 * One input, not a row of dropdowns. The searchable space here is small and
 * well-typed — project, branch, model, tool, id — so a person can hold it in
 * their head, and typing `project:foo` is faster than opening a menu. The hint
 * text carries the syntax so it never has to be remembered.
 */
export function SearchBox({
  value,
  onChange,
  placeholder,
  hint,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder: string;
  hint?: string;
}) {
  return (
    <div className="search">
      <svg className="search-icon" viewBox="0 0 14 14" aria-hidden="true">
        <circle
          cx="6"
          cy="6"
          r="4.2"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
        />
        <path
          d="M9.2 9.2L12.5 12.5"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
      <input
        type="search"
        value={value}
        placeholder={placeholder}
        aria-label={placeholder}
        title={hint}
        onChange={(e) => onChange(e.target.value)}
      />
      {value && (
        <button
          type="button"
          className="search-clear"
          aria-label="Clear search"
          onClick={() => onChange("")}
        >
          ×
        </button>
      )}
    </div>
  );
}
