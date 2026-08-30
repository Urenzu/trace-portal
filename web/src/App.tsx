import { useEffect, useState } from "react";

import { api, type Health, type Stats } from "./api";
import { Dashboard } from "./components/Dashboard";
import { DataCoverage } from "./components/DataCoverage";
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

/** The session id lives in the hash so a timeline can be linked and reloaded. */
function readHash(): string | null {
  const hash = window.location.hash.replace(/^#\/?/, "");
  return hash.startsWith("session/")
    ? decodeURIComponent(hash.slice("session/".length))
    : null;
}

export function App() {
  const [days, setDays] = useState(7);
  const [sessionId, setSessionId] = useState<string | null>(readHash);
  const [stats, setStats] = useState<Stats | null>(null);
  const [health, setHealth] = useState<Health | null>(null);
  const [statsError, setStatsError] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem("tp-theme") as Theme) || "system",
  );

  useEffect(() => {
    const onHash = () => setSessionId(readHash());
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
    // Coverage is read once: it describes the whole archive, not a window.
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

  function openSession(id: string) {
    window.location.hash = `#/session/${encodeURIComponent(id)}`;
    setSessionId(id);
  }

  function closeSession() {
    window.location.hash = "";
    setSessionId(null);
  }

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

      {sessionId ? (
        <SessionView id={sessionId} days={days} onBack={closeSession} />
      ) : (
        <>
          {health?.coverage && <DataCoverage coverage={health.coverage} />}
          {stats && <Dashboard stats={stats} />}
          <SessionList days={days} onOpen={openSession} />
        </>
      )}
    </div>
  );
}
