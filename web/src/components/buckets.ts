import type { Turn } from "../api";

/**
 * A drawable column of the timeline: either one turn, or several turns summed
 * because they would not have been separable on screen anyway.
 */
export interface Bucket {
  key: string;
  /** Midpoint of the bucket's time span, where the column is drawn. */
  at: Date;
  from: Date;
  to: Date;
  turns: Turn[];

  cacheRead: number;
  cacheWrite: number;
  input: number;
  output: number;
  costUSD: number;
  errors: number;
  toolCalls: number;
}

/** Above this many turns, a per-turn column is narrower than a pixel and the
 *  browser is asked to lay out thousands of nodes for no gain. */
export const BUCKET_THRESHOLD = 240;

/**
 * Groups turns into at most `target` columns of equal time width.
 *
 * A session can run to thousands of turns; one 2,746-turn session exists in
 * real data. Drawing a column each would emit thousands of SVG nodes to render
 * marks thinner than a pixel — slow, and unreadable regardless. Bucketing by
 * time rather than by index keeps the x-axis honest: a gap where nothing
 * happened stays a gap, which is what makes cache expiry visible.
 */
export function bucketTurns(turns: Turn[], target = 120): Bucket[] {
  if (turns.length === 0) return [];
  if (turns.length <= BUCKET_THRESHOLD) {
    return turns.map((t) => single(t));
  }

  const times = turns.map((t) => new Date(t.started_at).getTime());
  const min = Math.min(...times);
  const max = Math.max(...times);
  // A session compressed into one instant still needs a non-zero width.
  const span = Math.max(max - min, 1);
  const width = span / target;

  const byIndex = new Map<number, Bucket>();
  for (const turn of turns) {
    const at = new Date(turn.started_at).getTime();
    const index = Math.min(target - 1, Math.floor((at - min) / width));

    let bucket = byIndex.get(index);
    if (!bucket) {
      const from = new Date(min + index * width);
      const to = new Date(min + (index + 1) * width);
      bucket = {
        key: `b${index}`,
        at: new Date(from.getTime() + width / 2),
        from,
        to,
        turns: [],
        cacheRead: 0,
        cacheWrite: 0,
        input: 0,
        output: 0,
        costUSD: 0,
        errors: 0,
        toolCalls: 0,
      };
      byIndex.set(index, bucket);
    }
    add(bucket, turn);
  }

  return [...byIndex.values()].sort((a, b) => a.at.getTime() - b.at.getTime());
}

function single(turn: Turn): Bucket {
  const at = new Date(turn.started_at);
  const bucket: Bucket = {
    key: turn.turn_id,
    at,
    from: at,
    to: at,
    turns: [],
    cacheRead: 0,
    cacheWrite: 0,
    input: 0,
    output: 0,
    costUSD: 0,
    errors: 0,
    toolCalls: 0,
  };
  add(bucket, turn);
  return bucket;
}

function add(bucket: Bucket, turn: Turn) {
  bucket.turns.push(turn);
  bucket.cacheRead += turn.usage.cache_read_input_tokens;
  bucket.cacheWrite += turn.usage.cache_creation_input_tokens;
  bucket.input += turn.usage.input_tokens;
  bucket.output += turn.usage.output_tokens;
  bucket.costUSD += turn.cost_usd;
  bucket.toolCalls += turn.tool_calls?.length ?? 0;
  if (turn.error) bucket.errors += 1;
}

export function bucketTotal(b: Bucket): number {
  return b.cacheRead + b.cacheWrite + b.input + b.output;
}

/** Share of a bucket's input tokens served from cache. */
export function bucketHitRate(b: Bucket): number {
  const totalInput = b.cacheRead + b.cacheWrite + b.input;
  return totalInput > 0 ? b.cacheRead / totalInput : 0;
}
