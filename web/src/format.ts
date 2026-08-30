export function tokens(n: number): string {
  if (n === 0) return "0";
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

export function usd(n: number): string {
  if (n === 0) return "$0";
  if (n < 0.01) return `$${n.toFixed(4)}`;
  if (n < 1) return `$${n.toFixed(3)}`;
  if (n < 1000) return `$${n.toFixed(2)}`;
  return `$${n.toLocaleString(undefined, { maximumFractionDigits: 0 })}`;
}

export function pct(fraction: number): string {
  return `${(fraction * 100).toFixed(1)}%`;
}

export function ms(n: number): string {
  if (n < 1000) return `${Math.round(n)}ms`;
  if (n < 60_000) return `${(n / 1000).toFixed(1)}s`;
  const minutes = Math.floor(n / 60_000);
  const seconds = Math.round((n % 60_000) / 1000);
  return `${minutes}m ${seconds}s`;
}

export function duration(fromISO: string, toISO: string): string {
  return ms(new Date(toISO).getTime() - new Date(fromISO).getTime());
}

export function clockTime(iso: string): string {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function dateTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function relative(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  const minutes = Math.round(diff / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/** Short form of a session hash, which is a 16-character digest by default. */
export function shortId(id: string): string {
  return id.length > 14 ? `${id.slice(0, 10)}…` : id;
}

/**
 * Latency that may not have been observed at all. Agent transcripts record the
 * conversation, not the request, so a tailed turn carries no timing — and an
 * API call never genuinely takes zero milliseconds. Rendering that as "0ms"
 * would present a gap as a measurement.
 */
export function msOrUnknown(n: number): string {
  return n > 0 ? ms(n) : "—";
}

/**
 * A session's span, or an em dash when it has none. A single-turn session ends
 * where it started unless a turn reported its own duration, and tailed turns do
 * not, so "0ms" would be a gap dressed as a measurement.
 */
export function durationOrUnknown(fromISO: string, toISO: string): string {
  const span = new Date(toISO).getTime() - new Date(fromISO).getTime();
  return span > 0 ? ms(span) : "—";
}
