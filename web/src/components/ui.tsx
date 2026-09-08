import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { AlertTriangle, Loader2 } from 'lucide-react'

/** A surface panel. The building block for every list and detail view. */
export function Card({
  children,
  className = '',
}: {
  children: ReactNode
  className?: string
}) {
  return (
    <div
      className={`rounded-lg border border-border-subtle bg-surface ${className}`}
    >
      {children}
    </div>
  )
}

export function CardHeader({
  title,
  subtitle,
  actions,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="flex items-start justify-between gap-4 border-b border-border-subtle px-4 py-3">
      <div className="min-w-0">
        <h2 className="truncate text-sm font-semibold">{title}</h2>
        {subtitle ? (
          <p className="mt-0.5 truncate text-xs text-fg-muted">{subtitle}</p>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  )
}

type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'

const BUTTON_STYLES: Record<ButtonVariant, string> = {
  primary: 'bg-accent text-white hover:opacity-90',
  secondary:
    'border border-border-strong bg-surface-2 text-fg hover:border-fg-faint',
  ghost: 'text-fg-muted hover:bg-surface-2 hover:text-fg',
  danger: 'border border-st-failure/40 text-st-failure hover:bg-st-failure/10',
}

export function Button({
  children,
  variant = 'secondary',
  size = 'md',
  loading = false,
  ...rest
}: {
  children: ReactNode
  variant?: ButtonVariant
  size?: 'sm' | 'md'
  loading?: boolean
} & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  const sizing = size === 'sm' ? 'px-2 py-1 text-xs' : 'px-3 py-1.5 text-sm'
  return (
    <button
      {...rest}
      disabled={rest.disabled || loading}
      className={`inline-flex items-center gap-1.5 rounded-md font-medium transition disabled:cursor-not-allowed disabled:opacity-50 ${sizing} ${BUTTON_STYLES[variant]} ${rest.className ?? ''}`}
    >
      {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
      {children}
    </button>
  )
}

/** A small pill. Used for statuses, policies and counts. */
export function Badge({
  children,
  tone = 'neutral',
  title,
}: {
  children: ReactNode
  tone?: 'neutral' | 'accent' | 'warn' | 'danger' | 'success'
  title?: string
}) {
  const tones = {
    neutral: 'border-border-strong text-fg-muted',
    accent: 'border-accent/40 bg-accent-soft text-accent',
    warn: 'border-st-blocked/40 text-st-blocked',
    danger: 'border-st-failure/40 text-st-failure',
    success: 'border-st-success/40 text-st-success',
  }
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] font-medium ${tones[tone]}`}
    >
      {children}
    </span>
  )
}

/** Skeleton rows, used instead of a spinner so layout does not jump. */
export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div className="space-y-2 p-4">
      {Array.from({ length: rows }).map((_, i) => (
        <div
          key={i}
          className="h-10 animate-pulse rounded bg-surface-2"
          style={{ animationDelay: `${i * 80}ms` }}
        />
      ))}
    </div>
  )
}

export function EmptyState({
  title,
  body,
  action,
}: {
  title: string
  body?: string
  action?: ReactNode
}) {
  return (
    <div className="px-4 py-12 text-center">
      <p className="text-sm font-medium">{title}</p>
      {body ? <p className="mx-auto mt-1 max-w-md text-xs text-fg-muted">{body}</p> : null}
      {action ? <div className="mt-4 flex justify-center">{action}</div> : null}
    </div>
  )
}

export function ErrorState({
  title = 'Something went wrong',
  error,
  onRetry,
}: {
  title?: string
  error: unknown
  onRetry?: () => void
}) {
  const message =
    error instanceof Error ? error.message : typeof error === 'string' ? error : 'Unknown error'
  const problems =
    error && typeof error === 'object' && 'problems' in error
      ? ((error as { problems?: string[] }).problems ?? [])
      : []

  return (
    <div className="p-4">
      <div className="flex gap-3 rounded-md border border-st-failure/30 bg-st-failure/5 p-3">
        <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-st-failure" />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-st-failure">{title}</p>
          <p className="mt-0.5 text-xs break-words text-fg-muted">{message}</p>
          {problems.length > 0 ? (
            <ul className="mt-2 list-inside list-disc space-y-0.5 text-xs text-fg-muted">
              {problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          ) : null}
          {onRetry ? (
            <button
              onClick={onRetry}
              className="mt-2 text-xs font-medium text-accent hover:underline"
            >
              Try again
            </button>
          ) : null}
        </div>
      </div>
    </div>
  )
}

export function PageHeader({
  title,
  subtitle,
  actions,
  back,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
  back?: { to: string; label: string }
}) {
  return (
    <div className="mb-5">
      {back ? (
        <Link
          to={back.to}
          className="mb-1.5 inline-block text-xs text-fg-muted hover:text-fg"
        >
          ← {back.label}
        </Link>
      ) : null}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold tracking-tight">{title}</h1>
          {subtitle ? <p className="mt-1 text-sm text-fg-muted">{subtitle}</p> : null}
        </div>
        {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
      </div>
    </div>
  )
}

/** A labelled value, for the metadata grids on detail pages. */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-[11px] font-medium tracking-wide text-fg-faint uppercase">
        {label}
      </dt>
      <dd className="mt-0.5 truncate text-sm tnum">{children}</dd>
    </div>
  )
}
