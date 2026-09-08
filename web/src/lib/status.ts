import type { CheckState, ExecutionStatus } from '../api/types'

/** How an execution status is presented.
 *
 * The user asked for green or red at a glance, and that remains the dominant
 * read: success is green, the failure family is warm. But the real state
 * space is wider, and flattening it would hide the distinction the whole tool
 * exists to surface. `blocked` means a dependency was broken and the task
 * never ran, which calls for a completely different response than `failure`,
 * where Claude ran and the work itself went wrong.
 *
 * Colour is never the only channel: each status also carries a distinct glyph
 * and an explicit label.
 */
export interface StatusPresentation {
  label: string
  /** Tailwind class for the swatch background. */
  swatch: string
  /** Tailwind class for text in this status's colour. */
  text: string
  /** A short glyph so the status survives greyscale and colour blindness. */
  glyph: string
  /** One-line explanation shown in tooltips and detail headers. */
  meaning: string
}

const PRESENTATION: Record<ExecutionStatus, StatusPresentation> = {
  success: {
    label: 'Success',
    swatch: 'bg-st-success',
    text: 'text-st-success',
    glyph: '✓',
    meaning: 'The run completed and Claude reported no error.',
  },
  failure: {
    label: 'Failed',
    swatch: 'bg-st-failure',
    text: 'text-st-failure',
    glyph: '✕',
    meaning: 'The run executed but finished with an error.',
  },
  blocked: {
    label: 'Blocked',
    swatch: 'bg-st-blocked',
    text: 'text-st-blocked',
    glyph: '⃫',
    meaning: 'A dependency was unhealthy, so the task never ran. Nothing was spent.',
  },
  rate_limited: {
    label: 'Rate limited',
    swatch: 'bg-st-rate',
    text: 'text-st-rate',
    glyph: '◷',
    meaning: 'A usage or credit limit was reached. This is not a fault in the task.',
  },
  timeout: {
    label: 'Timed out',
    swatch: 'bg-st-timeout',
    text: 'text-st-timeout',
    glyph: '⏱',
    meaning: 'The run exceeded its timeout and was stopped.',
  },
  cancelled: {
    label: 'Cancelled',
    swatch: 'bg-st-cancelled',
    text: 'text-st-cancelled',
    glyph: '⊘',
    meaning: 'The run was stopped before it finished.',
  },
  skipped: {
    label: 'Skipped',
    swatch: 'bg-st-skipped',
    text: 'text-fg-faint',
    glyph: '-',
    meaning: 'The run was suppressed, usually because the previous one was still going.',
  },
  running: {
    label: 'Running',
    swatch: 'bg-st-running',
    text: 'text-st-running',
    glyph: '▸',
    meaning: 'The run is in progress.',
  },
  pending: {
    label: 'Queued',
    swatch: 'bg-st-running',
    text: 'text-st-running',
    glyph: '·',
    meaning: 'The run is waiting for a free execution slot.',
  },
}

export function statusPresentation(status: ExecutionStatus): StatusPresentation {
  return (
    PRESENTATION[status] ?? {
      label: status,
      swatch: 'bg-st-empty',
      text: 'text-fg-muted',
      glyph: '?',
      meaning: 'Unrecognised status.',
    }
  )
}

export const ALL_STATUSES: ExecutionStatus[] = [
  'success',
  'failure',
  'blocked',
  'rate_limited',
  'timeout',
  'cancelled',
  'skipped',
  'running',
  'pending',
]

export function isTerminal(status: ExecutionStatus): boolean {
  return status !== 'running' && status !== 'pending'
}

/** How a dependency check state is presented. */
export interface CheckPresentation {
  label: string
  text: string
  swatch: string
  glyph: string
  /** Whether this state stops an execution. Mirrors store.Gates in Go. */
  gates: boolean
  meaning: string
}

const CHECKS: Record<CheckState, CheckPresentation> = {
  ok: {
    label: 'Healthy',
    text: 'text-st-success',
    swatch: 'bg-st-success',
    glyph: '✓',
    gates: false,
    meaning: 'Verified working.',
  },
  needs_login: {
    label: 'Needs login',
    text: 'text-st-blocked',
    swatch: 'bg-st-blocked',
    glyph: '⚿',
    gates: true,
    meaning: 'The credential expired. Tasks that require this will not run.',
  },
  misconfigured: {
    label: 'Misconfigured',
    text: 'text-st-failure',
    swatch: 'bg-st-failure',
    glyph: '✕',
    gates: true,
    meaning: 'Configuration is wrong or missing. Tasks that require this will not run.',
  },
  unavailable: {
    label: 'Unreachable',
    text: 'text-st-timeout',
    swatch: 'bg-st-timeout',
    glyph: '⚠',
    gates: false,
    meaning:
      'Could not be reached right now. Treated as transient, so it does not block runs.',
  },
  unknown: {
    label: 'Unknown',
    text: 'text-fg-muted',
    swatch: 'bg-st-empty',
    glyph: '?',
    gates: false,
    meaning: 'The probe returned something unrecognised. It does not block runs.',
  },
}

export function checkPresentation(state: CheckState): CheckPresentation {
  return CHECKS[state] ?? CHECKS.unknown
}

export function requirementKindLabel(kind: string): string {
  switch (kind) {
    case 'aws_profile':
      return 'AWS profile'
    case 'mcp_server':
      return 'MCP server'
    case 'binary':
      return 'Binary'
    default:
      return kind
  }
}
