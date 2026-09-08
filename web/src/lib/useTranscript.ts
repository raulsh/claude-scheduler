import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ExecutionStatus, TranscriptEvent } from '../api/types'
import { streamUrl } from '../api/client'

/** A rendered transcript entry, derived from the raw NDJSON events.
 *
 * The stream is presented as a structured conversation rather than a raw
 * dump: assistant prose, tool calls with their inputs, and tool results
 * attached to the call they answer. */
export type Entry =
  | { kind: 'text'; seq: number; text: string }
  | { kind: 'thinking'; seq: number; text: string }
  | { kind: 'tool_use'; seq: number; id: string; name: string; input: unknown; result?: ToolResult }
  | { kind: 'system'; seq: number; model: string; version: string; tools: number; mcp: number }
  | { kind: 'rate_limit'; seq: number; detail: string }
  | { kind: 'result'; seq: number; text: string; isError: boolean }

interface ToolResult {
  content: string
  isError: boolean
}

interface ContentBlock {
  type: string
  text?: string
  thinking?: string
  id?: string
  name?: string
  input?: unknown
  tool_use_id?: string
  content?: unknown
  is_error?: boolean
}

export interface TranscriptState {
  entries: Entry[]
  /** Live connection state, for the header indicator. */
  connection: 'idle' | 'connecting' | 'live' | 'closed' | 'error'
  /** Terminal status pushed by the server, if it arrived on this stream. */
  terminalStatus?: ExecutionStatus
  lastSeq: number
}

/** Consumes an execution's SSE stream, resuming from the last sequence seen.
 *
 * Historical backfill is handled by the server: the endpoint replays stored
 * events before attaching to the live feed, so a reconnect never drops
 * anything. EventSource reconnects on its own and sends Last-Event-ID. */
export function useTranscript(executionId: number, live: boolean): TranscriptState {
  const [events, setEvents] = useState<TranscriptEvent[]>([])
  const [connection, setConnection] = useState<TranscriptState['connection']>('idle')
  const [terminalStatus, setTerminalStatus] = useState<ExecutionStatus | undefined>()
  const lastSeqRef = useRef(0)

  const append = useCallback((event: TranscriptEvent) => {
    if (event.seq > 0) lastSeqRef.current = Math.max(lastSeqRef.current, event.seq)
    setEvents((prev) => {
      // The server may replay an event a reconnect already delivered.
      if (event.seq > 0 && prev.some((e) => e.seq === event.seq)) return prev
      return [...prev, event]
    })
  }, [])

  useEffect(() => {
    if (!live) return

    setConnection('connecting')
    const source = new EventSource(streamUrl(executionId, lastSeqRef.current))

    const onMessage = (raw: MessageEvent<string>) => {
      setConnection('live')
      try {
        const event = JSON.parse(raw.data) as TranscriptEvent
        if (event.type === 'status') {
          if (event.status) setTerminalStatus(event.status)
          if (event.terminal) {
            setConnection('closed')
            source.close()
          }
          return
        }
        append(event)
      } catch {
        // A malformed frame is not worth tearing the stream down for.
      }
    }

    // The server names each frame after the event type, so every type needs
    // its own listener; onmessage only receives unnamed frames.
    const types = ['message', 'system', 'assistant', 'user', 'result', 'rate_limit_event', 'status']
    types.forEach((type) => source.addEventListener(type, onMessage as EventListener))

    source.onerror = () => {
      // EventSource retries by itself; only a closed socket is terminal.
      setConnection((current) => (current === 'closed' ? current : 'error'))
    }

    return () => {
      types.forEach((type) => source.removeEventListener(type, onMessage as EventListener))
      source.close()
    }
  }, [executionId, live, append])

  const entries = useMemo(() => buildEntries(events), [events])

  return { entries, connection, terminalStatus, lastSeq: lastSeqRef.current }
}

/** Seeds the transcript from stored events, for a finished execution. */
export function buildEntries(events: TranscriptEvent[]): Entry[] {
  const entries: Entry[] = []
  // Tool results arrive in a later event than the call, so calls are indexed
  // by id and filled in when their result shows up.
  const toolCalls = new Map<string, Extract<Entry, { kind: 'tool_use' }>>()

  const sorted = [...events].sort((a, b) => a.seq - b.seq)

  for (const event of sorted) {
    const payload = (event.payload ?? {}) as Record<string, unknown>

    switch (event.type) {
      case 'system': {
        if (event.subtype !== 'init') break
        const mcp = Array.isArray(payload.mcp_servers) ? payload.mcp_servers.length : 0
        const tools = Array.isArray(payload.tools) ? payload.tools.length : 0
        entries.push({
          kind: 'system',
          seq: event.seq,
          model: String(payload.model ?? ''),
          version: String(payload.claude_code_version ?? ''),
          tools,
          mcp,
        })
        break
      }

      case 'rate_limit_event': {
        const info = (payload.rate_limit_info ?? {}) as Record<string, unknown>
        const parts: string[] = []
        if (info.rateLimitType) parts.push(String(info.rateLimitType))
        if (info.status) parts.push(`status ${info.status}`)
        if (info.overageDisabledReason) parts.push(String(info.overageDisabledReason))
        entries.push({
          kind: 'rate_limit',
          seq: event.seq,
          detail: parts.join(' · ') || 'usage limit notice',
        })
        break
      }

      case 'assistant': {
        const message = (payload.message ?? {}) as { content?: ContentBlock[] }
        for (const block of message.content ?? []) {
          if (block.type === 'text' && block.text) {
            entries.push({ kind: 'text', seq: event.seq, text: block.text })
          } else if (block.type === 'thinking' && block.thinking) {
            entries.push({ kind: 'thinking', seq: event.seq, text: block.thinking })
          } else if (block.type === 'tool_use') {
            const entry: Extract<Entry, { kind: 'tool_use' }> = {
              kind: 'tool_use',
              seq: event.seq,
              id: block.id ?? '',
              name: block.name ?? 'tool',
              input: block.input,
            }
            entries.push(entry)
            if (entry.id) toolCalls.set(entry.id, entry)
          }
        }
        break
      }

      case 'user': {
        const message = (payload.message ?? {}) as { content?: ContentBlock[] }
        for (const block of message.content ?? []) {
          if (block.type !== 'tool_result') continue
          const call = block.tool_use_id ? toolCalls.get(block.tool_use_id) : undefined
          const result: ToolResult = {
            content: stringifyContent(block.content),
            isError: Boolean(block.is_error),
          }
          if (call) {
            call.result = result
          } else {
            // A result without its call still deserves to be shown.
            entries.push({
              kind: 'tool_use',
              seq: event.seq,
              id: block.tool_use_id ?? '',
              name: 'tool result',
              input: undefined,
              result,
            })
          }
        }
        break
      }

      case 'result': {
        entries.push({
          kind: 'result',
          seq: event.seq,
          text: String(payload.result ?? ''),
          isError: Boolean(payload.is_error),
        })
        break
      }
    }
  }

  return entries
}

function stringifyContent(content: unknown): string {
  if (content == null) return ''
  if (typeof content === 'string') return content
  if (Array.isArray(content)) {
    return content
      .map((part) => {
        if (typeof part === 'string') return part
        if (part && typeof part === 'object' && 'text' in part) {
          return String((part as { text?: unknown }).text ?? '')
        }
        return JSON.stringify(part)
      })
      .join('\n')
  }
  return JSON.stringify(content, null, 2)
}
