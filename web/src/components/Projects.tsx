import { useMemo, useState } from "react";

import type { ProjectStat } from "../api";
import { tokens, usd, pct } from "../format";
import { CardHead, Disclosure, SearchBox } from "./ui";

/** Rows visible before the list starts scrolling inside the card. */
const LIST_MAX_HEIGHT = 380;
const TOP = 6;

/**
 * Spend attributed to the repository the work happened in.
 *
 * Cost per project is the question someone with several repositories actually
 * asks, and it is the one number an aggregate dashboard cannot answer. The
 * cache hit rate beside it is what explains a project being expensive: a long
 * session with a warm cache and a short one that keeps re-seeding it cost very
 * different amounts for the same work.
 */
export function Projects({ projects }: { projects: ProjectStat[] }) {
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");

  // Two directories can legitimately share a name — deleted subdirectories of
  // one repository, most often. They are distinct entities, so the label gets a
  // short suffix rather than the rows being merged or silently repeated.
  const nameCounts = new Map<string, number>();
  for (const p of projects)
    nameCounts.set(p.project, (nameCounts.get(p.project) ?? 0) + 1);
  const label = (p: ProjectStat) =>
    (nameCounts.get(p.project) ?? 0) > 1
      ? `${p.project} · ${p.project_id.slice(0, 4)}`
      : p.project;

  // This list is already in memory in full, so its search runs here rather than
  // over the network. The session list is the opposite case and searches
  // server-side, because most of its history has never been loaded.
  const matches = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (!needle) return projects;
    return projects.filter(
      (p) =>
        p.project.toLowerCase().includes(needle) ||
        p.project_id.toLowerCase().includes(needle),
    );
  }, [projects, search]);

  if (projects.length === 0) return null;

  // A search shows everything it matched: collapsing results back to six would
  // hide the row someone just searched for.
  const searching = search.trim() !== "";
  const expanded = open || searching;
  const shown = expanded ? matches : matches.slice(0, TOP);
  const hidden = matches.length - TOP;
  const maxCost = Math.max(...projects.map((p) => p.cost_usd), 0.000001);
  const total = matches.reduce((sum, p) => sum + p.cost_usd, 0);

  return (
    <div className="card">
      <CardHead
        title="By project"
        meta={
          searching
            ? `${usd(total)} across ${matches.length} of ${projects.length}`
            : `${usd(total)} across ${projects.length}`
        }
        action={
          <>
            <SearchBox
              value={search}
              onChange={setSearch}
              placeholder="Search projects…"
            />
            {!searching && hidden > 0 && (
              <Disclosure
                open={open}
                onToggle={() => setOpen(!open)}
                controls="projects-body"
                label={open ? "Less" : `All ${projects.length}`}
              />
            )}
          </>
        }
      />

      {shown.length === 0 ? (
        <div className="empty">
          No project matches <span className="mono">{search.trim()}</span>.
        </div>
      ) : (
        <div
          className={`card-body table-wrap${expanded ? " scroll-list" : ""}`}
          id="projects-body"
          /* Opening the full list should not push the rest of the page down by
             however many repositories happen to exist. */
          style={expanded ? { maxHeight: LIST_MAX_HEIGHT } : undefined}
        >
          <table>
            <thead>
              <tr>
                <th>Project</th>
                <th style={{ width: "26%" }}>Share of spend</th>
                <th className="right">Cost</th>
                <th className="right">Turns</th>
                <th className="right">Cache hit</th>
                <th className="right">Tokens</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((p) => (
                <tr key={p.project_id}>
                  <td>
                    <span style={{ fontWeight: 550 }} title={p.project_id}>
                      {label(p)}
                    </span>
                    {/* Not every directory is a repository. Saying so beats
                        presenting a Downloads folder as a project. */}
                    {!p.in_repo && (
                      <span
                        className="pill"
                        style={{ marginLeft: 6 }}
                        title="Not a git repository — work done in a plain directory"
                      >
                        directory
                      </span>
                    )}
                    {p.errors ? (
                      <span className="pill err" style={{ marginLeft: 6 }}>
                        {p.errors} err
                      </span>
                    ) : null}
                  </td>
                  <td>
                    {/* Neutral: length already carries the magnitude, and the
                        projects have no order of their own to encode. */}
                    <div className="bar-track">
                      <div
                        className="bar-fill"
                        style={{
                          width: `${Math.max(1.5, (p.cost_usd / maxCost) * 100)}%`,
                          background: "var(--bar-neutral)",
                        }}
                      />
                    </div>
                  </td>
                  <td className="right num">{usd(p.cost_usd)}</td>
                  <td className="right num muted">
                    {p.turns.toLocaleString()}
                  </td>
                  <td className="right num">{pct(p.cache_hit_rate)}</td>
                  <td className="right num muted">
                    {tokens(p.input_tokens + p.output_tokens)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
