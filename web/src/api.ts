// Types mirror the Go structs in internal/query and internal/api. Keep them in
// step with those; the API is the contract between the two halves.

export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cache_creation_input_tokens: number;
  cache_read_input_tokens: number;
  cache_creation?: {
    ephemeral_5m_input_tokens: number;
    ephemeral_1h_input_tokens: number;
  };
}

export interface ToolCall {
  id: string;
  name: string;
}

export interface Turn {
  turn_id: string;
  session_id: string;
  started_at: string;
  model: string;
  stream: boolean;
  status_code?: number;
  stop_reason?: string;
  duration_ms: number;
  ttfb_ms: number;
  message_count: number;
  system_blocks: number;
  tools_offered?: string[];
  usage: Usage;
  cost_usd: number;
  priced: boolean;
  tool_calls?: ToolCall[];
  request_blob?: string;
  response_blob?: string;
  error?: string;
  pending: boolean;
}

export interface Session {
  id: string;
  model: string;
  project?: string;
  git_branch?: string;
  models?: string[];
  started_at: string;
  ended_at: string;
  turns: number;
  usage: Usage;
  cost_usd: number;
  unpriced_turns?: number;
  cache_hit_rate: number;
  tool_calls: number;
  tool_names?: string[];
  errors?: number;
}

export interface SessionDetail extends Session {
  turn_list: Turn[];
}

export interface SessionPage {
  sessions: Session[];
  next_cursor?: string;
  days_scanned: number;
}

export interface ProjectStat {
  project: string;
  project_id: string;
  in_repo: boolean;
  turns: number;
  sessions: number;
  cost_usd: number;
  cache_hit_rate: number;
  errors?: number;
  input_tokens: number;
  output_tokens: number;
}

export interface Stats {
  from: string;
  to: string;
  sessions: number;
  turns: number;
  errors: number;
  usage: Usage;
  cost_usd: number;
  cache_hit_rate: number;
  savings_usd: number;
  projects?: ProjectStat[];
  turns_by_model?: Record<string, number>;
  tool_calls_by_name?: Record<string, number>;
  unpriced_turns?: number;
  sessions_exact: boolean;
}

/** What ingestion understood, per source. Agent log formats change weekly, so
 *  the UI reports what it could not read rather than presenting gaps as zeros. */
export interface Coverage {
  records: number;
  parsed: number;
  skipped: number;
  unreadable: number;
  by_version?: Record<string, number>;
  missing_field?: Record<string, number>;
  unknown_field?: Record<string, number>;
}

export interface Health {
  status: string;
  days_captured: number;
  first_day?: string;
  last_day?: string;
  coverage?: Record<string, Coverage>;
}

class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function get<T>(
  path: string,
  params: Record<string, string | number | undefined> = {},
): Promise<T> {
  const url = new URL(path, window.location.origin);
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== "")
      url.searchParams.set(key, String(value));
  }

  const resp = await fetch(url);
  if (!resp.ok) {
    // The API reports failures as {"error": "..."}; fall back to the status
    // text when the body is not the shape we expect.
    let detail = resp.statusText;
    try {
      const body = await resp.json();
      if (body && typeof body.error === "string") detail = body.error;
    } catch {
      /* keep statusText */
    }
    throw new ApiError(detail, resp.status);
  }
  return resp.json() as Promise<T>;
}

export const api = {
  health: () => get<Health>("/api/health"),

  stats: (days: number) => get<Stats>("/api/stats", { days }),

  sessions: (days: number, limit: number, cursor?: string, q?: string) =>
    get<SessionPage>("/api/sessions", { days, limit, cursor, q }),

  session: (id: string, days: number) =>
    get<SessionDetail>(`/api/sessions/${encodeURIComponent(id)}`, { days }),

  // Payloads are fetched only when a turn is expanded, which is the whole
  // point of keeping them out of the event records.
  blob: (ref: string) => get<unknown>(`/api/blobs/${encodeURIComponent(ref)}`),
};

export { ApiError };
