import { useState } from 'react'
import { Code2, Star, Table2 } from 'lucide-react'
import type { ParsedTable } from '../api/types'
import { Badge, Button, Pre, cx } from './ui'
import { pageRange } from '../lib/format'

// TableView 按 cells 的 row_span / col_span 还原合并单元格渲染表格；可切换查看 Markdown 与原始 JSON。
export function TableView({ table, index }: { table: ParsedTable; index?: number }) {
  const [mode, setMode] = useState<'table' | 'markdown' | 'json'>('table')
  const byRow = new Map<number, ParsedTable['cells']>()
  for (const c of table.cells) {
    const list = byRow.get(c.row) ?? []
    list.push(c)
    byRow.set(c.row, list)
  }
  const pages = new Set(table.cells.map((c) => c.page))
  return (
    <div className="rounded-md ring-1 ring-slate-200">
      <div className="flex flex-wrap items-center gap-2 border-b border-slate-100 bg-slate-50 px-2 py-1 text-xs text-slate-500">
        <Table2 className="size-3.5 text-blue-600" />
        <span className="font-medium text-slate-700">表格{index !== undefined ? ` ${index + 1}` : ''}</span>
        <span>{table.rows} 行 × {table.cols} 列</span>
        {table.header_rows > 0 && <span>表头 {table.header_rows} 行</span>}
        <Badge tone={table.page_end > table.page_start ? 'purple' : 'slate'}>{pageRange(table.page_start, table.page_end)}{table.page_end > table.page_start ? ' 跨页' : ''}</Badge>
        {table.mandatory && <Badge tone="red"><Star className="size-3 fill-red-500" />含强制参数</Badge>}
        <span className="text-slate-400">来源 {table.source}</span>
        <div className="ml-auto flex gap-1">
          <Button size="sm" variant={mode === 'table' ? 'secondary' : 'ghost'} onClick={() => setMode('table')}>表格</Button>
          <Button size="sm" variant={mode === 'markdown' ? 'secondary' : 'ghost'} onClick={() => setMode('markdown')}>Markdown</Button>
          <Button size="sm" variant={mode === 'json' ? 'secondary' : 'ghost'} icon={<Code2 className="size-3" />} onClick={() => setMode('json')}>JSON</Button>
        </div>
      </div>
      {mode === 'table' ? (
        <div className="max-h-96 overflow-auto">
          <table className="w-full border-collapse text-xs">
            <tbody>
              {Array.from({ length: table.rows }, (_, r) => (
                <tr key={r} className={cx(r < table.header_rows && 'bg-slate-100 font-medium')}>
                  {(byRow.get(r) ?? []).sort((a, b) => a.col - b.col).map((c) => (
                    <td key={`${c.row}-${c.col}`} rowSpan={c.row_span} colSpan={c.col_span}
                      className={cx('border border-slate-200 px-2 py-1 align-top whitespace-pre-wrap',
                        /^\s*[★▲]/.test(c.text) && 'bg-red-50 text-red-800')}
                      title={pages.size > 1 ? `第 ${c.page} 页` : undefined}>
                      {c.text}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Pre className="m-2">{mode === 'markdown' ? table.markdown : JSON.stringify(table, null, 2)}</Pre>
      )}
    </div>
  )
}
