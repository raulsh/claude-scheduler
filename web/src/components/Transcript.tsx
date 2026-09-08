import { useEffect, useRef, useState } from 'react'
import { ChevronDown, ChevronRight, Terminal, Wrench } from 'lucide-react'

import type { Entry } from '../lib/useTranscript'
import { Markdown } from './MarkdownAsync'
import { Badge } from './ui'

/** Renders the transcript as a structured conversation.
 *
 * Autoscroll follows the tail but detaches the moment the reader scrolls up,
 * so reading back through a long run is not fought by incoming events. */
export function Transcript({
  entries,
  live,
}: {
  entries: Entry[]
  live: boolean
}) {
  const bottomRef = useRef<HTMLDivElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)
  const [pinned, setPinned] = useState(true)

  useEffect(() => {
    if (!pinned) return
    bottomRef.current?.scrollIntoView({ block: 'end' })
  }, [entries.length, pinned])

  const onScroll = () => {
    const el = containerRef.current
    if (!el) return
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight
    setPinned(distance < 80)
  }

  if (entries.length === 0) {
    return (
      <div className="px-4 py-10 text-center text-xs text-fg-muted">
        {live ? 'Waiting for output…' : 'This run produced no transcript.'}
      </div>
    )
  }

  return (
    <div className="relative">
      <div
        ref={containerRef}
        onScroll={onScroll}
        className="max-h-[65vh] overflow-y-auto px-4 py-3"
      >
        <div className="space-y-2.5">
          {entries.map((entry, index) => (
            <EntryView key={`${entry.seq}-${index}`} entry={entry} />
          ))}
        </div>
        <div ref={bottomRef} />
      </div>

      {!pinned && live ? (
        <button
          onClick={() => {
            setPinned(true)
            bottomRef.current?.scrollIntoView({ behavior: 'smooth', block: 'end' })
          }}
          className="absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full border border-border-strong bg-surface px-3 py-1 text-xs font-medium shadow-lg transition hover:border-fg-faint"
        >
          Jump to latest ↓
        </button>
      ) : null}
    </div>
  )
}

function EntryView({ entry }: { entry: Entry }) {
  switch (entry.kind) {
    case 'system':
      return (
        <div className="flex flex-wrap items-center gap-1.5 text-xs text-fg-muted">
          <Badge tone="accent">session</Badge>
          <span>{entry.model || 'unknown model'}</span>
          {entry.version ? <span>· CLI {entry.version}</span> : null}
          <span>· {entry.tools} tools</span>
          <span>· {entry.mcp} MCP servers</span>
        </div>
      )

    case 'rate_limit':
      return (
        <div className="flex items-center gap-1.5 text-xs">
          <Badge tone="warn">usage</Badge>
          <span className="text-fg-muted">{entry.detail}</span>
        </div>
      )

    case 'text':
      return <Markdown>{entry.text}</Markdown>

    case 'thinking':
      return <Collapsible label="Thinking" body={entry.text} muted />

    case 'tool_use':
      return <ToolCall entry={entry} />

    case 'result':
      return (
        <div
          className={`rounded-md border p-3 text-sm ${
            entry.isError
              ? 'border-st-failure/30 bg-st-failure/5'
              : 'border-st-success/30 bg-st-success/5'
          }`}
        >
          <div className="mb-1 text-[11px] font-medium tracking-wide uppercase">
            {entry.isError ? (
              <span className="text-st-failure">Final result: error</span>
            ) : (
              <span className="text-st-success">Final result</span>
            )}
          </div>
          {entry.text ? (
            <Markdown>{entry.text}</Markdown>
          ) : (
            <span className="text-fg-muted">(empty)</span>
          )}
        </div>
      )
  }
}

function ToolCall({ entry }: { entry: Extract<Entry, { kind: 'tool_use' }> }) {
  const [open, setOpen] = useState(false)
  const command = extractCommand(entry.input)

  return (
    <div className="rounded-md border border-border-subtle bg-surface-2/60">
      <button
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs"
      >
        {open ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-fg-faint" />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-fg-faint" />
        )}
        {entry.name === 'Bash' ? (
          <Terminal className="h-3.5 w-3.5 shrink-0 text-accent" />
        ) : (
          <Wrench className="h-3.5 w-3.5 shrink-0 text-accent" />
        )}
        <span className="font-medium">{entry.name}</span>
        {command ? (
          <code className="truncate font-mono text-fg-muted">{command}</code>
        ) : null}
        {entry.result?.isError ? (
          <span className="ml-auto shrink-0 text-st-failure">failed</span>
        ) : entry.result ? (
          <span className="ml-auto shrink-0 text-st-success">ok</span>
        ) : (
          <span className="ml-auto shrink-0 text-fg-faint">running…</span>
        )}
      </button>

      {open ? (
        <div className="space-y-2 border-t border-border-subtle px-3 py-2">
          {entry.input !== undefined ? (
            <div>
              <div className="mb-1 text-[11px] font-medium text-fg-faint uppercase">
                Input
              </div>
              <pre className="max-h-48 overflow-auto rounded bg-bg p-2 font-mono text-[11px] whitespace-pre-wrap">
                {JSON.stringify(entry.input, null, 2)}
              </pre>
            </div>
          ) : null}
          {entry.result ? (
            <div>
              <div className="mb-1 text-[11px] font-medium text-fg-faint uppercase">
                Result
              </div>
              <pre
                className={`max-h-64 overflow-auto rounded p-2 font-mono text-[11px] whitespace-pre-wrap ${
                  entry.result.isError ? 'bg-st-failure/10' : 'bg-bg'
                }`}
              >
                {entry.result.content || '(no output)'}
              </pre>
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

function Collapsible({
  label,
  body,
  muted = false,
}: {
  label: string
  body: string
  muted?: boolean
}) {
  const [open, setOpen] = useState(false)
  return (
    <div className="rounded-md border border-border-subtle">
      <button
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-fg-muted"
      >
        {open ? <ChevronDown className="h-3 w-3" /> : <ChevronRight className="h-3 w-3" />}
        {label}
      </button>
      {open ? (
        <div
          className={`border-t border-border-subtle px-3 py-2 ${muted ? 'text-fg-muted' : ''}`}
        >
          <Markdown>{body}</Markdown>
        </div>
      ) : null}
    </div>
  )
}

/** Pulls the most identifying field out of a tool input for the collapsed row. */
function extractCommand(input: unknown): string {
  if (!input || typeof input !== 'object') return ''
  const record = input as Record<string, unknown>
  for (const key of ['command', 'file_path', 'path', 'pattern', 'url', 'query']) {
    const value = record[key]
    if (typeof value === 'string' && value) return value
  }
  return ''
}
