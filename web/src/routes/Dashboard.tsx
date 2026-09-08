import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Play, PauseCircle, ShieldAlert, Zap } from 'lucide-react'

import { api } from '../api/client'
import type { Task } from '../api/types'
import { StatusStrip } from '../components/StatusStrip'
import { Badge, Button, Card, EmptyState, ErrorState, PageHeader, Skeleton } from '../components/ui'
import { describeCron, formatCost, formatRelative, pluralize } from '../lib/format'
import { requirementKindLabel } from '../lib/status'

export function Dashboard() {
  const queryClient = useQueryClient()

  const tasksQuery = useQuery({
    queryKey: ['tasks'],
    queryFn: api.listTasks,
    // Keeps in-flight runs and next-run countdowns current.
    refetchInterval: 10_000,
  })

  const runNow = useMutation({
    mutationFn: (id: number) => api.runTask(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: ['executions'] })
    },
  })

  const resume = useMutation({
    mutationFn: (id: number) => api.resumeTask(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['tasks'] }),
  })

  const tasks = tasksQuery.data?.tasks ?? []
  const spend = tasks.reduce(
    (sum, task) => sum + task.last_executions.reduce((s, e) => s + e.total_cost_usd, 0),
    0,
  )

  return (
    <>
      <PageHeader
        title="Schedules"
        subtitle={
          tasks.length > 0
            ? `${pluralize(tasks.length, 'task')} · ${formatCost(spend)} across the runs shown`
            : 'Cron-scheduled Claude Code tasks, checked before they run.'
        }
      />

      {runNow.isError ? (
        <div className="mb-4">
          <ErrorState title="Could not start the run" error={runNow.error} />
        </div>
      ) : null}

      <Card>
        {tasksQuery.isPending ? (
          <Skeleton rows={4} />
        ) : tasksQuery.isError ? (
          <ErrorState
            title="Could not load schedules"
            error={tasksQuery.error}
            onRetry={() => tasksQuery.refetch()}
          />
        ) : tasks.length === 0 ? (
          <EmptyState
            title="No schedules yet"
            body="Create one to run a Claude Code prompt on a cron schedule. Declare what it depends on (an AWS profile, an MCP server, a binary) and it will refuse to run against a broken environment."
            action={
              <Link
                to="/tasks/new"
                className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white hover:opacity-90"
              >
                New schedule
              </Link>
            }
          />
        ) : (
          <ul className="divide-y divide-border-subtle">
            {tasks.map((task) => (
              <TaskRow
                key={task.id}
                task={task}
                onRun={() => runNow.mutate(task.id)}
                onResume={() => resume.mutate(task.id)}
                running={runNow.isPending && runNow.variables === task.id}
                resuming={resume.isPending && resume.variables === task.id}
              />
            ))}
          </ul>
        )}
      </Card>

      <p className="mt-3 text-xs text-fg-faint">
        Each square is one of the last five runs, oldest on the left. Click one to open it.
      </p>
    </>
  )
}

function TaskRow({
  task,
  onRun,
  onResume,
  running,
  resuming,
}: {
  task: Task
  onRun: () => void
  onResume: () => void
  running: boolean
  resuming: boolean
}) {
  const gatingRequirements = task.requirements.length

  return (
    <li className="group px-4 py-3 transition hover:bg-surface-2/50">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <Link
              to={`/tasks/${task.id}`}
              className="truncate text-sm font-medium hover:text-accent"
            >
              {task.name}
            </Link>

            {task.paused ? (
              <Badge tone="warn" title={task.paused_reason}>
                <PauseCircle className="h-3 w-3" />
                Paused
              </Badge>
            ) : null}
            {!task.enabled ? <Badge>Disabled</Badge> : null}
            {task.bypass_permissions ? (
              <Badge
                tone="danger"
                title="This task runs with all permission checks bypassed. Claude can take any action as your user."
              >
                <ShieldAlert className="h-3 w-3" />
                Bypass
              </Badge>
            ) : null}
          </div>

          <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
            <span title={`${task.cron_expr} (${task.timezone})`}>
              {describeCron(task.cron_expr)}
            </span>
            {task.paused ? (
              <span className="text-st-blocked">Will not run while paused</span>
            ) : task.next_run_at ? (
              <span title={new Date(task.next_run_at).toString()}>
                next {formatRelative(task.next_run_at)}
              </span>
            ) : null}
            {gatingRequirements > 0 ? (
              <span
                title={task.requirements
                  .map((r) => `${requirementKindLabel(r.kind)}: ${r.target}`)
                  .join('\n')}
                className="inline-flex items-center gap-1"
              >
                <Zap className="h-3 w-3" />
                {pluralize(gatingRequirements, 'dependency', 'dependencies')}
              </span>
            ) : null}
          </div>

          {task.paused && task.paused_reason ? (
            <p className="mt-1.5 line-clamp-2 text-xs text-st-blocked">
              {task.paused_reason}
            </p>
          ) : null}
        </div>

        <StatusStrip executions={task.last_executions} />

        <div className="flex items-center gap-1.5 opacity-60 transition group-hover:opacity-100 focus-within:opacity-100">
          {task.paused ? (
            <Button size="sm" onClick={onResume} loading={resuming}>
              Resume
            </Button>
          ) : null}
          <Button size="sm" onClick={onRun} loading={running} title="Run now">
            <Play className="h-3 w-3" />
            Run
          </Button>
        </div>
      </div>
    </li>
  )
}
