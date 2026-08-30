import { useState } from "react";

import type { Coverage } from "../api";
import { CardHead, Disclosure } from "./ui";

/** Fields this UI can display, named as a reader would recognise them. */
const FIELD_LABELS: Record<string, string> = {
  thinking_tokens: "Thinking tokens",
  cache_creation_ttl_split: "Cache TTL split",
  tool_calls: "Tool calls",
};

/**
 * Agent log formats move constantly — one machine saw fifteen Claude Code
 * versions and three schema changes in under a month. When a build never
 * reported a field, saying so is the honest thing: a gap shown as zero reads as
 * a measurement and quietly understates whatever it touches.
 *
 * This panel stays quiet when there is nothing to report.
 */
export function DataCoverage({
  coverage,
}: {
  coverage: Record<string, Coverage>;
}) {
  const [open, setOpen] = useState(false);

  const sources = Object.entries(coverage).filter(([, c]) => c.parsed > 0);
  if (sources.length === 0) return null;

  const missing = new Map<string, number>();
  const unknown = new Map<string, number>();
  const versions = new Map<string, number>();
  let parsed = 0;
  let unreadable = 0;

  for (const [, c] of sources) {
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

  const hasGaps = missing.size > 0 || unknown.size > 0 || unreadable > 0;
  if (!hasGaps && versions.size <= 1) return null;

  return (
    <div className="card" style={{ marginBottom: 14 }}>
      <CardHead
        title="Data coverage"
        meta={`${parsed.toLocaleString()} turns · ${versions.size} tool ${
          versions.size === 1 ? "version" : "versions"
        }`}
        action={
          <Disclosure
            open={open}
            onToggle={() => setOpen(!open)}
            controls="coverage-detail"
          />
        }
      />

      <div className="cov-facts">
        {[...missing.entries()].map(([field, n]) => (
          <span className="fact warn" key={field}>
            <span className="dot" />
            <span>
              <strong>{n.toLocaleString()}</strong> turns without{" "}
              {(FIELD_LABELS[field] ?? field).toLowerCase()}
            </span>
          </span>
        ))}
        {unknown.size > 0 && (
          <span className="fact">
            <span className="dot" />
            <span>
              <strong>{unknown.size}</strong> unread field
              {unknown.size === 1 ? "" : "s"} in the logs
            </span>
          </span>
        )}
        {unreadable > 0 && (
          <span className="fact">
            <span className="dot" />
            <span>
              <strong>{unreadable.toLocaleString()}</strong> records not
              decodable
            </span>
          </span>
        )}
      </div>

      {open && (
        <div className="cov-grid" id="coverage-detail">
          <Column
            title="Turns by tool version"
            entries={[...versions.entries()].sort((a, b) =>
              a[0].localeCompare(b[0], undefined, { numeric: true }),
            )}
          />
          {missing.size > 0 && (
            <Column
              title="Not reported by that build"
              entries={[...missing.entries()].map(
                ([k, v]) => [FIELD_LABELS[k] ?? k, v] as [string, number],
              )}
              note="Excluded from totals rather than counted as zero."
            />
          )}
          {unknown.size > 0 && (
            <Column
              title="Fields this build ignores"
              entries={[...unknown.entries()].sort((a, b) => b[1] - a[1])}
              note="The format moved; there may be data worth reading here."
            />
          )}
        </div>
      )}
    </div>
  );
}

function Column({
  title,
  entries,
  note,
}: {
  title: string;
  entries: [string, number][];
  note?: string;
}) {
  if (entries.length === 0) return null;
  return (
    <div className="cov-col">
      <h3>{title}</h3>
      <div className="cov-list">
        {entries.map(([label, count]) => (
          <div className="cov-item" key={label}>
            <span title={label}>{label}</span>
            <span>{count.toLocaleString()}</span>
          </div>
        ))}
      </div>
      {note && (
        <p className="card-sub" style={{ marginTop: 8, fontSize: 11.5 }}>
          {note}
        </p>
      )}
    </div>
  );
}
