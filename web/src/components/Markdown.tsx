import { memo, useState, type ReactNode } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Check, Copy } from 'lucide-react'

import { rehypeHighlightSubset } from './highlight'

const remarkPlugins = [remarkGfm]
const rehypePlugins = [rehypeHighlightSubset]

/** A minimal hast node, enough to recover a code block's text and language. */
interface HastNode {
  type?: string
  tagName?: string
  value?: string
  properties?: { className?: unknown }
  children?: HastNode[]
}

/** Concatenates the text of a hast subtree.
 *
 * The syntax highlighter has already replaced the code element's children
 * with nested spans by the time components render, so the original source
 * has to be walked back out of the tree for the copy button. */
function textOf(node: HastNode | undefined): string {
  if (!node) return ''
  if (node.type === 'text') return node.value ?? ''
  return (node.children ?? []).map(textOf).join('')
}

function languageOf(node: HastNode | undefined): string {
  const classes = node?.properties?.className
  const list = Array.isArray(classes) ? classes.map(String) : []
  for (const name of list) {
    if (name.startsWith('language-')) return name.slice('language-'.length)
  }
  return ''
}

/** Renders Claude's output as markdown.
 *
 * Raw HTML is deliberately not enabled: this is model output, and
 * react-markdown builds a React tree rather than setting innerHTML, so
 * nothing in a transcript can inject markup. */
const Markdown = memo(function Markdown({ children }: { children: string }) {
  return (
    <div className="md">
      <ReactMarkdown
        remarkPlugins={remarkPlugins}
        rehypePlugins={rehypePlugins}
        components={{
          pre: PreBlock,
          a: Anchor,
          img: Image,
          table: Table,
          input: Checkbox,
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  )
})

/** A fenced code block with a language label and a copy button. */
function PreBlock({ children, node }: { children?: ReactNode; node?: unknown }) {
  const [copied, setCopied] = useState(false)

  const element = node as HastNode | undefined
  const codeNode = (element?.children ?? []).find((child) => child.tagName === 'code')
  const source = textOf(codeNode)
  const language = languageOf(codeNode)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(source)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard access can be refused; the code is on screen regardless.
    }
  }

  return (
    <div className="md-code group">
      <div className="md-code-bar">
        <span>{language || 'text'}</span>
        <button onClick={copy} aria-label="Copy code" title="Copy code">
          {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre>{children}</pre>
    </div>
  )
}

/** External links open in a new tab and leak no referrer. */
function Anchor({ href, children }: { href?: string; children?: ReactNode }) {
  const external = /^https?:\/\//i.test(href ?? '')
  return (
    <a
      href={href}
      target={external ? '_blank' : undefined}
      rel={external ? 'noreferrer noopener' : undefined}
    >
      {children}
    </a>
  )
}

/** Images are lazy and referrer-free: a transcript can name any URL, and
 *  this page is served from localhost. */
function Image({ src, alt }: { src?: string; alt?: string }) {
  return <img src={src} alt={alt ?? ''} loading="lazy" referrerPolicy="no-referrer" />
}

/** Wide tables scroll inside their own container rather than the page. */
function Table({ children }: { children?: ReactNode }) {
  return (
    <div className="md-table-wrap">
      <table>{children}</table>
    </div>
  )
}

export default Markdown

/** GFM task-list checkboxes are display only. */
function Checkbox({ checked, type }: { checked?: boolean; type?: string }) {
  if (type !== 'checkbox') return null
  return <input type="checkbox" checked={Boolean(checked)} readOnly disabled />
}
