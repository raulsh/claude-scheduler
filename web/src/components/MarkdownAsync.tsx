import { Suspense, lazy } from 'react'

/** The markdown renderer, loaded on demand.
 *
 * react-markdown and the highlighting grammars are about two thirds of the
 * JavaScript in this app, and only the execution transcript needs them. Split
 * out, the dashboard and executions list load without any of it, and the
 * chunk arrives while the transcript's first frame is already on screen.
 */
const Renderer = lazy(() => import('./Markdown'))

export function Markdown({ children }: { children: string }) {
  return (
    <Suspense fallback={<PlainText>{children}</PlainText>}>
      <Renderer>{children}</Renderer>
    </Suspense>
  )
}

/** Shown for the moment before the renderer arrives. The text is already
 *  readable as-is, so this shows the real content rather than a skeleton. */
function PlainText({ children }: { children: string }) {
  return <div className="text-sm leading-relaxed whitespace-pre-wrap">{children}</div>
}
