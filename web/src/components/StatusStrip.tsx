import { Link } from 'react-router-dom'
import type { Execution } from '../api/types'
import { statusPresentation } from '../lib/status'
import { formatCost, formatDuration, formatRelative } from '../lib/format'

const SLOTS = 5

interface Props {
  executions: Execution[]
  /** Renders larger squares for detail headers. */
  size?: 'sm' | 'md'
}

/** The last five runs as a row of squares, oldest to newest.
 *
 * Fewer than five runs are left-padded with dashed placeholders so rows stay
 * aligned down a long task list, and the newest square carries a ring so
 * "now" is unambiguous. */
export function StatusStrip({ executions, size = 'sm' }: Props) {
  // The API returns newest-first; the strip reads left-to-right in time order.
  const recent = executions.slice(0, SLOTS).slice().reverse()
  const padding = Math.max(0, SLOTS - recent.length)

  const box = size === 'sm' ? 'h-2.5 w-2.5' : 'h-4 w-4'
  const radius = size === 'sm' ? 'rounded-[3px]' : 'rounded'

  return (
    <div
      className="flex items-center gap-1"
      role="group"
      aria-label={`Last ${SLOTS} runs`}
    >
      {Array.from({ length: padding }).map((_, i) => (
        <span
          key={`empty-${i}`}
          className={`${box} ${radius} border border-dashed border-border-strong`}
          aria-hidden="true"
        />
      ))}

      {recent.map((execution, index) => {
        const presentation = statusPresentation(execution.status)
        const isNewest = index === recent.length - 1
        const running = execution.status === 'running' || execution.status === 'pending'

        return (
          <Link
            key={execution.id}
            to={`/executions/${execution.id}`}
            title={tooltip(execution)}
            aria-label={tooltip(execution)}
            className={[
              box,
              radius,
              presentation.swatch,
              'relative block transition-transform hover:scale-125',
              running ? 'animate-soft-pulse' : '',
              isNewest ? 'ring-2 ring-offset-1 ring-border-strong ring-offset-surface' : '',
              // Blocked runs get a diagonal cut so they are distinguishable
              // from a failure without relying on hue alone.
              execution.status === 'blocked' ? 'blocked-slash' : '',
              execution.status === 'skipped' ? 'opacity-40' : '',
            ].join(' ')}
          />
        )
      })}
    </div>
  )
}

function tooltip(execution: Execution): string {
  const presentation = statusPresentation(execution.status)
  const parts = [
    `${presentation.label}, ${formatRelative(execution.finished_at ?? execution.queued_at)}`,
  ]
  if (execution.duration_ms) parts.push(formatDuration(execution.duration_ms))
  if (execution.total_cost_usd) parts.push(formatCost(execution.total_cost_usd))
  if (execution.status === 'blocked' && execution.error_message) {
    parts.push(execution.error_message)
  }
  return parts.join(' · ')
}
