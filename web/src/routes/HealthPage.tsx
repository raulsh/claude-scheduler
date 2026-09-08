import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, ExternalLink, RefreshCw } from 'lucide-react'

import { api } from '../api/client'
import type { CheckResult, LoginSession } from '../api/types'
import { Badge, Button, Card, CardHeader, ErrorState, PageHeader, Skeleton } from '../components/ui'
import { checkPresentation, requirementKindLabel } from '../lib/status'
import { formatRelative } from '../lib/format'

/** The dependency health page.
 *
 * This is where an expiring credential gets fixed. Its whole job is to make
 * "which of my dependencies will stop a scheduled run" answerable at a
 * glance, and then to fix it in as few clicks as possible. */
export function HealthPage() {
  const queryClient = useQueryClient()
  const [refreshing, setRefreshing] = useState(false)

  // The kind list is cheap; each kind's results are then fetched on their
  // own so AWS profiles and binaries paint in about a second instead of
  // waiting behind the minute-long MCP probe.
  const kindsQuery = useQuery({
    queryKey: ['health', 'kinds'],
    queryFn: api.healthKinds,
  })

  const refresh = async () => {
    setRefreshing(true)
    try {
      await Promise.all(
        (kindsQuery.data?.kinds ?? []).map((kind) =>
          queryClient.fetchQuery({
            queryKey: ['health', 'kind', kind],
            queryFn: () => api.checkKind(kind, true),
          }),
        ),
      )
      queryClient.invalidateQueries({ queryKey: ['health', 'snapshot'] })
    } finally {
      setRefreshing(false)
    }
  }

  const sortedKinds = kindsQuery.data?.kinds ?? []

  return (
    <>
      <PageHeader
        title="Dependencies"
        subtitle="Checked before every run. A dependency needing attention stops the tasks that require it; one that is merely unreachable does not."
        actions={
          <Button onClick={refresh} loading={refreshing}>
            <RefreshCw className="h-3.5 w-3.5" />
            Re-check all
          </Button>
        }
      />

      {kindsQuery.isError ? (
        <ErrorState
          title="Could not load dependency kinds"
          error={kindsQuery.error}
          onRetry={() => kindsQuery.refetch()}
        />
      ) : kindsQuery.isPending ? (
        <Card>
          <Skeleton rows={3} />
        </Card>
      ) : (
        <div className="space-y-4">
          {sortedKinds.map((kind) => (
            <KindSection key={kind} kind={kind} />
          ))}
        </div>
      )}
    </>
  )
}

function KindSection({ kind }: { kind: string }) {
  const [showHealthy, setShowHealthy] = useState(kind !== 'mcp_server')

  const query = useQuery({
    queryKey: ['health', 'kind', kind],
    queryFn: () => api.checkKind(kind, false),
  })

  const results = query.data?.results ?? []
  const requiredBy = query.data?.required_by ?? {}

  // Ordered by what needs action: dependencies a task requires and that are
  // unhealthy come first, then other unhealthy ones, then the rest.
  const rank = (r: CheckResult) => {
    const gates = checkPresentation(r.state).gates
    const used = (requiredBy[r.target] ?? 0) > 0
    if (gates && used) return 0
    if (gates) return 1
    if (used) return 2
    return 3
  }
  const gatingFirst = [...results].sort(
    (a, b) => rank(a) - rank(b) || a.target.localeCompare(b.target),
  )

  const healthy = gatingFirst.filter((r) => r.state === 'ok')
  const visible = showHealthy ? gatingFirst : gatingFirst.filter((r) => r.state !== 'ok')

  const counts = results.reduce<Record<string, number>>((acc, r) => {
    acc[r.state] = (acc[r.state] ?? 0) + 1
    return acc
  }, {})

  if (query.isPending) {
    return (
      <Card>
        <CardHeader
          title={requirementKindLabel(kind)}
          subtitle={
            kind === 'mcp_server'
              ? 'Health-checking every server, which takes up to a minute'
              : 'Checking…'
          }
        />
        <Skeleton rows={kind === 'mcp_server' ? 3 : 2} />
      </Card>
    )
  }

  if (query.isError) {
    return (
      <Card>
        <CardHeader title={requirementKindLabel(kind)} />
        <ErrorState error={query.error} onRetry={() => query.refetch()} />
      </Card>
    )
  }

  return (
    <Card>
      <CardHeader
        title={requirementKindLabel(kind)}
        subtitle={
          query.data?.error
            ? query.data.error
            : (query.data?.blocking_declared ?? 0) > 0
              ? `${results.length} known · ${query.data?.blocking_declared} blocking a schedule`
              : `${results.length} known`
        }
        actions={
          <div className="flex items-center gap-1.5">
            {Object.entries(counts).map(([state, count]) => {
              const presentation = checkPresentation(state as CheckResult['state'])
              return (
                <Badge
                  key={state}
                  tone={
                    state === 'ok'
                      ? 'success'
                      : presentation.gates
                        ? 'warn'
                        : 'neutral'
                  }
                  title={presentation.meaning}
                >
                  {count} {presentation.label.toLowerCase()}
                </Badge>
              )
            })}
          </div>
        }
      />

      {visible.length === 0 ? (
        <p className="px-4 py-6 text-center text-xs text-fg-muted">
          Everything here is healthy.
        </p>
      ) : (
        <ul className="divide-y divide-border-subtle">
          {visible.map((result) => (
            <CheckRow
              key={`${result.kind}-${result.target}`}
              result={result}
              usedBy={requiredBy[result.target] ?? 0}
            />
          ))}
        </ul>
      )}

      {healthy.length > 0 && results.length !== healthy.length ? (
        <button
          onClick={() => setShowHealthy(!showHealthy)}
          className="w-full border-t border-border-subtle px-4 py-2 text-xs text-accent hover:bg-surface-2"
        >
          {showHealthy
            ? `Hide ${healthy.length} healthy`
            : `Show ${healthy.length} healthy`}
        </button>
      ) : null}
    </Card>
  )
}

function CheckRow({ result, usedBy = 0 }: { result: CheckResult; usedBy?: number }) {
  const presentation = checkPresentation(result.state)
  const [copied, setCopied] = useState(false)
  const queryClient = useQueryClient()

  const recheck = useMutation({
    mutationFn: () => api.checkOne(result.kind, result.target, true),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['health'] }),
  })

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard access can be denied; the command is visible either way.
    }
  }

  return (
    <li className="px-4 py-3">
      <div className="flex flex-wrap items-start gap-x-3 gap-y-1.5">
        <span className={`mt-0.5 shrink-0 text-sm ${presentation.text}`} aria-hidden>
          {presentation.glyph}
        </span>

        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="truncate text-sm font-medium">{result.target}</span>
            <span className={`text-xs ${presentation.text}`}>{presentation.label}</span>
            {usedBy > 0 ? (
              <Badge tone="accent" title="Schedules that declare this dependency">
                {usedBy === 1 ? 'used by 1 schedule' : `used by ${usedBy} schedules`}
              </Badge>
            ) : null}
            {presentation.gates && usedBy > 0 ? (
              <Badge tone="warn" title="The schedules requiring this will not run">
                blocking
              </Badge>
            ) : presentation.gates ? (
              <Badge title="Unhealthy, but no schedule requires it">
                unused
              </Badge>
            ) : null}
          </div>

          {result.detail ? (
            <p className="mt-1 text-xs break-words text-fg-muted">{result.detail}</p>
          ) : null}

          {result.checked_at ? (
            <p className="mt-1 text-[11px] text-fg-faint">
              checked {formatRelative(result.checked_at)}
              {result.latency_ms ? ` · ${(result.latency_ms / 1000).toFixed(1)}s` : ''}
            </p>
          ) : null}

          {result.remediation ? (
            <div className="mt-2 rounded-md border border-border-subtle bg-surface-2/60 p-2.5">
              {result.remediation.hint ? (
                <p className="mb-2 text-xs text-fg-muted">{result.remediation.hint}</p>
              ) : null}

              {result.remediation.automatic && result.kind === 'aws_profile' ? (
                <AwsLoginFlow profile={result.target} />
              ) : result.remediation.command ? (
                <div className="flex items-center gap-2">
                  <code className="min-w-0 flex-1 truncate rounded bg-bg px-2 py-1 font-mono text-xs">
                    {result.remediation.command}
                  </code>
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => copy(result.remediation!.command!)}
                  >
                    {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
                    {copied ? 'Copied' : 'Copy'}
                  </Button>
                </div>
              ) : null}
            </div>
          ) : null}
        </div>

        <Button
          size="sm"
          variant="ghost"
          onClick={() => recheck.mutate()}
          loading={recheck.isPending}
          title="Re-check now, bypassing the cache"
        >
          <RefreshCw className="h-3 w-3" />
        </Button>
      </div>
    </li>
  )
}

/** Drives an AWS SSO login from the browser.
 *
 * The service runs `aws sso login --no-browser` and scrapes the verification
 * URL and code from its output. That works because the flow is one-way: the
 * user authorises in a browser and the CLI polls, with nothing to paste
 * back. */
function AwsLoginFlow({ profile }: { profile: string }) {
  const queryClient = useQueryClient()
  const [started, setStarted] = useState(false)

  const start = useMutation({
    mutationFn: () => api.startAwsLogin(profile),
    onSuccess: () => setStarted(true),
  })

  const session = useQuery({
    queryKey: ['aws-login', profile],
    queryFn: () => api.getAwsLogin(profile),
    enabled: started,
    refetchInterval: (query) => {
      const state = query.state.data?.state
      return state === 'completed' || state === 'failed' || state === 'cancelled' ? false : 1500
    },
  })

  const cancel = useMutation({
    mutationFn: () => api.cancelAwsLogin(profile),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['aws-login', profile] }),
  })

  const data = session.data

  if (data?.state === 'completed') {
    return (
      <div className="flex items-center gap-2 text-xs text-st-success">
        <Check className="h-3.5 w-3.5" />
        Signed in. Re-check to confirm.
        <Button
          size="sm"
          variant="ghost"
          onClick={() => queryClient.invalidateQueries({ queryKey: ['health'] })}
        >
          Re-check
        </Button>
      </div>
    )
  }

  if (!started) {
    return (
      <div className="flex items-center gap-2">
        <Button size="sm" variant="primary" onClick={() => start.mutate()} loading={start.isPending}>
          Sign in to AWS
        </Button>
        {start.isError ? (
          <span className="text-xs text-st-failure">
            {start.error instanceof Error ? start.error.message : 'Could not start'}
          </span>
        ) : null}
      </div>
    )
  }

  return (
    <div className="space-y-2">
      {data?.state === 'failed' || data?.state === 'cancelled' ? (
        <p className="text-xs text-st-failure">
          {data.error || `Login ${data.state}.`}{' '}
          <button
            className="font-medium text-accent hover:underline"
            onClick={() => {
              setStarted(false)
              start.reset()
            }}
          >
            Try again
          </button>
        </p>
      ) : data?.verification_uri || data?.user_code ? (
        <>
          <p className="text-xs text-fg-muted">
            Open the link, then confirm this code:
          </p>
          <div className="flex flex-wrap items-center gap-2">
            {data.user_code ? (
              <code className="rounded border border-accent/40 bg-accent-soft px-2.5 py-1 font-mono text-sm font-semibold tracking-widest text-accent">
                {data.user_code}
              </code>
            ) : null}
            {data.verification_uri ? (
              <a
                href={data.verification_uri}
                target="_blank"
                rel="noreferrer noopener"
                className="inline-flex items-center gap-1.5 rounded-md bg-accent px-2.5 py-1 text-xs font-medium text-white hover:opacity-90"
              >
                <ExternalLink className="h-3 w-3" />
                Authorize
              </a>
            ) : null}
            <span className="inline-flex items-center gap-1.5 text-xs text-fg-muted">
              <RefreshCw className="h-3 w-3 animate-spin" />
              waiting…
            </span>
            <Button size="sm" variant="ghost" onClick={() => cancel.mutate()}>
              Cancel
            </Button>
          </div>
          {data.login_profile !== data.profile ? (
            <p className="text-[11px] text-fg-faint">
              Signing in to <code className="font-mono">{data.login_profile}</code>, which{' '}
              <code className="font-mono">{data.profile}</code> chains from.
            </p>
          ) : null}
        </>
      ) : (
        <p className="inline-flex items-center gap-1.5 text-xs text-fg-muted">
          <RefreshCw className="h-3 w-3 animate-spin" />
          Starting the login…
        </p>
      )}
      {data ? <LoginOutput session={data} /> : null}
    </div>
  )
}

function LoginOutput({ session }: { session: LoginSession }) {
  const [open, setOpen] = useState(false)
  if (!session.output) return null
  return (
    <div>
      <button
        onClick={() => setOpen(!open)}
        className="text-[11px] text-fg-faint hover:text-fg-muted"
      >
        {open ? 'Hide' : 'Show'} command output
      </button>
      {open ? (
        <pre className="mt-1 max-h-40 overflow-auto rounded bg-bg p-2 font-mono text-[11px] whitespace-pre-wrap">
          {session.output}
        </pre>
      ) : null}
    </div>
  )
}
