// Response types mirroring the Go handlers in internal/api.
//
// These are hand-written rather than generated: the surface is small enough
// that codegen would cost more in build complexity than it saves, and the Go
// response structs sit next to these in review.

export type ExecutionStatus =
  | 'pending'
  | 'running'
  | 'success'
  | 'failure'
  | 'blocked'
  | 'rate_limited'
  | 'timeout'
  | 'cancelled'
  | 'skipped'

export type CheckState =
  | 'ok'
  | 'needs_login'
  | 'misconfigured'
  | 'unavailable'
  | 'unknown'

export type RequirementKind = 'aws_profile' | 'mcp_server' | 'binary'

export type OverlapPolicy = 'skip' | 'queue' | 'parallel'
export type GatingPolicy = 'fail_fast' | 'skip' | 'pause_schedule'

export interface Requirement {
  id: number
  task_id: number
  kind: RequirementKind
  target: string
  required: boolean
}

export interface MCPRef {
  name: string
  status: string
}

export interface Execution {
  id: number
  task_id: number | null
  task_name?: string
  trigger: 'cron' | 'manual' | 'retry'
  status: ExecutionStatus
  queued_at: string
  started_at?: string
  finished_at?: string
  claude_session_id: string
  claude_version: string
  model: string
  exit_code: number | null
  is_error: boolean | null
  result_subtype: string
  terminal_reason: string
  num_turns: number
  duration_ms: number
  duration_api_ms: number
  total_cost_usd: number
  input_tokens: number
  output_tokens: number
  cache_read_tokens: number
  cache_creation_tokens: number
  permission_denials: string[]
  mcp_snapshot: MCPRef[]
  preflight_outcome: 'ok' | 'blocked' | 'skipped'
  result_text: string
  error_message: string
  transcript_path: string
  checks?: CheckResult[]
}

export interface Task {
  id: number
  name: string
  description: string
  cron_expr: string
  timezone: string
  prompt: string
  model: string
  cwd: string
  timeout_seconds: number
  max_budget_usd: number
  allowed_tools: string[]
  tools: string[]
  bypass_permissions: boolean
  overlap_policy: OverlapPolicy
  gating_policy: GatingPolicy
  enabled: boolean
  paused: boolean
  paused_reason: string
  created_at: string
  updated_at: string
  requirements: Requirement[]
  last_executions: Execution[]
  next_run_at?: string
}

export interface CheckResult {
  id?: number
  execution_id?: number
  kind: string
  target: string
  state: CheckState
  detail: string
  latency_ms: number
  checked_at: string
  remediation?: Remediation
}

export interface Remediation {
  kind: 'aws_sso_login' | 'manual_command'
  command?: string
  automatic: boolean
  hint?: string
}

/** One dependency kind's results, fetched independently of the others so a
 *  slow probe cannot hide the fast ones. */
export interface KindResults {
  kind: string
  results: CheckResult[]
  /** Every unhealthy dependency of this kind. */
  gating_failures?: number
  /** The subset some enabled task actually requires, which is what a
   *  warning should be based on. */
  blocking_declared?: number
  /** target -> number of enabled tasks requiring it. */
  required_by?: Record<string, number>
  error?: string
}

export interface HealthSnapshot {
  kinds: Record<string, CheckResult[]>
  counts: Partial<Record<CheckState, number>>
  /** Every unhealthy dependency on the machine. */
  gating_failures: number
  /** Those an enabled task requires. The banner uses this, because a
   *  configured-but-unused MCP connector is not a problem. */
  blocking_declared: number
}

export interface ServiceHealth {
  status: string
  version: string
  uptime_seconds: number
  go_version: string
  database: string
  ui_built: boolean
  binaries: { claude: string; aws: string }
  warnings: string[]
}

export interface LoginSession {
  profile: string
  login_profile: string
  state: 'starting' | 'awaiting_authorization' | 'completed' | 'failed' | 'cancelled'
  verification_uri?: string
  user_code?: string
  started_at: string
  finished_at?: string
  error?: string
  output?: string
}

export interface PreflightResult {
  task_id: number
  results: CheckResult[]
  failing: CheckResult[]
  would_run: boolean
  gating_policy: GatingPolicy
}

/** One event of an execution transcript, as delivered over SSE or the events endpoint. */
export interface TranscriptEvent {
  seq: number
  type: string
  subtype?: string
  payload?: unknown
  terminal?: boolean
  status?: ExecutionStatus
}

export interface TaskInput {
  name: string
  description?: string
  cron_expr: string
  timezone?: string
  prompt: string
  model?: string
  cwd?: string
  timeout_seconds?: number
  max_budget_usd?: number
  allowed_tools?: string[]
  tools?: string[]
  bypass_permissions?: boolean
  overlap_policy?: OverlapPolicy
  gating_policy?: GatingPolicy
  enabled?: boolean
  requirements?: Array<{ kind: RequirementKind; target: string; required?: boolean }>
}

export interface ValidationError {
  error: string
  problems?: string[]
}
