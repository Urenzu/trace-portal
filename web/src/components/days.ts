import type { Turn } from "../api";

/**
 * A stretch of one session that happened on a single local calendar day.
 *
 * Sessions are resumed. Claude Code keeps the same session id when a
 * conversation is picked back up, so one session routinely spans a night, a
 * weekend, or a week — and the turn list then reads as a run of clock times
 * with no indication that the clock wrapped. A 09:14 turn following a 23:51 one
 * looks like the list is out of order when it is simply the next morning.
 *
 * Splitting on the local day is deliberate: the boundary a person recognises is
 * their own midnight, not UTC's.
 */
export interface DaySegment {
  /** Local calendar day, YYYY-MM-DD, usable as a React key. */
  key: string;
  from: Date;
  to: Date;
  turns: Turn[];
  /** Idle time since the previous segment's last turn; 0 for the first. */
  gapMS: number;
  costUSD: number;
}

function localDayKey(d: Date): string {
  // Not toISOString: that is UTC, and would move the boundary for anyone not
  // on it — a 7pm turn in New York would be filed under the following day.
  const month = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${month}-${day}`;
}

/**
 * Splits turns into per-day segments, in chronological order.
 *
 * The turns are expected to arrive oldest first, and are sorted here anyway:
 * the day rules are drawn from this result, so a stray out-of-order turn would
 * otherwise draw a boundary that never happened.
 */
export function daySegments(turns: Turn[]): DaySegment[] {
  if (turns.length === 0) return [];

  const ordered = [...turns].sort(
    (a, b) =>
      new Date(a.started_at).getTime() - new Date(b.started_at).getTime(),
  );

  const segments: DaySegment[] = [];
  for (const turn of ordered) {
    const at = new Date(turn.started_at);
    const key = localDayKey(at);
    const current = segments[segments.length - 1];

    if (current && current.key === key) {
      current.turns.push(turn);
      current.to = at;
      current.costUSD += turn.cost_usd;
      continue;
    }
    segments.push({
      key,
      from: at,
      to: at,
      turns: [turn],
      gapMS: current ? at.getTime() - current.to.getTime() : 0,
      costUSD: turn.cost_usd,
    });
  }
  return segments;
}

/** How a day segment is labelled: the date, plus how long the session idled. */
export function dayLabel(segment: DaySegment): string {
  return segment.from.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
}

/** A compact idle span — "18h", "3d" — for the resume marker. */
export function idleLabel(gapMS: number): string {
  const hours = gapMS / 3_600_000;
  if (hours < 1) return `${Math.max(1, Math.round(gapMS / 60_000))}m`;
  if (hours < 48) return `${Math.round(hours)}h`;
  return `${Math.round(hours / 24)}d`;
}
