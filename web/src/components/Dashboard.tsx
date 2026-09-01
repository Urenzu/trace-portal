import { useState } from "react";

import type { Stats } from "../api";
import { tokens, usd, pct } from "../format";
import { Projects } from "./Projects";
import { BarRow, CardHead, Disclosure } from "./ui";

/** A stat tile: one number, no plot. A single headline value does not need a
 *  chart to be understood. */
function Tile({
  label,
  value,
  note,
}: {
  label: string;
  value: string;
  note?: string;
}) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value num">{value}</div>
      {note && <div className="tile-note">{note}</div>}
    </div>
  );
}

export function Dashboard({
  stats,
  onOpenProject,
}: {
  stats: Stats;
  onOpenProject?: (projectId: string) => void;
}) {
  const u = stats.usage;
  const totalInput =
    u.input_tokens + u.cache_creation_input_tokens + u.cache_read_input_tokens;

  const models = Object.entries(stats.turns_by_model ?? {}).sort(
    (a, b) => b[1] - a[1],
  );
  const tools = Object.entries(stats.tool_calls_by_name ?? {}).sort(
    (a, b) => b[1] - a[1],
  );

  return (
    <>
      <div className="tiles">
        <Tile
          label="Spend"
          value={usd(stats.cost_usd)}
          /* On a subscription nothing here is billed; it is what the usage
             would cost at API list prices. Saying so beats a number that looks
             like an invoice. */
          note={
            stats.unpriced_turns
              ? `${stats.unpriced_turns} turns unpriced`
              : "at API list prices"
          }
        />
        <Tile
          label="Saved by caching"
          value={usd(stats.savings_usd)}
          note="vs. full input price"
        />
        <Tile
          label="Cache hit rate"
          value={pct(stats.cache_hit_rate)}
          note="of input tokens"
        />
        <Tile
          label="Turns"
          value={stats.turns.toLocaleString()}
          note={`${tokens(totalInput)} input tokens`}
        />
        <Tile
          label="Sessions"
          value={stats.sessions.toLocaleString()}
          /* Rollups count a session once per day it touched, so a session
             spanning midnight inflates this. Say so rather than quietly
             presenting an upper bound as exact. */
          note={stats.sessions_exact ? undefined : "approximate across days"}
        />
        <Tile
          label="Errors"
          value={stats.errors.toLocaleString()}
          note={stats.errors > 0 ? "failed or 4xx/5xx turns" : "none"}
        />
      </div>

      <div className="card">
        <CardHead
          title="Where the tokens went"
          meta={`${tokens(totalInput)} input`}
        />
        <p className="card-sub">
          Cached reads bill at a tenth of the input rate, so a high share here
          is the difference between a cheap agent loop and an expensive one.
        </p>
        <div className="card-body">
          <BarRow
            label="Cache read"
            value={u.cache_read_input_tokens}
            max={totalInput}
            display={tokens(u.cache_read_input_tokens)}
            color="var(--series-1)"
          />
          <BarRow
            label="Cache write"
            value={u.cache_creation_input_tokens}
            max={totalInput}
            display={tokens(u.cache_creation_input_tokens)}
            color="var(--series-2)"
          />
          <BarRow
            label="Fresh input"
            value={u.input_tokens}
            max={totalInput}
            display={tokens(u.input_tokens)}
            color="var(--series-3)"
          />
          <BarRow
            label="Output"
            value={u.output_tokens}
            max={totalInput}
            display={tokens(u.output_tokens)}
            color="var(--series-4)"
          />
        </div>
      </div>

      {stats.projects && stats.projects.length > 0 && (
        <Projects projects={stats.projects} onOpen={onOpenProject} />
      )}

      {models.length > 0 && (
        <RankedBars
          title="Turns by model"
          entries={models}
          unit="turns"
          initial={5}
        />
      )}
      {tools.length > 0 && (
        <RankedBars
          title="Tool calls"
          entries={tools}
          unit="calls"
          initial={5}
        />
      )}
    </>
  );
}

/**
 * A ranked histogram with a long tail. Showing the leaders keeps the shape
 * readable; the rest stays one chevron away rather than being lost. Bars scale
 * to the overall maximum, so expanding never rescales the rows already on
 * screen.
 */
function RankedBars({
  title,
  entries,
  unit,
  initial,
}: {
  title: string;
  entries: [string, number][];
  unit: string;
  initial: number;
}) {
  const [open, setOpen] = useState(false);
  const id = `ranked-${title.replace(/\s+/g, "-").toLowerCase()}`;

  const max = Math.max(1, ...entries.map(([, n]) => n));
  const total = entries.reduce((sum, [, n]) => sum + n, 0);
  const shown = open ? entries : entries.slice(0, initial);
  const hidden = entries.length - initial;

  return (
    <div className="card">
      <CardHead
        title={title}
        meta={`${total.toLocaleString()} ${unit} · ${entries.length} distinct`}
        action={
          hidden > 0 ? (
            <Disclosure
              open={open}
              onToggle={() => setOpen(!open)}
              controls={id}
              label={open ? "Less" : `All ${entries.length}`}
            />
          ) : undefined
        }
      />
      <div className="card-body" id={id}>
        {shown.map(([label, count]) => (
          <BarRow
            key={label}
            label={label}
            value={count}
            max={max}
            display={count.toLocaleString()}
          />
        ))}
      </div>
    </div>
  );
}
