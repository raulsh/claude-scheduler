import { Link, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Search, X } from 'lucide-react'

import { api } from '../api/client'
import type { ExecutionStatus } from '../api/types'
import { Card, EmptyState, ErrorState, PageHeader, Skeleton } from '../components/ui'
import { ALL_STATUSES, statusPresentation } from '../lib/status'
import { flattenMarkdown, formatCost, formatDuration, formatRelative } from '../lib/format'

/** The executions list. Every filter lives in the query string, so a filtered
 * view can be bookmarked or shared and survives a reload. */
export function ExecutionsList() {
  const [params, setParams] = useSearchParams()

  const statuses = params.getAll('status')
  const taskId = params.get('task_id')
  const search = params.get('q') ?? ''
  const cursor = params.get('cursor') ?? undefined

  const tasksQuery = useQuery({ queryKey: ['tasks'], queryFn: api.listTasks })

  const executionsQuery = useQuery({
    queryKey: ['executions', { statuses, taskId, search, cursor }],
    queryFn: () =>
      api.listExecutions({
        statuses: statuses.length ? statuses : undefined,
        taskId: taskId ? Number(taskId) : undefined,
        q: search || undefined,
        cursor,
        limit: 50,
      }),
    refetchInterval: 15_000,
  })

  const update = (mutate: (next: URLSearchParams) => void) => {
    const next = new URLSearchParams(params)
    mutate(next)
    // Any filter change invalidates the pagination position.
    next.delete('cursor')
    setParams(next, { replace: true })
  }

  const toggleStatus = (status: ExecutionStatus) =>
    update((next) => {
      const current = next.getAll('status')
      next.delete('status')
      const remaining = current.includes(status)
        ? current.filter((s) => s !== status)
        : [...current, status]
      remaining.forEach((s) => next.append('status', s))
    })

  const executions = executionsQuery.data?.executions ?? []
  const nextCursor = executionsQuery.data?.next_cursor
  const filtersActive = statuses.length > 0 || Boolean(taskId) || Boolean(search)

  return (
    <>
      <PageHeader
        title="Executions"
        subtitle="Every run, including the ones that were blocked before they started."
      />

      <Card className="mb-4">
        <div className="flex flex-wrap items-center gap-2 p-3">
          <div className="relative min-w-48 flex-1">
            <Search className="absolute top-1/2 left-2.5 h-3.5 w-3.5 -translate-y-1/2 text-fg-faint" />
            <input
              value={search}
              onChange={(e) =>
                update((next) => {
                  if (e.target.value) next.set('q', e.target.value)
                  else next.delete('q')
                })
              }
              placeholder="Search results, errors, task names…"
              className="w-full rounded-md border border-border-subtle bg-bg py-1.5 pr-2 pl-8 text-sm outline-none focus:border-accent"
            />
          </div>

          <select
            value={taskId ?? ''}
            onChange={(e) =>
              update((next) => {
                if (e.target.value) next.set('task_id', e.target.value)
                else next.delete('task_id')
              })
            }
            className="rounded-md border border-border-subtle bg-bg px-2 py-1.5 text-sm outline-none focus:border-accent"
          >
            <option value="">All schedules</option>
            {(tasksQuery.data?.tasks ?? []).map((task) => (
              <option key={task.id} value={task.id}>
                {task.name}
              </option>
            ))}
          </select>

          {filtersActive ? (
            <button
              onClick={() => setParams(new URLSearchParams(), { replace: true })}
              className="inline-flex items-center gap-1 rounded-md px-2 py-1.5 text-xs text-fg-muted hover:bg-surface-2 hover:text-fg"
            >
              <X className="h-3 w-3" />
              Clear
            </button>
          ) : null}
        </div>

        <div className="flex flex-wrap gap-1.5 border-t border-border-subtle px-3 py-2">
          {ALL_STATUSES.map((status) => {
            const presentation = statusPresentation(status)
            const active = statuses.includes(status)
            return (
              <button
                key={status}
                onClick={() => toggleStatus(status)}
                title={presentation.meaning}
                className={`inline-flex items-center gap-1.5 rounded border px-2 py-0.5 text-xs transition ${
                  active
                    ? 'border-accent bg-accent-soft text-accent'
                    : 'border-border-subtle text-fg-muted hover:border-border-strong'
                }`}
              >
                <span className={`h-2 w-2 rounded-sm ${presentation.swatch}`} aria-hidden />
                {presentation.label}
              </button>
            )
          })}
        </div>
      </Card>

      <Card>
        {executionsQuery.isPending ? (
          <Skeleton rows={6} />
        ) : executionsQuery.isError ? (
          <ErrorState
            title="Could not load executions"
            error={executionsQuery.error}
            onRetry={() => executionsQuery.refetch()}
          />
        ) : executions.length === 0 ? (
          <EmptyState
            title={filtersActive ? 'No executions match these filters' : 'No executions yet'}
            body={
              filtersActive
                ? 'Try clearing a filter, or widening the status selection.'
                : 'Runs will appear here once a schedule fires or you trigger one by hand.'
            }
          />
        ) : (
          <ul className="divide-y divide-border-subtle">
            {executions.map((execution) => {
              const presentation = statusPresentation(execution.status)
              return (
                <li key={execution.id}>
                  <Link
                    to={`/executions/${execution.id}`}
                    className="flex items-center gap-3 px-4 py-2.5 transition hover:bg-surface-2/50"
                  >
                    <span
                      className={`h-2.5 w-2.5 shrink-0 rounded-[3px] ${presentation.swatch} ${
                        execution.status === 'running' ? 'animate-soft-pulse' : ''
                      }`}
                      title={presentation.label}
                      aria-label={presentation.label}
                    />
                    <span className="w-24 shrink-0 truncate text-xs font-medium">
                      {presentation.label}
                    </span>
                    <span className="min-w-0 flex-1 truncate text-sm">
                      {execution.task_name || `Execution ${execution.id}`}
                      {execution.result_text ? (
                        <span className="ml-2 text-fg-muted">
                          {flattenMarkdown(execution.result_text)}
                        </span>
                      ) : execution.error_message ? (
                        <span className="ml-2 text-fg-muted">
                          {flattenMarkdown(execution.error_message)}
                        </span>
                      ) : null}
                    </span>
                    <span className="hidden w-14 shrink-0 text-right text-xs text-fg-muted sm:block">
                      {execution.trigger}
                    </span>
                    <span className="w-16 shrink-0 text-right text-xs tnum text-fg-muted">
                      {formatDuration(execution.duration_ms)}
                    </span>
                    <span className="w-16 shrink-0 text-right text-xs tnum text-fg-muted">
                      {formatCost(execution.total_cost_usd)}
                    </span>
                    <span className="w-20 shrink-0 text-right text-xs text-fg-faint">
                      {formatRelative(execution.finished_at ?? execution.queued_at)}
                    </span>
                  </Link>
                </li>
              )
            })}
          </ul>
        )}

        {nextCursor ? (
          <div className="border-t border-border-subtle p-3 text-center">
            <button
              onClick={() =>
                setParams(
                  (prev) => {
                    const next = new URLSearchParams(prev)
                    next.set('cursor', nextCursor)
                    return next
                  },
                  { replace: false },
                )
              }
              className="text-xs font-medium text-accent hover:underline"
            >
              Load older executions
            </button>
          </div>
        ) : null}
      </Card>
    </>
  )
}
