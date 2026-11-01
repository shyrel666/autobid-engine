package db

// F1：任务原件登记（task_file_objects）。物理对象按租户内容寻址，行按任务登记；招标原件不得写入企业证据表。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskFileObject 是任务内一个内容寻址文件对象。RelativePath 由服务端生成，不接受外部输入。
type TaskFileObject struct {
	ID            int64     `json:"id"`
	TenantID      string    `json:"tenant_id"`
	TaskID        int64     `json:"task_id"`
	Kind          string    `json:"kind"`
	SHA256        string    `json:"sha256"`
	ByteSize      int64     `json:"byte_size"`
	RelativePath  string    `json:"-"`
	OriginalName  string    `json:"original_name,omitempty"`
	MediaType     string    `json:"media_type,omitempty"`
	ArchivePath   string    `json:"archive_path,omitempty"`
	Format        string    `json:"format,omitempty"`
	ProcessStatus string    `json:"process_status"`
	SourceJobID   *int64    `json:"source_job_id,omitempty"`
	ParentFileID  *int64    `json:"parent_file_id,omitempty"`
	Availability  string    `json:"availability"`
	CreatedBy     string    `json:"created_by,omitempty"`
	UpdatedBy     string    `json:"updated_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Task file object kinds.
const (
	FileKindOriginal     = "original"
	FileKindScript       = "script"
	FileKindDerived      = "derived"
	FileKindTextSnapshot = "text_snapshot"
)

const taskFileObjectCols = `id, tenant_id, task_id, kind, sha256, byte_size, relative_path, original_name, media_type,
	archive_path, format, process_status, source_job_id, parent_file_id, availability, created_by, updated_by, created_at, updated_at`

func validFileKind(k string) bool {
	switch k {
	case FileKindOriginal, FileKindScript, FileKindDerived, FileKindTextSnapshot:
		return true
	}
	return false
}

func scanTaskFileObject(r rowScanner) (*TaskFileObject, error) {
	var o TaskFileObject
	if err := r.Scan(&o.ID, &o.TenantID, &o.TaskID, &o.Kind, &o.SHA256, &o.ByteSize, &o.RelativePath, &o.OriginalName,
		&o.MediaType, &o.ArchivePath, &o.Format, &o.ProcessStatus, &o.SourceJobID, &o.ParentFileID, &o.Availability,
		&o.CreatedBy, &o.UpdatedBy, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

// InsertTaskFileObject 登记一个已落盘的对象；同租户同任务同哈希同包内路径幂等返回已有行。
func (d *DB) InsertTaskFileObject(ctx context.Context, o *TaskFileObject) (*TaskFileObject, bool, error) {
	if o == nil {
		return nil, false, errors.New("db: nil file object")
	}
	if o.TaskID <= 0 || !validFileKind(o.Kind) || len(o.SHA256) != 64 || o.RelativePath == "" {
		return nil, false, fmt.Errorf("db: invalid file object (task_id/kind/sha256/relative_path)")
	}
	tenant := tenantOr(o.TenantID)
	o.TenantID = tenant
	if strings.TrimSpace(o.Availability) == "" {
		o.Availability = "available"
	}
	if strings.TrimSpace(o.ProcessStatus) == "" {
		o.ProcessStatus = "listed"
	}
	created := false
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			INSERT INTO task_file_objects(tenant_id, task_id, kind, sha256, byte_size, relative_path, original_name,
				media_type, archive_path, format, process_status, source_job_id, parent_file_id, availability, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15)
			ON CONFLICT (tenant_id, task_id, sha256, archive_path) DO UPDATE
				SET updated_by = EXCLUDED.updated_by
			RETURNING `+taskFileObjectCols+`, (xmax = 0)`,
			tenant, o.TaskID, o.Kind, o.SHA256, o.ByteSize, o.RelativePath, o.OriginalName, o.MediaType, o.ArchivePath,
			o.Format, o.ProcessStatus, o.SourceJobID, o.ParentFileID, o.Availability, o.CreatedBy).Scan(&o.ID, &o.TenantID,
			&o.TaskID, &o.Kind, &o.SHA256, &o.ByteSize, &o.RelativePath, &o.OriginalName, &o.MediaType, &o.ArchivePath,
			&o.Format, &o.ProcessStatus, &o.SourceJobID, &o.ParentFileID, &o.Availability, &o.CreatedBy, &o.UpdatedBy,
			&o.CreatedAt, &o.UpdatedAt, &created)
	})
	if err != nil {
		return nil, false, err
	}
	saved, _, err := d.GetTaskFileObject(ctx, tenant, o.TaskID, o.ID)
	return saved, created, err
}

// GetTaskFileObject 在租户 + 任务范围内读取对象；跨任务 ID 返回 ErrNotFound。
func (d *DB) GetTaskFileObject(ctx context.Context, tenant string, taskID, id int64) (*TaskFileObject, bool, error) {
	o, err := scanTaskFileObject(d.QueryRowContext(ctx, `SELECT `+taskFileObjectCols+` FROM task_file_objects
		WHERE tenant_id = $1 AND task_id = $2 AND id = $3`, tenantOr(tenant), taskID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: file object %d", ErrNotFound, id)
	}
	return o, err == nil, err
}

// ListTaskFileObjects 按登记顺序列出任务对象（可选 kind 过滤）。
func (d *DB) ListTaskFileObjects(ctx context.Context, tenant string, taskID int64, kind string, limit int) ([]*TaskFileObject, error) {
	return d.ListTaskFileObjectsAfter(ctx, tenant, taskID, kind, 0, limit)
}

func (d *DB) ListTaskFileObjectsAfter(ctx context.Context, tenant string, taskID int64, kind string, after int64, limit int) ([]*TaskFileObject, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.QueryContext(ctx, `SELECT `+taskFileObjectCols+` FROM task_file_objects
		WHERE tenant_id = $1 AND task_id = $2 AND ($3 = '' OR kind = $3) AND id > $5 ORDER BY id LIMIT $4`,
		tenantOr(tenant), taskID, kind, limit, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TaskFileObject
	for rows.Next() {
		o, err := scanTaskFileObject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ReferencedFilePaths 返回租户内所有登记行引用的物理相对路径（GC 只清理不在集合中的对象）。
func (d *DB) ReferencedFilePaths(ctx context.Context, tenant string) (map[string]bool, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT relative_path FROM task_file_objects WHERE tenant_id = $1`, tenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// MarkTaskFileObjectMissing 标记对象不可用（保留登记与审计）。
func (d *DB) MarkTaskFileObjectMissing(ctx context.Context, tenant string, taskID, id int64, actor string) error {
	return d.Transaction(ctx, func(tx *DB) error {
		if err := tx.LockFileProcessingTask(ctx, tenant, taskID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE task_file_objects SET availability = 'missing', updated_by = $4
		WHERE tenant_id = $1 AND task_id = $2 AND id = $3 AND availability <> 'missing'`, tenantOr(tenant), taskID, id, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: file object %d", ErrNotFound, id)
		}
		return nil
	})
}
