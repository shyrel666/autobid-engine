import type { DocumentChunk } from './types'

export interface ProcessingFile {
  id: number; original_name: string; archive_path?: string; parent_file_id?: number
  format: string; kind: string; sha256: string; byte_size: number; availability: string
}
export type ProcessingRecordStatus = 'ready' | 'adopted' | 'failed' | 'rejected'
/** S5a 解析路由：auto 文本层 + 扫描页 OCR；native 不调用 OCR；ocr_only PDF 每页与图片都用 OCR（enhanced_pdf 尚未交付，不出现在页面选项中）。 */
export type ParserProfile = 'auto' | 'native' | 'ocr_only'
export interface ProcessingRecord {
  id: number; file_id: number; operation: 'import' | 'reparse'; status: ProcessingRecordStatus
  expected_revision: number; target_document_id?: number; error?: string; created_at: string; approved_by: string
  created_by?: string; review_note?: string
  /** 导入只生成候选（人工采用后才写正式招标文档）；重新解析恒为候选。 */
  candidate_only?: boolean
  parser_profile?: ParserProfile | ''; parser_version?: string
  rejected_by?: string; reject_reason?: string; rejected_at?: string
  result: { document?: { id: number }; document_id?: number; source_bumped?: boolean }
}

export interface ParseGap { page?: number; kind: string; detail?: string }
export interface CandidateBox { page: number; x0: number; y0: number; x1: number; y1: number; confidence?: number; coord?: string; img_w?: number; img_h?: number }
export interface CandidateLocator {
  file_id: number; sha256: string; page_start?: number; page_end?: number; page_is_physical: boolean
  ocr?: boolean; ocr_lines?: number; min_confidence?: number; box_count?: number; boxes?: CandidateBox[]
  source_locator?: { sheet?: string; range?: string; kind?: string }
}
export interface CandidateSection {
  seq: number; section_code?: string; section_title?: string; is_mandatory?: boolean; tables?: number
  chars: number; content: string; truncated?: boolean; locator: CandidateLocator
}
/** 服务端保存的完整解析候选（分页），每段附原件定位；OCR 文字是机器识别结果，未经人工核验。 */
export interface CandidatePage {
  record_id: number; operation: 'import' | 'reparse'; status: ProcessingRecordStatus; file_id: number; source_hash: string
  parser_profile?: string; parser_version?: string
  document: { tender_no: string; lot_no?: string; title: string; doc_role?: string; published_at?: string; file_name?: string; format?: string; page_count?: number }
  total_sections: number; ocr_pages?: number[]; gaps: ParseGap[]; warnings?: string[]
  sections: CandidateSection[]; cursor: number; next_cursor: number
}
export interface ProcessingDetail {
  record: ProcessingRecord; stale: boolean; warnings?: string[]; gaps?: ParseGap[]; total_changes: number; offset: number; has_more: boolean
  preview: { inserted: number; updated: number; deleted: number; unchanged: number; affected_requirements: number
    changes: { seq: number; kind: string; before?: DocumentChunk; after?: DocumentChunk }[] }
  /** 候选分段（按 cursor 每页 20 段）；解析失败的记录为空。 */
  candidate?: CandidatePage | null
}

export interface TaskMaterial {
  id: number; name: string; required: boolean; file_id: number | null; version: number
  note: string; updated_by: string; updated_at: string
  state: 'missing' | 'unavailable' | 'processing' | 'review' | 'not_imported' | 'complete'
}
export interface FixedJobInput {
  request_key: string; file_id?: number; document_id?: number; expected_revision?: number
  tender_no?: string; lot_no?: string; title?: string; doc_role?: string; published_at?: string; operation?: 'archive'
  candidate_only?: boolean; parser_profile?: ParserProfile
}
export interface FixedJob {
  id: number; task_id: number; file_id: number; input: FixedJobInput
  status: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  attempts: number; max_attempts: number; record_id?: number; error?: string; created_at: string
  created_by?: string; deadline_at?: string
  /** 首次提交时固定的解析路由（解析器版本，含 OCR 时附锁定的 OCR 服务版本）。 */
  executor?: string
  /** RUNTIME_UNAVAILABLE / BUDGET_EXHAUSTED / PARSE_FAILED / CANCELLED */
  error_code?: string
  record_status?: ProcessingRecordStatus
}
