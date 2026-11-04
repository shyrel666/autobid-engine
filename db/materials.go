package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"time"
)

var ErrInvalidMaterial = errors.New("invalid required material")

type TaskMaterial struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Required  bool      `json:"required"`
	FileID    *int64    `json:"file_id"`
	Version   int       `json:"version"`
	Note      string    `json:"note"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
	State     string    `json:"state"`
}

func (m TaskMaterial) StateLabel() string {
	switch m.State {
	case "missing":
		return "尚未上传或绑定原件"
	case "unavailable":
		return "原件不可用"
	case "processing":
		return "正在处理"
	case "review":
		return "解析候选或修订待核对采用"
	case "not_imported":
		return "尚未正式导入"
	case "complete":
		return "已完成"
	}
	return m.State
}

func listTaskMaterials(ctx context.Context, q queryer, tenant string, taskID int64) ([]*TaskMaterial, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.id,m.name,m.required,m.file_id,m.version,m.note,m.updated_by,m.updated_at,
      CASE WHEN f.id IS NULL THEN 'missing'
        WHEN f.availability <> 'available' THEN 'unavailable'
        WHEN EXISTS (SELECT 1 FROM fixed_file_jobs j WHERE j.tenant_id=m.tenant_id AND j.task_id=m.task_id AND j.file_id=m.file_id AND j.status IN ('queued','running')) THEN 'processing'
        WHEN EXISTS (SELECT 1 FROM file_processing_records r JOIN tender_documents d ON d.id=r.target_document_id AND d.tenant_id=r.tenant_id AND d.task_id=r.task_id
          WHERE r.tenant_id=m.tenant_id AND r.task_id=m.task_id AND r.file_id=m.file_id AND r.status='ready' AND r.operation='reparse' AND r.expected_revision=d.parse_revision) THEN 'review'
        -- S5a: an import candidate waiting for a person to adopt or reject it.
        WHEN EXISTS (SELECT 1 FROM file_processing_records r WHERE r.tenant_id=m.tenant_id AND r.task_id=m.task_id AND r.file_id=m.file_id
          AND r.status='ready' AND r.operation='import' AND r.source_hash=f.sha256)
          AND NOT EXISTS (SELECT 1 FROM tender_documents d WHERE d.tenant_id=m.tenant_id AND d.task_id=m.task_id AND d.original_file_id=m.file_id AND d.content_hash=f.sha256) THEN 'review'
        WHEN NOT EXISTS (SELECT 1 FROM tender_documents d WHERE d.tenant_id=m.tenant_id AND d.task_id=m.task_id AND d.original_file_id=m.file_id AND d.content_hash=f.sha256) THEN 'not_imported'
        ELSE 'complete' END
      FROM task_materials m LEFT JOIN task_file_objects f ON f.tenant_id=m.tenant_id AND f.task_id=m.task_id AND f.id=m.file_id
      WHERE m.tenant_id=$1 AND m.task_id=$2 ORDER BY m.id`, tenantOr(tenant), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskMaterial{}
	for rows.Next() {
		m := new(TaskMaterial)
		if err := rows.Scan(&m.ID, &m.Name, &m.Required, &m.FileID, &m.Version, &m.Note, &m.UpdatedBy, &m.UpdatedAt, &m.State); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) ListTaskMaterials(ctx context.Context, tenant string, taskID int64) ([]*TaskMaterial, error) {
	return listTaskMaterials(ctx, d, tenant, taskID)
}

// SaveTaskMaterial uses an optimistic version and preserves an audit snapshot.
// No delete endpoint: excluding a material requires a reason, retaining its history.
func (d *DB) SaveTaskMaterial(ctx context.Context, tenant string, taskID int64, in TaskMaterial, actor string) (*TaskMaterial, error) {
	tenant = tenantOr(tenant)
	in.Name = strings.TrimSpace(in.Name)
	in.Note = strings.TrimSpace(in.Note)
	if in.Name == "" || len([]rune(in.Name)) > 200 || len(in.Note) > 4000 || (!in.Required && in.Note == "") {
		return nil, ErrInvalidMaterial
	}
	if in.FileID != nil && *in.FileID <= 0 {
		return nil, ErrInvalidMaterial
	}
	if in.ID < 0 || (in.ID > 0 && in.Version < 1) {
		return nil, ErrInvalidMaterial
	}
	err := d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, taskID); err != nil {
			return err
		}
		if in.FileID != nil {
			f, _, err := tx.GetTaskFileObject(ctx, tenant, taskID, *in.FileID)
			if err != nil {
				return err
			}
			if f.Kind == FileKindScript || f.Format == "zip" {
				return fmt.Errorf("%w: bind an individual input document", ErrInvalidMaterial)
			}
		}
		if in.ID == 0 {
			err := tx.QueryRowContext(ctx, `INSERT INTO task_materials(tenant_id,task_id,name,required,file_id,note,created_by,updated_by) VALUES($1,$2,$3,$4,$5,$6,$7,$7) RETURNING id,version`, tenant, taskID, in.Name, in.Required, in.FileID, in.Note, actor).Scan(&in.ID, &in.Version)
			if err != nil {
				return err
			}
		} else {
			err := tx.QueryRowContext(ctx, `UPDATE task_materials SET name=$4,required=$5,file_id=$6,note=$7,updated_by=$8,updated_at=now(),version=version+1 WHERE tenant_id=$1 AND task_id=$2 AND id=$3 AND version=$9 RETURNING version`, tenant, taskID, in.ID, in.Name, in.Required, in.FileID, in.Note, actor, in.Version).Scan(&in.Version)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrFileProcessingConflict
			}
			if err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO task_material_events(tenant_id,task_id,material_id,snapshot,created_by)
			SELECT tenant_id,task_id,id,to_jsonb(m),$4 FROM task_materials m WHERE tenant_id=$1 AND task_id=$2 AND id=$3`, tenant, taskID, in.ID, actor)
		return err
	})
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return nil, fmt.Errorf("%w: material name already exists", ErrFileProcessingConflict)
		}
		return nil, err
	}
	rows, err := d.ListTaskMaterials(ctx, tenant, taskID)
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		if m.ID == in.ID {
			return m, nil
		}
	}
	return nil, ErrNotFound
}
