import { useEffect, useState } from "react";

import { api, type Health, type Stats } from "./api";
import { Dashboard } from "./components/Dashboard";
import { DataCoverage } from "./components/DataCoverage";
import { ProjectView } from "./components/ProjectView";
import { SessionList } from "./components/SessionList";
import { SessionView } from "./components/SessionView";

const WINDOWS = [
  { days: 1, label: "24h" },
  { days: 7, label: "7d" },
  { days: 30, label: "30d" },
  { days: 365, label: "1y" },
];

type Theme = "light" | "dark" | "system";

/** What the viewer is actually looking at, resolving "system" against the OS. */
function resolveTheme(theme: Theme): "light" | "dark" {
  if (theme !== "system") return theme;
  return window.matchMedia("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

/**
 * What the hash is pointing at. Routing through the hash keeps every view
 * linkable and survives a reload, which matters most for the two drill-ins: a
 * session timeline and a project.
 */
type Route =
  | { kind: "dashboard" }
  | { kind: "session"; id: string }
  | { kind: "project"; id: string };

function readHash(): Route {
  const hash = window.location.hash.replace(/^#\/?/, "");
  for (const kind of ["session", "project"] as const) {
    const prefix = `${kind}/`;
    if (hash.startsWith(prefix)) {
      return { kind, id: decodeURIComponent(hash.slice(prefix.length)) };
    }
  }
  return { kind: "dashboard" };
}

export function App() {
  const [days, setDays] = useState(7);
  const [route, setRoute] = useState<Route>(readHash);
  const [stats, setStats] = useState<Stats | null>(null);
  const [health, setHealth] = useState<Health | null>(null);
  const [statsError, setStatsError] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem("tp-theme") as Theme) || "system",
  );

  useEffect(() => {
    const onHash = () => setRoute(readHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  useEffect(() => {
    if (theme === "system") {
      document.documentElement.removeAttribute("data-theme");
    } else {
      document.documentElement.setAttribute("data-theme", theme);
    }
    // Wrapped because storage throws outright in some privacy modes.
    try {
      localStorage.setItem("tp-theme", theme);
    } catch {
      /* a remembered theme is a convenience, not a requirement */
    }
  }, [theme]);

  useEffect(() => {
    // Read once: the payload is the whole archive, and the panel narrows it to
    // whatever window is selected rather than refetching per window.
    api
      .health()
      .then(setHealth)
      .catch(() => setHealth(null));
  }, []);

  useEffect(() => {
    let cancelled = false;
    setStatsError(null);
    api
      .stats(days)
      .then((s) => !cancelled && setStats(s))
      .catch(
        (e) =>
          !cancelled &&
          setStatsError(e instanceof Error ? e.message : String(e)),
      );
    return () => {
      cancelled = true;
    };
  }, [days]);

  function go(next: Route) {
    window.location.hash =
      next.kind === "dashboard"
        ? ""
        : `#/${next.kind}/${encodeURIComponent(next.id)}`;
    setRoute(next);
    // A drill-in is a new page, so it starts at the top rather than wherever
    // the reader happened to be scrolled in the one before it.
    window.scrollTo({ top: 0 });
  }

  const openSession = (id: string) => go({ kind: "session", id });
  const openProject = (id: string) => go({ kind: "project", id });
  const goHome = () => go({ kind: "dashboard" });

  return (
    <div className="app">
      <header className="topbar">
        <div className="spacer" />
        <div className="filters">
          <div className="seg" role="group" aria-label="Time window">
            {WINDOWS.map((w) => (
              <button
                key={w.days}
                aria-pressed={days === w.days}
                onClick={() => setDays(w.days)}
              >
                {w.label}
              </button>
            ))}
          </div>
          <button
            className="icon-btn"
            /* Toggle from what is actually on screen. Flipping the stored
               value instead means the first click out of "system" picks the
               mode the OS already resolved to, and appears to do nothing. */
            onClick={() =>
              setTheme(resolveTheme(theme) === "dark" ? "light" : "dark")
            }
            aria-label="Toggle colour theme"
            title={`Theme: ${theme}`}
          >
            {resolveTheme(theme) === "dark" ? "☾" : "☀"}
          </button>
        </div>
      </header>

      {statsError && (
        <div className="error-banner">Could not load stats: {statsError}</div>
      )}

      {route.kind === "session" && (
        <SessionView id={route.id} days={days} onBack={goHome} />
      )}

      {route.kind === "project" && (
        <ProjectView
          projectId={route.id}
          days={days}
          project={stats?.projects?.find((p) => p.project_id === route.id)}
          onBack={goHome}
          onOpenSession={openSession}
        />
      )}

      {route.kind === "dashboard" && (
        <>
          {health && <DataCoverage health={health} days={days} />}
          {stats && <Dashboard stats={stats} onOpenProject={openProject} />}
          <SessionList days={days} onOpen={openSession} />
        </>
      )}
    </div>
  );
}
