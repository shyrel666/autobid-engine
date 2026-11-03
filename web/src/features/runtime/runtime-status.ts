// 运行能力状态的纯逻辑（便于 node --test 单测）：状态文案、色调与顶栏汇总。
import type { RuntimeCapability, RuntimeCapabilityStatus, RuntimeState } from '../../api/types'

export const runtimeStateLabel: Record<RuntimeState, string> = {
  unconfigured: '未配置', starting: '检查中', healthy: '正常', degraded: '最近调用失败', self_test_failed: '自检失败',
}

export function runtimeStateTone(s: RuntimeState): 'green' | 'amber' | 'red' | 'slate' | 'blue' {
  switch (s) {
    case 'healthy': return 'green'
    case 'starting': return 'blue'
    case 'degraded': return 'amber'
    case 'self_test_failed': return 'red'
    default: return 'slate'
  }
}

/** 顶栏汇总只看业务主流程依赖的能力；增强 PDF 与语义检索模型是可选能力，只在明细中展示。 */
export const KEY_CAPABILITIES: RuntimeCapability[] = ['office', 'mermaid', 'ocr']

export interface RuntimeSummary { tone: 'green' | 'amber' | 'red' | 'blue' | 'slate'; label: string; problems: RuntimeCapabilityStatus[] }

export function summarizeRuntime(list: RuntimeCapabilityStatus[] | undefined): RuntimeSummary {
  if (!list) return { tone: 'slate', label: '运行能力读取中', problems: [] }
  const key = list.filter((c) => KEY_CAPABILITIES.includes(c.capability))
  const failed = key.filter((c) => c.state === 'self_test_failed' || c.state === 'degraded')
  const missing = key.filter((c) => c.state === 'unconfigured')
  if (failed.length) return { tone: 'red', label: `${failed.length} 项运行能力异常`, problems: [...failed, ...missing] }
  if (key.some((c) => c.state === 'starting')) return { tone: 'blue', label: '运行能力检查中', problems: missing }
  if (missing.length) return { tone: 'amber', label: `${missing.length} 项运行能力未配置`, problems: missing }
  return { tone: 'green', label: '运行能力正常', problems: [] }
}

/** 仍有能力在自检时需要轮询，自检结束后停止。 */
export function runtimeNeedsPoll(list: RuntimeCapabilityStatus[] | undefined): boolean {
  return !!list?.some((c) => c.state === 'starting')
}
