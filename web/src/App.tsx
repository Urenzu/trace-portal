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

const DEFAULT_DAYS = 7;

/**
 * The window travels with the route.
 *
 * A link to a session that silently reverts to the default window is a link to
 * a different page: the session it names may not be inside seven days, and the
 * reader who sent it was looking at thirty. Carrying it in the hash makes every
 * view reproducible from its URL, and makes the back button undo a window
 * change the same way it undoes a drill-in.
 */
function readHash(): { route: Route; days: number } {
  const raw = window.location.hash.replace(/^#\/?/, "");
  const [path, search] = raw.split("?");
  const days = Number(new URLSearchParams(search ?? "").get("d"));

  let route: Route = { kind: "dashboard" };
  for (const kind of ["session", "project"] as const) {
    const prefix = `${kind}/`;
    if (path.startsWith(prefix)) {
      route = { kind, id: decodeURIComponent(path.slice(prefix.length)) };
      break;
    }
  }
  return {
    route,
    days: WINDOWS.some((w) => w.days === days) ? days : DEFAULT_DAYS,
  };
}

function writeHash(route: Route, days: number) {
  const path =
    route.kind === "dashboard"
      ? ""
      : `${route.kind}/${encodeURIComponent(route.id)}`;
  const suffix = days === DEFAULT_DAYS ? "" : `?d=${days}`;
  window.location.hash = path || suffix ? `#/${path}${suffix}` : "";
}

/** YYYY-MM-DD, UTC, for the window's ends. The day rollups are keyed this way,
 *  so the charts and the calendar agree on where a day starts. */
function dayKey(date: Date): string {
  return date.toISOString().slice(0, 10);
}

export function App() {
  const initial = readHash();
  const [days, setDays] = useState(initial.days);
  const [route, setRoute] = useState<Route>(initial.route);
  const [seed, setSeed] = useState<{ query: string; nonce: number }>();
  const [stats, setStats] = useState<Stats | null>(null);
  const [health, setHealth] = useState<Health | null>(null);
  const [statsError, setStatsError] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem("tp-theme") as Theme) || "system",
  );

  useEffect(() => {
    const onHash = () => {
      const next = readHash();
      setRoute(next.route);
      setDays(next.days);
    };
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
    writeHash(next, days);
    setRoute(next);
    // A drill-in is a new page, so it starts at the top rather than wherever
    // the reader happened to be scrolled in the one before it.
    window.scrollTo({ top: 0 });
  }

  function setWindow(next: number) {
    setDays(next);
    writeHash(route, next);
  }

  const openSession = (id: string) => go({ kind: "session", id });
  const openProject = (id: string) => go({ kind: "project", id });
  const goHome = () => go({ kind: "dashboard" });

  // A tile that filters the list has to bring the list into view, or it reads
  // as having done nothing at all.
  function filterSessions(query: string) {
    setSeed({ query, nonce: Date.now() });
    requestAnimationFrame(() =>
      document
        .getElementById("session-list")
        ?.scrollIntoView({ behavior: "smooth", block: "start" }),
    );
  }

  const windowTo = dayKey(new Date());
  const windowFrom = dayKey(new Date(Date.now() - (days - 1) * 86_400_000));

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
                onClick={() => setWindow(w.days)}
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
          windowFrom={windowFrom}
          windowTo={windowTo}
          onBack={goHome}
          onOpenSession={openSession}
        />
      )}

      {route.kind === "dashboard" && (
        <>
          {health && <DataCoverage health={health} days={days} />}
          {stats && (
            <Dashboard
              stats={stats}
              onOpenProject={openProject}
              onFilterSessions={filterSessions}
              windowFrom={windowFrom}
              windowTo={windowTo}
            />
          )}
          <SessionList days={days} onOpen={openSession} seed={seed} />
        </>
      )}
    </div>
  );
}
