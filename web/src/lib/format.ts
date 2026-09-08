/** Formatting helpers shared across views. */

export function formatDuration(ms: number): string {
  if (!ms || ms < 0) return '-'
  if (ms < 1000) return `${ms}ms`
  const seconds = ms / 1000
  if (seconds < 60) return `${seconds.toFixed(1)}s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.round(seconds % 60)
  if (minutes < 60) return `${minutes}m ${rest}s`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

export function formatCost(usd: number): string {
  if (!usd) return '$0'
  if (usd < 0.01) return `$${usd.toFixed(4)}`
  return `$${usd.toFixed(2)}`
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
})

const dateTimeWithSeconds = new Intl.DateTimeFormat(undefined, {
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

export function formatAbsolute(iso?: string, withSeconds = false): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '-'
  return (withSeconds ? dateTimeWithSeconds : dateTime).format(d)
}

/** A compact relative time, e.g. "3m ago" or "in 12m". */
export function formatRelative(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '-'

  const deltaMs = d.getTime() - Date.now()
  const future = deltaMs > 0
  const abs = Math.abs(deltaMs)

  const units: Array<[number, string]> = [
    [1000, 's'],
    [60_000, 'm'],
    [3_600_000, 'h'],
    [86_400_000, 'd'],
  ]

  if (abs < 5000) return future ? 'in a moment' : 'just now'

  let value = Math.round(abs / 1000)
  let suffix = 's'
  for (const [ms, label] of units) {
    if (abs >= ms) {
      value = Math.floor(abs / ms)
      suffix = label
    }
  }
  return future ? `in ${value}${suffix}` : `${value}${suffix} ago`
}

/** Renders a cron expression in plain language for the common shapes.
 *
 * This covers the expressions people actually type; anything unusual falls
 * back to the raw expression rather than guessing wrong. The authoritative
 * next-run times always come from the server. */
export function describeCron(expr: string): string {
  const trimmed = expr.trim()

  const descriptors: Record<string, string> = {
    '@yearly': 'Once a year, on 1 January',
    '@annually': 'Once a year, on 1 January',
    '@monthly': 'Monthly, on the 1st',
    '@weekly': 'Weekly, on Sunday',
    '@daily': 'Daily at midnight',
    '@midnight': 'Daily at midnight',
    '@hourly': 'Every hour, on the hour',
  }
  if (descriptors[trimmed]) return descriptors[trimmed]

  const every = /^@every\s+(.+)$/.exec(trimmed)
  if (every?.[1]) return `Every ${every[1]}`

  const parts = trimmed.split(/\s+/)
  if (parts.length !== 5) return trimmed

  const [min, hour, dom, month, dow] = parts as [string, string, string, string, string]
  const days = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

  const stepMin = /^\*\/(\d+)$/.exec(min)
  if (stepMin?.[1] && hour === '*' && dom === '*' && month === '*' && dow === '*') {
    return `Every ${stepMin[1]} minutes`
  }
  if (min === '*' && hour === '*' && dom === '*' && month === '*' && dow === '*') {
    return 'Every minute'
  }

  const stepHour = /^\*\/(\d+)$/.exec(hour)
  if (/^\d+$/.test(min) && stepHour?.[1] && dom === '*' && month === '*' && dow === '*') {
    return `Every ${stepHour[1]} hours at :${min.padStart(2, '0')}`
  }

  if (/^\d+$/.test(min) && /^\d+$/.test(hour) && month === '*') {
    const time = `${hour.padStart(2, '0')}:${min.padStart(2, '0')}`
    if (dom === '*' && dow === '*') return `Daily at ${time}`
    if (dom === '*' && /^\d$/.test(dow)) return `Weekly on ${days[Number(dow)]} at ${time}`
    if (dow === '*' && /^\d+$/.test(dom)) return `Monthly on day ${dom} at ${time}`
    if (dow === '1-5') return `Weekdays at ${time}`
  }

  if (/^\d+$/.test(min) && hour === '*' && dom === '*' && month === '*' && dow === '*') {
    return `Hourly at :${min.padStart(2, '0')}`
  }

  return trimmed
}

export function pluralize(n: number, singular: string, plural = `${singular}s`): string {
  return `${n} ${n === 1 ? singular : plural}`
}

/** Flattens markdown to a single readable line, for table cells and tooltips.
 *
 * Claude's output is markdown, so a raw result string in a list column shows
 * literal `- **Account**: ...` syntax. This strips the markup rather than
 * rendering it, because a list row has no space for block formatting. */
export function flattenMarkdown(text: string, max = 160): string {
  if (!text) return ''

  let out = text
    // Fenced blocks become a placeholder: their content is never useful in
    // one line, and dumping it buries the summary.
    .replace(/```[\s\S]*?```/g, ' [code] ')
    .replace(/`([^`]+)`/g, '$1')
    // Images before links, since the syntax nests.
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/^\s{0,3}#{1,6}\s+/gm, '')
    .replace(/^\s{0,3}>\s?/gm, '')
    // A table's alignment row carries no information once flattened.
    .replace(/^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/gm, '')
    // Remaining cell separators become a visible divider, so "a | b" does
    // not silently run together as "a b".
    .replace(/[ \t]*\|[ \t]*/g, ' · ')
    .replace(/^\s*[-*+]\s+/gm, '')
    .replace(/^\s*\d+\.\s+/gm, '')
    // Task-list markers, after the bullet they follow has been removed.
    .replace(/^\s*\[([ xX])\]\s+/gm, (_m, mark: string) =>
      mark === ' ' ? '' : '✓ ',
    )
    .replace(/^\s*[-*_]{3,}\s*$/gm, ' ')
    .replace(/(\*\*|__)(.*?)\1/g, '$2')
    .replace(/(\*|_)(.*?)\1/g, '$2')
    .replace(/~~(.*?)~~/g, '$1')
    .replace(/\s+/g, ' ')
    // Tidy up separators left dangling by removed rows.
    .replace(/(?:\s*·\s*)+/g, ' · ')
    .replace(/^\s*·\s*|\s*·\s*$/g, '')
    .trim()

  if (out.length > max) out = out.slice(0, max).trimEnd() + '…'
  return out
}
