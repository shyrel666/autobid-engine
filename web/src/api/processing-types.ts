import type { DocumentChunk } from './types'

export interface ProcessingFile {
  id: number; original_name: string; archive_path?: string; parent_file_id?: number
  format: string; kind: string; sha256: string; byte_size: number; availability: string
}
export interface ProcessingRecord {
  id: number; file_id: number; operation: 'import' | 'reparse'; status: 'ready' | 'adopted' | 'failed'
  expected_revision: number; target_document_id?: number; error?: string; created_at: string; approved_by: string
  result: { document?: { id: number }; document_id?: number; source_bumped?: boolean }
}
export interface ProcessingDetail {
  record: ProcessingRecord; stale: boolean; warnings?: string[]; total_changes: number; offset: number; has_more: boolean
  preview: { inserted: number; updated: number; deleted: number; unchanged: number; affected_requirements: number
    changes: { seq: number; kind: string; before?: DocumentChunk; after?: DocumentChunk }[] }
}

export interface TaskMaterial {
  id: number; name: string; required: boolean; file_id: number | null; version: number
  note: string; updated_by: string; updated_at: string
  state: 'missing' | 'unavailable' | 'processing' | 'review' | 'not_imported' | 'complete'
}
export interface FixedJobInput {
  request_key: string; file_id?: number; document_id?: number; expected_revision?: number
  tender_no?: string; lot_no?: string; title?: string; doc_role?: string; published_at?: string; operation?: 'archive'
}
export interface FixedJob {
  id: number; task_id: number; file_id: number; input: FixedJobInput
  status: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  attempts: number; max_attempts: number; record_id?: number; error?: string; created_at: string
}
