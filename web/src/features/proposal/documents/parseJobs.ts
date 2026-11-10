// S5a 招标解析的展示规则（纯函数，单测见 parseJobs.test.ts）：
// 处理中、候选待核对、已采用、已拒绝、失败 / 取消分开显示；作业成功只表示“候选就绪”，不显示为已导入；
// OCR 识别结果标明“机器识别、未经人工核验”，不显示为证据已核验；增强解析后端尚未交付，不出现在可提交选项中。

import type { CandidateLocator, FixedJob, ParseGap, ParserProfile, ProcessingRecord } from '../../../api/processing-types'
import type { RuntimeCapabilityStatus } from '../../../api/types'
import type { Tone } from '../../../components/ui'

export interface StateView { label: string; tone: Tone; note?: string }

const ERROR_CODE_LABEL: Record<string, string> = {
  RUNTIME_UNAVAILABLE: '本机 OCR 不可用或版本已变化',
  BUDGET_EXHAUSTED: '尝试次数或截止时间已耗尽',
  PARSE_FAILED: '解析失败',
  CANCELLED: '已取消',
}

/** 作业是否由 Agent（Worker）提交。 */
export function isAgentJob(j: Pick<FixedJob, 'created_by'>): boolean {
  return j.created_by === 'worker'
}

/** OCR 是否参与了解析路由（executor / parser_version 中带锁定的 OCR 版本）。 */
export function usesOCR(version: string | undefined): boolean {
  return !!version && version.includes('+ocr:')
}

/** 执行版本的简短说明：解析器 + OCR 引擎与模型（完整版本放在 title 中）。 */
export function executorLabel(executor: string | undefined): string {
  if (!executor) return '旧作业（未固定执行版本）'
  if (executor === 'zip-manifest-v1') return 'ZIP 清单展开'
  const [parser, ocr] = executor.split('+ocr:')
  if (!ocr) return `${parser}（不使用 OCR）`
  const parts = ocr.split(';').map((s) => s.trim())
  const models = parts.filter((p) => p.includes('@')).map((p) => p.split('@')[0])
  return `${parser} + ${parts[0]}${models.length ? `（${models.join(' / ')}）` : ''}`
}

type JobView = StateView & { canCancel: boolean; canRetry: boolean; canReview: boolean }

/** 固定解析作业的状态：作业状态与候选业务状态分开。
 *  enabled=false 表示服务器关闭了材料处理：作业不会执行，只能取消；到提交时固定的截止时间后由服务器结束。
 *  重新处理和核对候选都要等重新启用，所以不给入口。 */
export function parseJobView(j: FixedJob, enabled = true): JobView {
  const view = jobStateView(j)
  if (enabled) return view
  const active = j.status === 'queued' || j.status === 'running'
  return { ...view, canRetry: false, canReview: false,
    label: active ? '已停止执行' : view.label, tone: active ? 'amber' : view.tone,
    note: active ? '材料处理已关闭，这个作业不会执行；到提交时固定的截止时间后自动结束，也可以现在取消。重新启用后，截止前会继续处理' : view.note }
}

function jobStateView(j: FixedJob): JobView {
  const base = { canCancel: false, canRetry: false, canReview: false }
  if (j.status === 'queued' || j.status === 'running') {
    const retrying = j.status === 'queued' && j.error_code === 'RUNTIME_UNAVAILABLE' && j.attempts > 0
    return { ...base, canCancel: true, label: j.status === 'queued' ? (retrying ? '等待重试' : '排队中') : '处理中', tone: 'blue',
      note: retrying ? `上次 OCR 暂时不可达，将在原截止时间内重试（第 ${j.attempts}/${j.max_attempts} 次）` : undefined }
  }
  if (j.status === 'cancelled') {
    return { ...base, canRetry: true, label: '已取消', tone: 'slate', note: j.error && j.error !== '用户取消' ? j.error : undefined }
  }
  if (j.status === 'failed') {
    const code = j.error_code ? ERROR_CODE_LABEL[j.error_code] ?? j.error_code : '处理失败'
    return { ...base, canRetry: true, label: `失败 · ${code}`, tone: 'red', note: j.error }
  }
  switch (j.record_status) {
    case 'ready':
      return { ...base, canReview: true, label: '候选待核对', tone: 'amber', note: '解析已完成，候选须对照原件人工采用后才成为正式招标文档' }
    case 'adopted':
      return { ...base, canReview: true, label: '已采用', tone: 'green' }
    case 'rejected':
      return { ...base, canReview: true, label: '候选已拒绝', tone: 'slate' }
    case 'failed':
      return { ...base, canRetry: true, label: '失败 · 解析失败', tone: 'red', note: j.error }
  }
  return { ...base, label: '处理完成', tone: 'green' }
}

/** 处理记录（候选）的状态。 */
export function recordStatusView(r: ProcessingRecord): StateView {
  const ocr = usesOCR(r.parser_version)
  switch (r.status) {
    case 'ready':
      return { label: r.operation === 'import' ? '导入候选待核对' : '解析修订待核对', tone: 'amber',
        note: ocr ? '含 OCR 机器识别文字，未经人工核验' : undefined }
    case 'adopted':
      return { label: '已采用', tone: 'green', note: r.approved_by ? `采用人 ${r.approved_by}` : undefined }
    case 'rejected':
      return { label: '已拒绝', tone: 'slate', note: [r.rejected_by && `拒绝人 ${r.rejected_by}`, r.reject_reason && `原因：${r.reject_reason}`].filter(Boolean).join(' · ') || undefined }
    default:
      return { label: '处理失败', tone: 'red', note: r.error }
  }
}

export interface ProfileOption { value: ParserProfile; label: string; disabled: boolean; hint: string }

/** 页面可选的解析配置；ocr_only 需要可用的 OCR（检查中也允许提交，服务器在提交时再核对）。 */
export function profileOptions(ocr: RuntimeCapabilityStatus | undefined): ProfileOption[] {
  const ocrUsable = !!ocr && (ocr.available || ocr.state === 'starting')
  return [
    { value: 'auto', label: '自动（文本层 + 扫描页 OCR）', disabled: false,
      hint: ocrUsable ? '有文本层的页面直接解析，扫描页经本机 OCR 识别' : 'OCR 不可用：扫描页记为缺口，图片文件无法提交' },
    { value: 'native', label: '仅文本层（不调用 OCR）', disabled: false, hint: '扫描页记为缺口，不当作空白页' },
    { value: 'ocr_only', label: '全部 OCR（忽略 PDF 文本层）', disabled: !ocrUsable,
      hint: ocrUsable ? '适用于文本层错乱的扫描件；没有页面图像的页记为缺口' : '本机 OCR 不可用' },
  ]
}

const GAP_LABEL: Record<string, string> = {
  ocr_unavailable: '扫描页未识别（OCR 不可用）',
  ocr_not_requested: '扫描页未识别（仅文本层配置）',
  ocr_failed: 'OCR 识别失败',
  ocr_empty: 'OCR 没有识别出文字',
  ocr_low_confidence: '存在低置信度识别行',
  image_encoding_unsupported: '扫描图像编码不支持',
  no_page_image: '页面没有可识别的图像',
}

export function gapText(g: ParseGap): string {
  const label = GAP_LABEL[g.kind] ?? g.kind
  return g.page ? `第 ${g.page} 页：${label}` : label
}

/** 原件定位的文字说明：物理页码、OCR 行数与最低置信度，或表格类文档的工作表区域。 */
export function locatorText(l: CandidateLocator): string {
  const parts: string[] = []
  if (l.page_is_physical && l.page_start) parts.push(l.page_end && l.page_end !== l.page_start ? `第 ${l.page_start}–${l.page_end} 页` : `第 ${l.page_start} 页`)
  else if (l.source_locator?.sheet) parts.push(`工作表「${l.source_locator.sheet}」${l.source_locator.range ? ` ${l.source_locator.range}` : ''}`)
  else parts.push('无物理页码')
  if (l.ocr) parts.push(`OCR ${l.ocr_lines ?? l.box_count ?? 0} 行${l.min_confidence ? ` · 最低置信度 ${l.min_confidence.toFixed(2)}` : ''}`)
  return parts.join(' · ')
}

/** 同一次提交的重发复用请求键；输入变化时换新键（服务端按键幂等，按钮禁用只改善体验）。 */
export function keyForInput(keys: Map<string, string>, input: unknown, make: () => string = () => crypto.randomUUID()): string {
  const sig = JSON.stringify(input)
  let key = keys.get(sig)
  if (!key) {
    key = make()
    keys.set(sig, key)
  }
  return key
}
