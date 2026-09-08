import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, ShieldAlert, Trash2 } from 'lucide-react'

import { api, ApiError } from '../api/client'
import type { CheckResult, GatingPolicy, OverlapPolicy, RequirementKind, TaskInput } from '../api/types'
import { Badge, Button, Card, CardHeader, ErrorState, PageHeader, Skeleton } from '../components/ui'
import { checkPresentation, requirementKindLabel } from '../lib/status'
import { describeCron } from '../lib/format'

interface RequirementDraft {
  kind: RequirementKind
  target: string
}

const GATING_HELP: Record<GatingPolicy, string> = {
  fail_fast:
    'Record a blocked run and notify. The failure stays visible in the history, and nothing is spent.',
  skip: 'Suppress the run quietly. Best for low-value tasks that would otherwise be noisy.',
  pause_schedule:
    'Block the run and pause the schedule, so it stops retrying until you fix the dependency.',
}

const OVERLAP_HELP: Record<OverlapPolicy, string> = {
  skip: 'Do not start if the previous run is still going.',
  queue: 'Wait for a free slot, then run.',
  parallel: 'Start regardless of what is already running.',
}

export function TaskForm() {
  const { id } = useParams<{ id: string }>()
  const editing = Boolean(id)
  const taskId = Number(id)
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const existing = useQuery({
    queryKey: ['task', taskId],
    queryFn: () => api.getTask(taskId),
    enabled: editing,
  })

  const [form, setForm] = useState<TaskInput>({
    name: '',
    cron_expr: '0 * * * *',
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    prompt: '',
    model: 'sonnet',
    timeout_seconds: 1800,
    max_budget_usd: 1,
    allowed_tools: [],
    bypass_permissions: false,
    overlap_policy: 'skip',
    gating_policy: 'fail_fast',
    enabled: true,
  })
  const [requirements, setRequirements] = useState<RequirementDraft[]>([])
  const [toolsText, setToolsText] = useState('')
  const [toolSetText, setToolSetText] = useState('')

  // Seed the form once the existing task loads.
  useEffect(() => {
    const task = existing.data
    if (!task) return
    setForm({
      name: task.name,
      description: task.description,
      cron_expr: task.cron_expr,
      timezone: task.timezone,
      prompt: task.prompt,
      model: task.model,
      cwd: task.cwd,
      timeout_seconds: task.timeout_seconds,
      max_budget_usd: task.max_budget_usd,
      allowed_tools: task.allowed_tools,
      bypass_permissions: task.bypass_permissions,
      overlap_policy: task.overlap_policy,
      gating_policy: task.gating_policy,
      enabled: task.enabled,
    })
    setRequirements(task.requirements.map((r) => ({ kind: r.kind, target: r.target })))
    setToolsText(task.allowed_tools.join('\n'))
    setToolSetText((task.tools ?? []).join(', '))
  }, [existing.data])

  const save = useMutation({
    mutationFn: (input: TaskInput) =>
      editing ? api.updateTask(taskId, input) : api.createTask(input),
    onSuccess: (task) => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: ['task', task.id] })
      navigate(`/tasks/${task.id}`)
    },
  })

  const remove = useMutation({
    mutationFn: () => api.deleteTask(taskId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      navigate('/')
    },
  })

  const submit = (event: React.FormEvent) => {
    event.preventDefault()
    const tools = toolsText
      .split('\n')
      .map((t) => t.trim())
      .filter(Boolean)
    const toolSet = toolSetText
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean)
    save.mutate({
      ...form,
      allowed_tools: tools,
      tools: toolSet,
      requirements: requirements
        .filter((r) => r.target.trim())
        .map((r) => ({ kind: r.kind, target: r.target.trim() })),
    })
  }

  if (editing && existing.isPending) {
    return (
      <Card>
        <Skeleton rows={8} />
      </Card>
    )
  }
  if (editing && existing.isError) {
    return <ErrorState title="Could not load this schedule" error={existing.error} />
  }

  return (
    <form onSubmit={submit}>
      <PageHeader
        back={editing ? { to: `/tasks/${taskId}`, label: 'Back to schedule' } : { to: '/', label: 'Schedules' }}
        title={editing ? `Edit ${existing.data?.name ?? ''}` : 'New schedule'}
        actions={
          <>
            {editing ? (
              <Button
                type="button"
                variant="danger"
                onClick={() => {
                  if (confirm('Delete this schedule and all of its execution history?')) {
                    remove.mutate()
                  }
                }}
                loading={remove.isPending}
              >
                <Trash2 className="h-3.5 w-3.5" />
                Delete
              </Button>
            ) : null}
            <Button type="submit" variant="primary" loading={save.isPending}>
              {editing ? 'Save changes' : 'Create schedule'}
            </Button>
          </>
        }
      />

      {save.isError ? (
        <div className="mb-4">
          <ErrorState
            title={
              save.error instanceof ApiError && save.error.problems.length > 0
                ? 'Please fix these fields'
                : 'Could not save'
            }
            error={save.error}
          />
        </div>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
        <div className="space-y-4">
          <Card>
            <CardHeader title="What it does" />
            <div className="space-y-3 p-4">
              <Labeled label="Name" hint="Shown in the schedule list and notifications.">
                <input
                  required
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="aws-canary"
                  className={inputClass}
                />
              </Labeled>

              <Labeled label="Description" optional>
                <input
                  value={form.description ?? ''}
                  onChange={(e) => setForm({ ...form, description: e.target.value })}
                  placeholder="Confirms the admin SSO profile still works."
                  className={inputClass}
                />
              </Labeled>

              <Labeled
                label="Prompt"
                hint="Passed to the claude CLI on stdin. Write it as you would type it into Claude Code."
              >
                <textarea
                  required
                  rows={6}
                  value={form.prompt}
                  onChange={(e) => setForm({ ...form, prompt: e.target.value })}
                  placeholder="Run aws sts get-caller-identity --profile admin and report the account."
                  className={`${inputClass} resize-y font-mono text-xs`}
                />
              </Labeled>
            </div>
          </Card>

          <Card>
            <CardHeader
              title="Dependencies"
              subtitle="Checked before every run. If one needs attention, the run is refused instead of spending tokens against a broken environment."
              actions={
                <Button
                  type="button"
                  size="sm"
                  onClick={() =>
                    setRequirements([...requirements, { kind: 'aws_profile', target: '' }])
                  }
                >
                  <Plus className="h-3 w-3" />
                  Add
                </Button>
              }
            />
            <RequirementEditor value={requirements} onChange={setRequirements} />
          </Card>

          <Card>
            <CardHeader
              title="Tools and permissions"
              subtitle="Two different levers. Restricting the tool set is the hard limit; the allowlist only pre-approves things that would otherwise ask."
            />
            <div className="space-y-3 p-4">
              <Labeled
                label="Available tools"
                hint="Comma-separated, for example: Bash, Read, Grep. Leave empty for the CLI default set. This removes tools outright, so it is the only way to guarantee a task cannot run commands."
                optional
              >
                <input
                  value={toolSetText}
                  onChange={(e) => setToolSetText(e.target.value)}
                  placeholder="Bash, Read, Grep"
                  disabled={form.bypass_permissions}
                  className={`${inputClass} font-mono text-xs disabled:opacity-50`}
                />
              </Labeled>

              <Labeled
                label="Pre-approved actions"
                hint="One rule per line, for example Bash(aws *). Nobody can answer a prompt on a scheduled run, so anything that would ask and is not listed here is denied and reported on the execution, where one click adds it and re-runs."
                optional
              >
                <textarea
                  rows={4}
                  value={toolsText}
                  onChange={(e) => setToolsText(e.target.value)}
                  placeholder={'Bash(aws *)\nRead'}
                  disabled={form.bypass_permissions}
                  className={`${inputClass} resize-y font-mono text-xs disabled:opacity-50`}
                />
              </Labeled>

              <label className="flex cursor-pointer items-start gap-2.5 rounded-md border border-st-failure/30 bg-st-failure/5 p-3">
                <input
                  type="checkbox"
                  checked={form.bypass_permissions ?? false}
                  onChange={(e) => setForm({ ...form, bypass_permissions: e.target.checked })}
                  className="mt-0.5"
                />
                <span className="text-xs">
                  <span className="flex items-center gap-1.5 font-medium text-st-failure">
                    <ShieldAlert className="h-3.5 w-3.5" />
                    Bypass all permission checks
                  </span>
                  <span className="mt-0.5 block text-fg-muted">
                    Runs with <code className="font-mono">--dangerously-skip-permissions</code>.
                    Claude can take any action as your user, including deleting files and
                    calling the network. Nothing will block, and both fields above are ignored.
                  </span>
                </span>
              </label>
            </div>
          </Card>
        </div>

        <div className="space-y-4">
          <Card>
            <CardHeader title="Schedule" />
            <div className="space-y-3 p-4">
              <Labeled label="Cron expression">
                <input
                  required
                  value={form.cron_expr}
                  onChange={(e) => setForm({ ...form, cron_expr: e.target.value })}
                  placeholder="0 * * * *"
                  className={`${inputClass} font-mono`}
                />
              </Labeled>
              <p className="rounded bg-surface-2 px-2 py-1.5 text-xs text-fg-muted">
                {describeCron(form.cron_expr)}
              </p>

              <Labeled label="Timezone">
                <input
                  value={form.timezone ?? ''}
                  onChange={(e) => setForm({ ...form, timezone: e.target.value })}
                  placeholder="America/Santiago"
                  className={inputClass}
                />
              </Labeled>

              <label className="flex items-center gap-2 text-xs">
                <input
                  type="checkbox"
                  checked={form.enabled ?? true}
                  onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
                />
                Enabled
              </label>
            </div>
          </Card>

          <Card>
            <CardHeader title="Limits" />
            <div className="space-y-3 p-4">
              <Labeled label="Model" optional>
                <select
                  value={form.model ?? ''}
                  onChange={(e) => setForm({ ...form, model: e.target.value })}
                  className={inputClass}
                >
                  <option value="">CLI default</option>
                  <option value="opus">opus</option>
                  <option value="sonnet">sonnet</option>
                  <option value="haiku">haiku</option>
                </select>
              </Labeled>

              <Labeled label="Timeout (seconds)">
                <input
                  type="number"
                  min={0}
                  value={form.timeout_seconds ?? 0}
                  onChange={(e) =>
                    setForm({ ...form, timeout_seconds: Number(e.target.value) })
                  }
                  className={`${inputClass} tnum`}
                />
              </Labeled>

              <Labeled label="Budget cap (USD)" hint="Passed as --max-budget-usd.">
                <input
                  type="number"
                  min={0}
                  step={0.05}
                  value={form.max_budget_usd ?? 0}
                  onChange={(e) =>
                    setForm({ ...form, max_budget_usd: Number(e.target.value) })
                  }
                  className={`${inputClass} tnum`}
                />
              </Labeled>

              <Labeled label="Working directory" optional>
                <input
                  value={form.cwd ?? ''}
                  onChange={(e) => setForm({ ...form, cwd: e.target.value })}
                  placeholder="/home/you/project"
                  className={`${inputClass} font-mono text-xs`}
                />
              </Labeled>
            </div>
          </Card>

          <Card>
            <CardHeader title="Policies" />
            <div className="space-y-3 p-4">
              <Labeled label="If a dependency is unhealthy">
                <select
                  value={form.gating_policy ?? 'fail_fast'}
                  onChange={(e) =>
                    setForm({ ...form, gating_policy: e.target.value as GatingPolicy })
                  }
                  className={inputClass}
                >
                  <option value="fail_fast">Block the run</option>
                  <option value="skip">Skip quietly</option>
                  <option value="pause_schedule">Block and pause the schedule</option>
                </select>
              </Labeled>
              <p className="text-xs text-fg-muted">
                {GATING_HELP[form.gating_policy ?? 'fail_fast']}
              </p>

              <Labeled label="If the previous run is still going">
                <select
                  value={form.overlap_policy ?? 'skip'}
                  onChange={(e) =>
                    setForm({ ...form, overlap_policy: e.target.value as OverlapPolicy })
                  }
                  className={inputClass}
                >
                  <option value="skip">Skip this one</option>
                  <option value="queue">Queue it</option>
                  <option value="parallel">Run in parallel</option>
                </select>
              </Labeled>
              <p className="text-xs text-fg-muted">
                {OVERLAP_HELP[form.overlap_policy ?? 'skip']}
              </p>
            </div>
          </Card>
        </div>
      </div>
    </form>
  )
}

const inputClass =
  'w-full rounded-md border border-border-subtle bg-bg px-2.5 py-1.5 text-sm outline-none transition focus:border-accent'

function Labeled({
  label,
  hint,
  optional,
  children,
}: {
  label: string
  hint?: string
  optional?: boolean
  children: React.ReactNode
}) {
  return (
    <label className="block">
      <span className="mb-1 flex items-center gap-1.5 text-xs font-medium">
        {label}
        {optional ? <span className="text-fg-faint">optional</span> : null}
      </span>
      {children}
      {hint ? <span className="mt-1 block text-xs text-fg-muted">{hint}</span> : null}
    </label>
  )
}

/** Requirement rows, each showing that dependency's live status inline, so a
 * broken selection is visible at authoring time rather than at 3am. */
function RequirementEditor({
  value,
  onChange,
}: {
  value: RequirementDraft[]
  onChange: (next: RequirementDraft[]) => void
}) {
  const snapshot = useQuery({
    queryKey: ['health', 'snapshot'],
    queryFn: () => api.healthSnapshot(false),
  })

  const byKind = snapshot.data?.kinds ?? {}

  const lookup = (kind: string, target: string): CheckResult | undefined =>
    (byKind[kind] ?? []).find((r) => r.target === target)

  if (value.length === 0) {
    return (
      <p className="px-4 py-6 text-center text-xs text-fg-muted">
        No dependencies declared. This task will run whatever the state of the environment.
      </p>
    )
  }

  return (
    <ul className="divide-y divide-border-subtle">
      {value.map((requirement, index) => {
        const options = byKind[requirement.kind] ?? []
        const current = lookup(requirement.kind, requirement.target)
        const presentation = current ? checkPresentation(current.state) : undefined

        return (
          <li key={index} className="flex flex-wrap items-center gap-2 px-4 py-2.5">
            <select
              value={requirement.kind}
              onChange={(e) => {
                const next = [...value]
                next[index] = { kind: e.target.value as RequirementKind, target: '' }
                onChange(next)
              }}
              className="rounded-md border border-border-subtle bg-bg px-2 py-1 text-xs outline-none focus:border-accent"
            >
              <option value="aws_profile">{requirementKindLabel('aws_profile')}</option>
              <option value="mcp_server">{requirementKindLabel('mcp_server')}</option>
              <option value="binary">{requirementKindLabel('binary')}</option>
            </select>

            <input
              list={`targets-${requirement.kind}`}
              value={requirement.target}
              onChange={(e) => {
                const next = [...value]
                next[index] = { ...requirement, target: e.target.value }
                onChange(next)
              }}
              placeholder={
                requirement.kind === 'aws_profile'
                  ? 'admin'
                  : requirement.kind === 'mcp_server'
                    ? 'claude.ai Slack'
                    : 'aws'
              }
              className="min-w-40 flex-1 rounded-md border border-border-subtle bg-bg px-2 py-1 text-xs outline-none focus:border-accent"
            />
            <datalist id={`targets-${requirement.kind}`}>
              {options.map((option) => (
                <option key={option.target} value={option.target} />
              ))}
            </datalist>

            {presentation ? (
              <span
                className={`shrink-0 text-xs ${presentation.text}`}
                title={current?.detail || presentation.meaning}
              >
                {presentation.glyph} {presentation.label}
              </span>
            ) : requirement.target ? (
              <Badge title="Not among the dependencies discovered on this machine">
                unknown
              </Badge>
            ) : null}

            <button
              type="button"
              onClick={() => onChange(value.filter((_, i) => i !== index))}
              className="shrink-0 rounded p-1 text-fg-faint hover:bg-surface-2 hover:text-st-failure"
              aria-label="Remove dependency"
            >
              <Trash2 className="h-3.5 w-3.5" />
            </button>
          </li>
        )
      })}
    </ul>
  )
}
