import { useState } from 'react'
import { getOperator, setOperator } from '../../api/client'

// 当前操作人写入审计字段，粉队 / 红队的作者与评审人分离也按它判定。它只是审计声明，不是登录身份（Phase 7 前）。
// 证据页和写作页都要能切换，否则一个人无法在页面上完成“作者提交、评审人批准”。
export function OperatorField() {
  const [name, setName] = useState(getOperator)
  return (
    <label className="flex items-center gap-2 text-xs text-muted" title="用于审计记录和评审人分离判定，不是登录身份">
      当前操作人
      <input value={name} placeholder="填写姓名" aria-label="当前操作人"
        onChange={(e) => { setName(e.target.value); setOperator(e.target.value.trim()) }}
        className="h-7 w-28 rounded-lg border border-line bg-canvas px-2 text-fg" />
    </label>
  )
}
