import type { ReactNode } from 'react'

// 安全的 Markdown 预览：只把正文当内容渲染为 React 元素，不使用 innerHTML、不执行原始 HTML 与脚本；
// 链接只允许 http(s)、mailto 与页内锚点，其余按纯文本显示。预览不等于最终 DOCX / PDF 版式。

function safeHref(url: string): string | undefined {
  const u = url.trim()
  if (/^(https?:|mailto:)/i.test(u) || u.startsWith('#')) return u
  return undefined
}

function inline(text: string, key: string): ReactNode[] {
  const out: ReactNode[] = []
  const re = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*)|(\[[^\]]+\]\([^)\s]+\))/g
  let last = 0
  let m: RegExpExecArray | null
  let i = 0
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const t = m[0]
    const k = `${key}-${i++}`
    if (m[1]) out.push(<code key={k} className="rounded bg-soft px-1 font-mono text-[0.9em]">{t.slice(1, -1)}</code>)
    else if (m[2]) out.push(<strong key={k}>{inline(t.slice(2, -2), k)}</strong>)
    else if (m[3]) out.push(<em key={k}>{inline(t.slice(1, -1), k)}</em>)
    else {
      const lm = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(t)!
      const href = safeHref(lm[2])
      out.push(href ? <a key={k} href={href} target="_blank" rel="noopener noreferrer nofollow" className="text-link underline">{lm[1]}</a> : `${lm[1]}（${lm[2]}）`)
    }
    last = m.index + t.length
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}

const isTableSep = (l: string) => /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$/.test(l)
const cells = (l: string) => l.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim())

export function Markdown({ source }: { source: string }) {
  const lines = source.replace(/\r\n?/g, '\n').split('\n')
  // 标题按相对层级排版：正文中最浅的一级（章节正文通常以 ## 开头）用 20px，下一级 16px，其余 14px。
  let top = 6
  let fenced = false
  for (const l of lines) {
    if (/^```/.test(l)) fenced = !fenced
    const m = fenced ? null : /^(#{1,6})\s+/.exec(l)
    if (m) top = Math.min(top, m[1].length)
  }
  const blocks: ReactNode[] = []
  let i = 0
  let n = 0
  while (i < lines.length) {
    const line = lines[i]
    const k = `b${n++}`
    if (!line.trim()) { i++; continue }
    const fence = /^```/.exec(line)
    if (fence) {
      const body: string[] = []
      i++
      while (i < lines.length && !/^```/.test(lines[i])) body.push(lines[i++])
      i++
      blocks.push(<pre key={k} className="my-3 overflow-x-auto rounded-lg border border-line bg-soft p-3 font-mono text-xs leading-5">{body.join('\n')}</pre>)
      continue
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line)
    if (h) {
      const rel = h[1].length - top
      const cls = ['mb-4 text-xl leading-7 font-bold', 'mt-5 mb-2.5 text-base leading-5 font-bold', 'mt-4 mb-2 text-sm font-semibold', 'mt-3 mb-2 text-sm font-semibold', 'mt-3 mb-1 text-sm font-medium', 'mt-3 mb-1 text-sm font-medium'][rel]
      const Tag = (`h${Math.min(rel + 2, 6)}`) as 'h2'
      blocks.push(<Tag key={k} className={cls}>{inline(h[2], k)}</Tag>)
      i++
      continue
    }
    if (/^(-{3,}|\*{3,})\s*$/.test(line)) { blocks.push(<hr key={k} className="my-4 border-line" />); i++; continue }
    if (/^>\s?/.test(line)) {
      const body: string[] = []
      while (i < lines.length && /^>\s?/.test(lines[i])) body.push(lines[i++].replace(/^>\s?/, ''))
      blocks.push(<blockquote key={k} className="my-3 border-l-4 border-line pl-3 text-muted">{inline(body.join(' '), k)}</blockquote>)
      continue
    }
    if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = cells(line)
      const rows: string[][] = []
      i += 2
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) rows.push(cells(lines[i++]))
      blocks.push(
        <div key={k} className="my-3 overflow-x-auto">
          <table className="w-full border-collapse text-sm">
            <thead><tr>{head.map((c, j) => <th key={j} className="border border-line bg-soft px-2 py-1 text-left font-medium">{inline(c, `${k}h${j}`)}</th>)}</tr></thead>
            <tbody>{rows.map((r, ri) => <tr key={ri}>{r.map((c, j) => <td key={j} className="border border-line px-2 py-1 align-top">{inline(c, `${k}r${ri}c${j}`)}</td>)}</tr>)}</tbody>
          </table>
        </div>,
      )
      continue
    }
    const ul = /^\s*[-*+]\s+/, ol = /^\s*\d+[.、)]\s+/
    if (ul.test(line) || ol.test(line)) {
      const ordered = ol.test(line)
      const re = ordered ? ol : ul
      const items: string[] = []
      while (i < lines.length) {
        if (re.test(lines[i])) {
          items.push(lines[i++].replace(re, '').trim())
        } else if (
          items.length > 0 &&
          lines[i].trim() &&
          !/^(#{1,6}\s|```|>|(-{3,}|\*{3,})\s*$|\s*[-*+]\s|\s*\d+[.、)]\s)/.test(lines[i])
        ) {
          items[items.length - 1] += ' ' + lines[i++].trim()
        } else {
          break
        }
      }
      const L = ordered ? 'ol' : 'ul'
      blocks.push(<L key={k} className={`my-2 space-y-1 pl-6 leading-[26px] ${ordered ? 'list-decimal' : 'list-disc'}`}>{items.map((t, j) => <li key={j}>{inline(t, `${k}i${j}`)}</li>)}</L>)
      continue
    }
    const para: string[] = []
    while (i < lines.length && lines[i].trim() && !/^(#{1,6}\s|```|>|\s*[-*+]\s|\s*\d+[.、)]\s)/.test(lines[i])) para.push(lines[i++])
    if (para.length === 0) { para.push(lines[i++]) }
    blocks.push(<p key={k} className="mb-3 leading-[26px]">{inline(para.join('\n'), k)}</p>)
  }
  return <div className="text-sm text-fg [overflow-wrap:anywhere] whitespace-pre-wrap [&_li]:whitespace-normal [&_table]:whitespace-normal">{blocks}</div>
}
