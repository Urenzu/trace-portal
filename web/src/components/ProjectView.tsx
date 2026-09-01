import type { ProjectStat } from "../api";
import { tokens, usd, pct } from "../format";
import { SessionList } from "./SessionList";

interface Props {
  projectId: string;
  days: number;
  /** The dashboard row this view was opened from, when the window has one. */
  project?: ProjectStat;
  onBack: () => void;
  onOpenSession: (id: string) => void;
}

/**
 * One project's spend and its sessions.
 *
 * The dashboard answers "which project cost the most"; the obvious next
 * question is "which sessions in it", and that is a different page rather than
 * a wider table. The session list is the same component the dashboard uses,
 * scoped to this project — so search, scrolling and paging all behave the way
 * they do everywhere else.
 *
 * Scoping is by project digest, not name: two directories can share a name, and
 * a link keyed on what is written on screen would quietly merge them.
 */
export function ProjectView({
  projectId,
  days,
  project,
  onBack,
  onOpenSession,
}: Props) {
  const totalTokens = project
    ? project.input_tokens + project.output_tokens
    : 0;

  return (
    <>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 12,
          marginBottom: 14,
        }}
      >
        <button className="icon-btn" onClick={onBack}>
          ← Dashboard
        </button>
        <span style={{ fontWeight: 600 }}>{project?.project ?? "Project"}</span>
        {project && !project.in_repo && (
          <span
            className="pill"
            title="Not a git repository — work done in a plain directory"
          >
            directory
          </span>
        )}
        <span className="mono muted" style={{ fontSize: 11.5 }}>
          {projectId}
        </span>
      </div>

      {project ? (
        <div className="tiles">
          <div className="tile">
            <div className="tile-label">Cost</div>
            <div className="tile-value num">{usd(project.cost_usd)}</div>
            <div className="tile-note">
              {project.turns.toLocaleString()} turns in the last {days} days
            </div>
          </div>
          <div className="tile">
            <div className="tile-label">Cache hit rate</div>
            <div className="tile-value num">{pct(project.cache_hit_rate)}</div>
            <div className="tile-note">of input tokens</div>
          </div>
          <div className="tile">
            <div className="tile-label">Tokens</div>
            <div className="tile-value num">{tokens(totalTokens)}</div>
            <div className="tile-note">fresh input and output</div>
          </div>
          <div className="tile">
            <div className="tile-label">Errors</div>
            <div className="tile-value num">{project.errors ?? 0}</div>
            <div className="tile-note">failed or 4xx/5xx turns</div>
          </div>
        </div>
      ) : (
        /* The window moved after the link was made, or the project has no
           activity inside it. The session list below is still authoritative. */
        <div className="empty" style={{ marginBottom: 14 }}>
          No activity for this project in the last {days} days. Widen the window
          above to see its history.
        </div>
      )}

      <SessionList
        days={days}
        onOpen={onOpenSession}
        scope={`projectid:${projectId}`}
        title="Sessions in this project"
      />
    </>
  );
}
