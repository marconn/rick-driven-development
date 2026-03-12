import { marked } from 'marked'
import hljs from 'highlight.js'
import mermaid from 'mermaid'
import 'highlight.js/styles/github.css'

mermaid.initialize({
  startOnLoad: false,
  theme: 'default',
  securityLevel: 'loose',
  fontFamily: "'Inter', sans-serif",
})

let mermaidCounter = 0

const renderer = new marked.Renderer()
renderer.code = function ({ text, lang }: { text: string; lang?: string }) {
  if (lang === 'mermaid') {
    const id = `mermaid-${++mermaidCounter}`
    return `<div class="mermaid" id="${id}">${text}</div>`
  }
  const highlighted =
    lang && hljs.getLanguage(lang)
      ? hljs.highlight(text, { language: lang }).value
      : hljs.highlightAuto(text).value
  return `<pre><code class="hljs language-${lang || ''}">${highlighted}</code></pre>`
}

marked.setOptions({
  breaks: true,
  gfm: true,
})
marked.use({ renderer })

export function renderMarkdown(text: string): string {
  if (!text) return ''
  return marked.parse(text, { async: false }) as string
}

export async function renderMermaidIn(container: HTMLElement): Promise<void> {
  const nodes = container.querySelectorAll<HTMLElement>('.mermaid:not([data-processed])')
  if (nodes.length === 0) return
  await mermaid.run({ nodes: Array.from(nodes) })
}
