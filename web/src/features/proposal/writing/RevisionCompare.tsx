import { useMemo, useState } from 'react'
import type { SectionRevision } from '../../../api/types'
import { Button, cx } from '../../../components/ui'
import { Markdown } from '../../../lib/markdown'

// 两个修订并排查看；逐行对比为基于 LCS 的计算结果。修改说明是作者填写的说明，不是自动计算的差异。

export function diffLines(a: string, b: string): { op: ' ' | '-' | '+'; line: string }[] {
  const x = a.split('\n').slice(0, 1500), y = b.split('\n').slice(0, 1500)
  const dp = Array.from({ length: x.length + 1 }, () => new Array<number>(y.length + 1).fill(0))
  for (let i = x.length - 1; i >= 0; i--) for (let j = y.length - 1; j >= 0; j--)
    dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out: { op: ' ' | '-' | '+'; line: string }[] = []
  let i = 0, j = 0
  while (i < x.length && j < y.length) {
    if (x[i] === y[j]) { out.push({ op: ' ', line: x[i] }); i++; j++ }
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ op: '-', line: x[i++] })
    else out.push({ op: '+', line: y[j++] })
  }
  while (i < x.length) out.push({ op: '-', line: x[i++] })
  while (j < y.length) out.push({ op: '+', line: y[j++] })
  return out
}

export default function RevisionCompare({ left, right, leftLabel, rightLabel }: { left: SectionRevision | string; right: SectionRevision; leftLabel: string; rightLabel: string }) {
  const [mode, setMode] = useState<'side' | 'diff'>('side')
  const leftText = typeof left === 'string' ? left : left.content_md
  const diff = useMemo(() => (mode === 'diff' ? diffLines(leftText, right.content_md) : []), [mode, leftText, right.content_md])
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2">
        <Button size="sm" variant={mode === 'side' ? 'primary' : 'secondary'} onClick={() => setMode('side')}>并排查看</Button>
        <Button size="sm" variant={mode === 'diff' ? 'primary' : 'secondary'} onClick={() => setMode('diff')}>逐行对比</Button>
        {typeof left !== 'string' && left.diff_summary && <span className="text-xs text-muted">左侧修改说明：{left.diff_summary}</span>}
        {right.diff_summary && <span className="text-xs text-muted">右侧修改说明：{right.diff_summary}</span>}
      </div>
      {mode === 'side' ? (
        <div className="grid min-h-0 flex-1 grid-cols-1 overflow-auto md:grid-cols-2 md:divide-x md:divide-line md:overflow-hidden">
          {[{ label: leftLabel, text: leftText }, { label: rightLabel, text: right.content_md }].map((c) => (
            <div key={c.label} className="min-h-0 overflow-auto p-4">
              <div className="mb-2 text-xs text-muted">{c.label}</div>
              {c.text ? <Markdown source={c.text} /> : <p className="text-sm text-muted">（无正文）</p>}
            </div>
          ))}
        </div>
      ) : (
        <pre className="min-h-0 flex-1 overflow-auto p-4 font-mono text-xs leading-5">
          {diff.map((d, i) => <div key={i} className={cx(d.op === '+' ? 'bg-[#ecfdf5] text-[#065f46]' : d.op === '-' ? 'bg-[#fef2f2] text-[#991b1b] line-through' : 'text-muted')}>{d.op} {d.line}</div>)}
        </pre>
      )}
    </div>
  )
}
