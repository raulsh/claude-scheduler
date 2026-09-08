import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, Radio, ShieldCheck } from 'lucide-react'

import { api } from '../api/client'
import type { CheckResult } from '../api/types'
import { Badge, Button, Card, CardHeader, ErrorState, Field, PageHeader, Skeleton } from '../components/ui'
import { Transcript } from '../components/Transcript'
import { buildEntries, useTranscript } from '../lib/useTranscript'
import { checkPresentation, isTerminal, requirementKindLabel, statusPresentation } from '../lib/status'
import { formatAbsolute, formatCost, formatDuration, formatTokens } from '../lib/format'

export function ExecutionDetail() {
  const { id } = useParams<{ id: string }>()
  const executionId = Number(id)
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const executionQuery = useQuery({
    queryKey: ['execution', executionId],
    queryFn: () => api.getExecution(executionId),
    enabled: Number.isFinite(executionId),
    // Poll only while the run could still change.
    refetchInterval: (query) =>
      query.state.data && isTerminal(query.state.data.status) ? false : 3000,
  })

  const execution = executionQuery.data
  const live = execution ? !isTerminal(execution.status) : false

  // A finished run is replayed from stored events; a live one streams.
  const storedQuery = useQuery({
    queryKey: ['execution', executionId, 'events'],
    queryFn: () => api.listEvents(executionId),
    enabled: Boolean(execution) && !live,
  })

  const stream = useTranscript(executionId, live)
  const storedEntries = useMemo(
    () => buildEntries(storedQuery.data?.events ?? []),
    [storedQuery.data],
  )
  const entries = live ? stream.entries : storedEntries

  // When the stream reports a terminal status, refresh the record so the
  // header stops saying "running".
  useEffect(() => {
    if (stream.terminalStatus) {
      queryClient.invalidateQueries({ queryKey: ['execution', executionId] })
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
    }
  }, [stream.terminalStatus, executionId, queryClient])

  const cancel = useMutation({
    mutationFn: () => api.cancelExecution(executionId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['execution', executionId] }),
  })

  // The approval loop: adopt the rules this run was denied, then start
  // again. Same end state as approving mid-run, at the cost of a re-run.
  const approve = useMutation({
    mutationFn: () => api.rerunWithAllowlist(executionId),
    onSuccess: (result) => {
      queryClient.invalidateQueries({ queryKey: ['tasks'] })
      queryClient.invalidateQueries({ queryKey: ['executions'] })
      navigate(`/executions/${result.execution.id}`)
    },
  })

  if (executionQuery.isPending) {
    return (
      <Card>
        <Skeleton rows={6} />
      </Card>
    )
  }
  if (executionQuery.isError || !execution) {
    return (
      <ErrorState
        title="Could not load this execution"
        error={executionQuery.error}
        onRetry={() => executionQuery.refetch()}
      />
    )
  }

  const presentation = statusPresentation(execution.status)

  return (
    <>
      <PageHeader
        back={{ to: '/executions', label: 'All executions' }}
        title={
          <span className="flex items-center gap-2.5">
            <span className={`h-3 w-3 rounded ${presentation.swatch}`} aria-hidden />
            <span>
              {execution.task_name || 'Execution'} #{execution.id}
            </span>
          </span>
        }
        subtitle={presentation.meaning}
        actions={
          <>
            {live ? (
              <Button variant="danger" size="sm" onClick={() => cancel.mutate()} loading={cancel.isPending}>
                <Ban className="h-3 w-3" />
                Cancel
              </Button>
            ) : null}
            {execution.task_id ? (
              <Link
                to={`/tasks/${execution.task_id}`}
                className="rounded-md border border-border-strong bg-surface-2 px-3 py-1.5 text-sm hover:border-fg-faint"
              >
                View schedule
              </Link>
            ) : null}
          </>
        }
      />

      {cancel.isError ? (
        <div className="mb-4">
          <ErrorState title="Could not cancel" error={cancel.error} />
        </div>
      ) : null}

      {execution.error_message ? (
        <div
          className={`mb-4 rounded-md border p-3 text-sm ${
            execution.status === 'blocked'
              ? 'border-st-blocked/30 bg-st-blocked/5'
              : execution.status === 'rate_limited'
                ? 'border-st-rate/30 bg-st-rate/5'
                : 'border-st-failure/30 bg-st-failure/5'
          }`}
        >
          <div className="mb-1 text-[11px] font-medium tracking-wide uppercase">
            {presentation.label}
          </div>
          <p className="whitespace-pre-wrap">{execution.error_message}</p>
        </div>
      ) : null}

      {execution.permission_denials.length > 0 ? (
        <Card className="mb-4 border-st-blocked/30">
          <CardHeader
            title="Tools this run was not allowed to use"
            subtitle="Scheduled runs deny anything outside their allowlist rather than waiting for an approval nobody is there to give."
          />
          <div className="flex flex-wrap gap-1.5 px-4 py-3">
            {execution.permission_denials.map((rule) => (
              <code
                key={rule}
                className="rounded border border-border-strong bg-surface-2 px-1.5 py-0.5 font-mono text-xs"
              >
                {rule}
              </code>
            ))}
          </div>
          {execution.task_id ? (
            <div className="flex flex-wrap items-center gap-2 border-t border-border-subtle px-4 py-2.5 text-xs">
              <Button
                variant="primary"
                size="sm"
                onClick={() => approve.mutate()}
                loading={approve.isPending}
              >
                <ShieldCheck className="h-3 w-3" />
                Allow these and re-run
              </Button>
              <span className="text-fg-muted">
                Adds the rules above to this schedule&apos;s allowlist, then starts a fresh run.
              </span>
              <Link
                to={`/tasks/${execution.task_id}/edit`}
                className="ml-auto text-accent hover:underline"
              >
                Edit by hand
              </Link>
            </div>
          ) : null}
          {approve.isError ? (
            <ErrorState title="Could not re-run" error={approve.error} />
          ) : null}
        </Card>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[1fr_18rem]">
        <Card className="min-w-0">
          <CardHeader
            title="Transcript"
            subtitle={`${entries.length} entries`}
            actions={
              live ? (
                <span className="inline-flex items-center gap-1.5 text-xs">
                  <Radio
                    className={`h-3 w-3 ${
                      stream.connection === 'live'
                        ? 'animate-soft-pulse text-st-running'
                        : stream.connection === 'error'
                          ? 'text-st-timeout'
                          : 'text-fg-faint'
                    }`}
                  />
                  <span className="text-fg-muted">
                    {stream.connection === 'live'
                      ? 'Live'
                      : stream.connection === 'error'
                        ? 'Reconnecting…'
                        : stream.connection === 'connecting'
                          ? 'Connecting…'
                          : 'Closed'}
                  </span>
                </span>
              ) : null
            }
          />
          {storedQuery.isPending && !live ? (
            <Skeleton rows={4} />
          ) : (
            <Transcript entries={entries} live={live} />
          )}
        </Card>

        <div className="space-y-4">
          <Card>
            <CardHeader title="Run" />
            <dl className="grid grid-cols-2 gap-3 px-4 py-3">
              <Field label="Status">
                <span className={presentation.text}>
                  {presentation.glyph} {presentation.label}
                </span>
              </Field>
              <Field label="Trigger">{execution.trigger}</Field>
              <Field label="Duration">{formatDuration(execution.duration_ms)}</Field>
              <Field label="Cost">{formatCost(execution.total_cost_usd)}</Field>
              <Field label="Turns">{execution.num_turns || '-'}</Field>
              <Field label="Model">{execution.model || '-'}</Field>
              <Field label="Queued">{formatAbsolute(execution.queued_at, true)}</Field>
              <Field label="Finished">{formatAbsolute(execution.finished_at, true)}</Field>
            </dl>
          </Card>

          {execution.input_tokens + execution.output_tokens + execution.cache_read_tokens > 0 ? (
            <Card>
              <CardHeader title="Tokens" />
              <dl className="grid grid-cols-2 gap-3 px-4 py-3">
                <Field label="Input">{formatTokens(execution.input_tokens)}</Field>
                <Field label="Output">{formatTokens(execution.output_tokens)}</Field>
                <Field label="Cache read">{formatTokens(execution.cache_read_tokens)}</Field>
                <Field label="Cache write">{formatTokens(execution.cache_creation_tokens)}</Field>
              </dl>
            </Card>
          ) : null}

          {execution.checks && execution.checks.length > 0 ? (
            <ChecksCard checks={execution.checks} />
          ) : null}

          {execution.mcp_snapshot.length > 0 ? (
            <MCPSnapshotCard snapshot={execution.mcp_snapshot} />
          ) : null}

          {execution.claude_version || execution.claude_session_id ? (
            <Card>
              <CardHeader title="Environment" />
              <dl className="space-y-3 px-4 py-3">
                <Field label="Claude CLI">{execution.claude_version || '-'}</Field>
                <Field label="Session">
                  <span className="font-mono text-xs">
                    {execution.claude_session_id || '-'}
                  </span>
                </Field>
                {execution.exit_code != null ? (
                  <Field label="Exit code">{execution.exit_code}</Field>
                ) : null}
              </dl>
            </Card>
          ) : null}
        </div>
      </div>
    </>
  )
}

function ChecksCard({ checks }: { checks: CheckResult[] }) {
  return (
    <Card>
      <CardHeader
        title="Pre-flight checks"
        subtitle="Verified before this run started"
      />
      <ul className="divide-y divide-border-subtle">
        {checks.map((check) => {
          const presentation = checkPresentation(check.state)
          return (
            <li key={`${check.kind}-${check.target}`} className="px-4 py-2.5">
              <div className="flex items-center gap-2 text-xs">
                <span className={presentation.text}>{presentation.glyph}</span>
                <span className="truncate font-medium">{check.target}</span>
                <span className="ml-auto shrink-0 text-fg-faint">
                  {requirementKindLabel(check.kind)}
                </span>
              </div>
              {check.detail ? (
                <p className="mt-1 line-clamp-2 text-xs text-fg-muted">{check.detail}</p>
              ) : null}
            </li>
          )
        })}
      </ul>
    </Card>
  )
}

function MCPSnapshotCard({ snapshot }: { snapshot: Array<{ name: string; status: string }> }) {
  const [expanded, setExpanded] = useState(false)

  const counts = snapshot.reduce<Record<string, number>>((acc, server) => {
    acc[server.status] = (acc[server.status] ?? 0) + 1
    return acc
  }, {})

  const shown = expanded ? snapshot : snapshot.filter((s) => s.status !== 'needs-auth')

  return (
    <Card>
      <CardHeader
        title="MCP servers at launch"
        subtitle={`${snapshot.length} configured`}
      />
      <div className="flex flex-wrap gap-1.5 px-4 py-3">
        {Object.entries(counts).map(([status, count]) => (
          <Badge
            key={status}
            tone={status === 'connected' ? 'success' : status === 'failed' ? 'danger' : 'neutral'}
          >
            {count} {status}
          </Badge>
        ))}
      </div>
      {shown.length > 0 ? (
        <ul className="max-h-52 overflow-y-auto border-t border-border-subtle px-4 py-2 text-xs">
          {shown.map((server) => (
            <li key={server.name} className="flex items-center gap-2 py-0.5">
              <span
                className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                  server.status === 'connected' ? 'bg-st-success' : 'bg-st-empty'
                }`}
              />
              <span className="truncate">{server.name}</span>
            </li>
          ))}
        </ul>
      ) : null}
      {snapshot.some((s) => s.status === 'needs-auth') ? (
        <button
          onClick={() => setExpanded(!expanded)}
          className="w-full border-t border-border-subtle px-4 py-2 text-xs text-accent hover:bg-surface-2"
        >
          {expanded ? 'Hide unauthenticated' : `Show all ${snapshot.length}`}
        </button>
      ) : null}
    </Card>
  )
}
