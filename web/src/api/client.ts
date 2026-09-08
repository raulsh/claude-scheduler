import type {
  CheckResult,
  KindResults,
  Execution,
  HealthSnapshot,
  LoginSession,
  PreflightResult,
  ServiceHealth,
  Task,
  TaskInput,
  TranscriptEvent,
} from './types'

const BASE = '/api/v1'

/** An API error carrying the server's own message and any field problems. */
export class ApiError extends Error {
  status: number
  problems: string[]

  constructor(status: number, message: string, problems: string[] = []) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.problems = problems
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(BASE + path, {
    ...init,
    headers: {
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
  })

  if (response.status === 204) {
    return undefined as T
  }

  const text = await response.text()
  let body: unknown
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    // A non-JSON body means something upstream of the handler failed.
    if (!response.ok) throw new ApiError(response.status, text || response.statusText)
    throw new ApiError(response.status, 'the server returned a malformed response')
  }

  if (!response.ok) {
    const err = body as { error?: string; problems?: string[] }
    throw new ApiError(
      response.status,
      err?.error ?? response.statusText,
      err?.problems ?? [],
    )
  }
  return body as T
}

export interface ExecutionFilters {
  taskId?: number
  statuses?: string[]
  from?: string
  to?: string
  q?: string
  limit?: number
  cursor?: string
}

/** Renders filters as the query string the executions endpoint expects. */
export function executionQuery(filters: ExecutionFilters): string {
  const params = new URLSearchParams()
  if (filters.taskId != null) params.set('task_id', String(filters.taskId))
  filters.statuses?.forEach((s) => params.append('status', s))
  if (filters.from) params.set('from', filters.from)
  if (filters.to) params.set('to', filters.to)
  if (filters.q) params.set('q', filters.q)
  if (filters.limit) params.set('limit', String(filters.limit))
  if (filters.cursor) params.set('cursor', filters.cursor)
  const qs = params.toString()
  return qs ? `?${qs}` : ''
}

export const api = {
  listTasks: () => request<{ tasks: Task[] }>('/tasks'),
  getTask: (id: number) => request<Task>(`/tasks/${id}`),
  createTask: (input: TaskInput) =>
    request<Task>('/tasks', { method: 'POST', body: JSON.stringify(input) }),
  updateTask: (id: number, input: Partial<TaskInput>) =>
    request<Task>(`/tasks/${id}`, { method: 'PATCH', body: JSON.stringify(input) }),
  deleteTask: (id: number) => request<void>(`/tasks/${id}`, { method: 'DELETE' }),
  runTask: (id: number) => request<Execution>(`/tasks/${id}/run`, { method: 'POST' }),
  preflightTask: (id: number) =>
    request<PreflightResult>(`/tasks/${id}/preflight`, { method: 'POST' }),
  resumeTask: (id: number) => request<Task>(`/tasks/${id}/resume`, { method: 'POST' }),

  listExecutions: (filters: ExecutionFilters = {}) =>
    request<{ executions: Execution[]; next_cursor: string }>(
      `/executions${executionQuery(filters)}`,
    ),
  getExecution: (id: number) => request<Execution>(`/executions/${id}`),
  listEvents: (id: number, afterSeq = 0) =>
    request<{ events: TranscriptEvent[] }>(
      `/executions/${id}/events?after_seq=${afterSeq}&limit=2000`,
    ),
  cancelExecution: (id: number) =>
    request<{ cancelled: number }>(`/executions/${id}/cancel`, { method: 'POST' }),
  rerunWithAllowlist: (id: number) =>
    request<{ execution: Execution; rules_added: string[]; allowed_tools: string[] }>(
      `/executions/${id}/rerun-with-allowlist`,
      { method: 'POST' },
    ),

  serviceHealth: () => request<ServiceHealth>('/health'),
  healthSnapshot: (force = false) =>
    request<HealthSnapshot>(`/health/snapshot${force ? '?force=1' : ''}`),
  healthKinds: () => request<{ kinds: string[] }>('/health/kinds'),
  checkKind: (kind: string, force = false) =>
    request<KindResults>(
      `/health/${encodeURIComponent(kind)}${force ? '?force=1' : ''}`,
    ),
  checkOne: (kind: string, target: string, force = false) =>
    request<CheckResult>(
      `/health/${encodeURIComponent(kind)}/${encodeURIComponent(target)}${force ? '?force=1' : ''}`,
    ),
  latestChecks: () => request<{ checks: CheckResult[] }>('/health/checks'),

  startAwsLogin: (profile: string) =>
    request<LoginSession>(`/health/aws/${encodeURIComponent(profile)}/login`, {
      method: 'POST',
    }),
  getAwsLogin: (profile: string) =>
    request<LoginSession>(`/health/aws/${encodeURIComponent(profile)}/login`),
  cancelAwsLogin: (profile: string) =>
    request<{ cancelled: string }>(`/health/aws/${encodeURIComponent(profile)}/login`, {
      method: 'DELETE',
    }),
}

/** The SSE endpoint for an execution's live transcript. */
export function streamUrl(executionId: number, afterSeq = 0): string {
  return `${BASE}/executions/${executionId}/stream?after_seq=${afterSeq}`
}
