import { Link, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { PauseCircle, Play, Pencil, ShieldAlert, Stethoscope } from 'lucide-react'

import { api } from '../api/client'
import { StatusStrip } from '../components/StatusStrip'
import { Button, Card, CardHeader, EmptyState, ErrorState, Field, PageHeader, Skeleton } from '../components/ui'
import { checkPresentation, requirementKindLabel, statusPresentation } from '../lib/status'
import {
  describeCron,
  flattenMarkdown,
  formatAbsolute,
  formatCost,
  formatDuration,
  formatRelative,
} from '../lib/format'

export function TaskDetail() {
  const { id } = useParams<{ id: string }>()
  const taskId = Number(id)
  const queryClient = useQueryClient()

  const taskQuery = useQuery({
    queryKey: ['task', taskId],
    queryFn: () => api.getTask(taskId),
    enabled: Number.isFinite(taskId),
    refetchInterval: 15_000,
  })

  const executionsQuery = useQuery({
    queryKey: ['executions', { taskId }],
    queryFn: () => api.listExecutions({ taskId, limit: 25 }),
    enabled: Number.isFinite(taskId),
    refetchInterval: 10_000,
  })

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['task', taskId] })
    queryClient.invalidateQueries({ queryKey: ['executions'] })
    queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const run = useMutation({ mutationFn: () => api.runTask(taskId), onSuccess: invalidate })
  const resume = useMutation({ mutationFn: () => api.resumeTask(taskId), onSuccess: invalidate })
  const preflight = useMutation({ mutationFn: () => api.preflightTask(taskId) })

  if (taskQuery.isPending) {
    return (
      <Card>
        <Skeleton rows={6} />
      </Card>
    )
  }
  if (taskQuery.isError || !taskQuery.data) {
    return (
      <ErrorState
        title="Could not load this schedule"
        error={taskQuery.error}
        onRetry={() => taskQuery.refetch()}
      />
    )
  }

  const task = taskQuery.data
  const executions = executionsQuery.data?.executions ?? []

  return (
    <>
      <PageHeader
        back={{ to: '/', label: 'Schedules' }}
        title={task.name}
        subtitle={task.description || describeCron(task.cron_expr)}
        actions={
          <>
            {task.paused ? (
              <Button onClick={() => resume.mutate()} loading={resume.isPending}>
                Resume
              </Button>
            ) : null}
            <Button onClick={() => preflight.mutate()} loading={preflight.isPending}>
              <Stethoscope className="h-3.5 w-3.5" />
              Check dependencies
            </Button>
            <Button variant="primary" onClick={() => run.mutate()} loading={run.isPending}>
              <Play className="h-3.5 w-3.5" />
              Run now
            </Button>
            <Link
              to={`/tasks/${task.id}/edit`}
              className="inline-flex items-center gap-1.5 rounded-md border border-border-strong bg-surface-2 px-3 py-1.5 text-sm hover:border-fg-faint"
            >
              <Pencil className="h-3.5 w-3.5" />
              Edit
            </Link>
          </>
        }
      />

      {run.isError ? (
        <div className="mb-4">
          <ErrorState title="Could not start the run" error={run.error} />
        </div>
      ) : null}

      {task.paused ? (
        <div className="mb-4 rounded-md border border-st-blocked/30 bg-st-blocked/5 p-3">
          <p className="flex items-center gap-1.5 text-sm font-medium text-st-blocked">
            <PauseCircle className="h-4 w-4" />
            This schedule is paused and will not run
          </p>
          {task.paused_reason ? (
            <p className="mt-1 text-xs text-fg-muted">{task.paused_reason}</p>
          ) : null}
        </div>
      ) : null}

      {task.bypass_permissions ? (
        <div className="mb-4 flex items-start gap-2 rounded-md border border-st-failure/30 bg-st-failure/5 p-3 text-xs">
          <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0 text-st-failure" />
          <span>
            <strong className="font-medium text-st-failure">
              Permission checks are bypassed.
            </strong>{' '}
            <span className="text-fg-muted">
              Every run can take any action as your user. The allowed-tools list is ignored.
            </span>
          </span>
        </div>
      ) : null}

      {preflight.data ? (
        <Card className="mb-4">
          <CardHeader
            title={
              preflight.data.would_run
                ? 'All dependencies are healthy'
                : 'Dependencies would block this run'
            }
            subtitle={
              preflight.data.would_run
                ? 'The next scheduled run will proceed.'
                : `Policy: ${preflight.data.gating_policy.replace('_', ' ')}`
            }
          />
          <ul className="divide-y divide-border-subtle">
            {preflight.data.results.map((check) => {
              const presentation = checkPresentation(check.state)
              return (
                <li
                  key={`${check.kind}-${check.target}`}
                  className="flex items-start gap-2 px-4 py-2.5 text-xs"
                >
                  <span className={presentation.text}>{presentation.glyph}</span>
                  <span className="font-medium">{check.target}</span>
                  <span className={presentation.text}>{presentation.label}</span>
                  <span className="ml-auto text-fg-faint">
                    {requirementKindLabel(check.kind)}
                  </span>
                </li>
              )
            })}
          </ul>
        </Card>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[1fr_18rem]">
        <Card className="min-w-0">
          <CardHeader
            title="Recent executions"
            subtitle={`${executions.length} shown`}
            actions={<StatusStrip executions={task.last_executions} />}
          />
          {executionsQuery.isPending ? (
            <Skeleton rows={4} />
          ) : executions.length === 0 ? (
            <EmptyState
              title="No runs yet"
              body="Trigger one with Run now, or wait for the schedule."
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
                        className={`h-2.5 w-2.5 shrink-0 rounded-[3px] ${presentation.swatch}`}
                        title={presentation.label}
                      />
                      <span className="w-20 shrink-0 text-xs font-medium">
                        {presentation.label}
                      </span>
                      <span className="min-w-0 flex-1 truncate text-xs text-fg-muted">
                        {flattenMarkdown(execution.result_text || execution.error_message) || '-'}
                      </span>
                      <span className="w-14 shrink-0 text-right text-xs tnum text-fg-muted">
                        {formatDuration(execution.duration_ms)}
                      </span>
                      <span className="w-14 shrink-0 text-right text-xs tnum text-fg-muted">
                        {formatCost(execution.total_cost_usd)}
                      </span>
                      <span className="w-16 shrink-0 text-right text-xs text-fg-faint">
                        {formatRelative(execution.finished_at ?? execution.queued_at)}
                      </span>
                    </Link>
                  </li>
                )
              })}
            </ul>
          )}
        </Card>

        <div className="space-y-4">
          <Card>
            <CardHeader title="Schedule" />
            <dl className="space-y-3 px-4 py-3">
              <Field label="Cron">
                <code className="font-mono text-xs">{task.cron_expr}</code>
              </Field>
              <Field label="In plain words">{describeCron(task.cron_expr)}</Field>
              <Field label="Timezone">{task.timezone}</Field>
              <Field label="Next run">
                {task.paused
                  ? 'paused'
                  : task.next_run_at
                    ? `${formatAbsolute(task.next_run_at)} (${formatRelative(task.next_run_at)})`
                    : '-'}
              </Field>
            </dl>
          </Card>

          <Card>
            <CardHeader title="Dependencies" subtitle="Checked before every run" />
            {task.requirements.length === 0 ? (
              <p className="px-4 py-4 text-xs text-fg-muted">
                None declared, so this task runs regardless of the environment.
              </p>
            ) : (
              <ul className="divide-y divide-border-subtle">
                {task.requirements.map((requirement) => (
                  <li key={requirement.id} className="px-4 py-2 text-xs">
                    <div className="flex items-center gap-2">
                      <span className="truncate font-medium">{requirement.target}</span>
                      <span className="ml-auto shrink-0 text-fg-faint">
                        {requirementKindLabel(requirement.kind)}
                      </span>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card>
            <CardHeader title="Configuration" />
            <dl className="space-y-3 px-4 py-3">
              <Field label="Model">{task.model || 'CLI default'}</Field>
              <Field label="Timeout">
                {task.timeout_seconds ? formatDuration(task.timeout_seconds * 1000) : 'default'}
              </Field>
              <Field label="Budget cap">
                {task.max_budget_usd ? formatCost(task.max_budget_usd) : 'default'}
              </Field>
              <Field label="On unhealthy dependency">
                {task.gating_policy.replace(/_/g, ' ')}
              </Field>
              <Field label="On overlap">{task.overlap_policy}</Field>
              {task.cwd ? (
                <Field label="Working directory">
                  <code className="font-mono text-xs">{task.cwd}</code>
                </Field>
              ) : null}
            </dl>
            {!task.bypass_permissions ? (
              <div className="space-y-3 border-t border-border-subtle px-4 py-3">
                <div>
                  <p className="mb-1.5 text-[11px] font-medium tracking-wide text-fg-faint uppercase">
                    Available tools
                  </p>
                  {(task.tools ?? []).length > 0 ? (
                    <div className="flex flex-wrap gap-1">
                      {(task.tools ?? []).map((tool) => (
                        <code
                          key={tool}
                          className="rounded border border-border-strong bg-surface-2 px-1.5 py-0.5 font-mono text-[11px]"
                        >
                          {tool}
                        </code>
                      ))}
                    </div>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      The CLI default set. Restrict this to guarantee the task cannot run
                      commands.
                    </p>
                  )}
                </div>
                <div>
                  <p className="mb-1.5 text-[11px] font-medium tracking-wide text-fg-faint uppercase">
                    Pre-approved actions
                  </p>
                  {task.allowed_tools.length > 0 ? (
                    <div className="flex flex-wrap gap-1">
                      {task.allowed_tools.map((tool) => (
                        <code
                          key={tool}
                          className="rounded border border-border-strong bg-surface-2 px-1.5 py-0.5 font-mono text-[11px]"
                        >
                          {tool}
                        </code>
                      ))}
                    </div>
                  ) : (
                    <p className="text-xs text-fg-muted">
                      None. Anything that would prompt is denied and reported on the
                      execution, where one click can approve it.
                    </p>
                  )}
                </div>
              </div>
            ) : null}
          </Card>

          <Card>
            <CardHeader title="Prompt" />
            <pre className="max-h-64 overflow-auto px-4 py-3 font-mono text-[11px] whitespace-pre-wrap">
              {task.prompt}
            </pre>
          </Card>
        </div>
      </div>
    </>
  )
}
