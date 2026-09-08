import { createLowlight } from 'lowlight'
import { visit } from 'unist-util-visit'
import type { Element, Root } from 'hast'

// Only the languages this tool's output actually contains.
//
// rehype-highlight is deliberately not used: it statically imports lowlight's
// `common` set, so rollup cannot tree-shake the ~37 grammars it pulls in even
// when a subset is configured. Measured on this bundle, configuring a subset
// through rehype-highlight changed nothing (209 kB gzipped either way);
// building the lowlight instance here instead brought it to 173 kB.
import bash from 'highlight.js/lib/languages/bash'
import diff from 'highlight.js/lib/languages/diff'
import dockerfile from 'highlight.js/lib/languages/dockerfile'
import go from 'highlight.js/lib/languages/go'
import ini from 'highlight.js/lib/languages/ini'
import javascript from 'highlight.js/lib/languages/javascript'
import json from 'highlight.js/lib/languages/json'
import python from 'highlight.js/lib/languages/python'
import sql from 'highlight.js/lib/languages/sql'
import typescript from 'highlight.js/lib/languages/typescript'
import yaml from 'highlight.js/lib/languages/yaml'

const lowlight = createLowlight({
  bash,
  diff,
  dockerfile,
  go,
  ini,
  javascript,
  json,
  python,
  sql,
  typescript,
  yaml,
})

lowlight.registerAlias({
  bash: ['sh', 'shell', 'zsh', 'console'],
  ini: ['toml', 'conf', 'cfg', 'properties'],
  javascript: ['js', 'jsx', 'mjs'],
  typescript: ['ts', 'tsx'],
  yaml: ['yml'],
})

/** Reads the fenced language from a code element's class list. */
function languageOf(node: Element): string {
  const classes = node.properties?.className
  const list = Array.isArray(classes) ? classes.map(String) : []
  for (const name of list) {
    if (name === 'no-highlight' || name === 'nohighlight') return ''
    if (name.startsWith('language-')) return name.slice('language-'.length)
    if (name.startsWith('lang-')) return name.slice('lang-'.length)
  }
  return ''
}

/** Concatenates the text of a hast subtree. */
function textOf(node: { type?: string; value?: string; children?: unknown[] }): string {
  if (node.type === 'text') return node.value ?? ''
  const children = (node.children ?? []) as Array<Parameters<typeof textOf>[0]>
  return children.map(textOf).join('')
}

/**
 * Highlights fenced code blocks whose language is registered above.
 *
 * A block with no language, or one this build does not carry a grammar for,
 * is left as plain text rather than guessed at: a wrong guess colours the
 * code misleadingly, which is worse than no colour at all.
 */
export function rehypeHighlightSubset() {
  return (tree: Root) => {
    visit(tree, 'element', (node: Element, _index, parent) => {
      if (node.tagName !== 'code') return
      const parentElement = parent as Element | undefined
      if (parentElement?.type !== 'element' || parentElement.tagName !== 'pre') return

      const language = languageOf(node)
      if (!language || !lowlight.registered(language)) return

      const source = textOf(node)
      if (!source) return

      try {
        const highlighted = lowlight.highlight(language, source)
        node.children = highlighted.children as Element['children']
        // Mark the element so the stylesheet's hljs-* rules apply.
        const existing = Array.isArray(node.properties.className)
          ? node.properties.className.map(String)
          : []
        node.properties.className = [...new Set([...existing, 'hljs'])]
      } catch {
        // A grammar failing on unusual input must not break the transcript;
        // the block stays plain.
      }
    })
  }
}
