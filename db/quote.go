package db

// D2 最小报价门禁：行金额 = 单价 × 数量、声明总额 = 行合计（容差 0.01）、声明总额 ≤ 最高限价，
// 以及报价与价格类承诺一致。金额以 0.0001 为最小单位的定点整数计算，禁止二进制浮点累加。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

var ErrQuoteInconsistent = errors.New("d2: bid quote is inconsistent")

const quoteScale int64 = 10000

// BidQuoteValidationCheck 是一条报价校验项。
type BidQuoteValidationCheck struct {
	Rule   string `json:"rule"`
	Status string `json:"status"` // pass | fail
	Detail string `json:"detail,omitempty"`
}

// BidQuoteValidation 是报价逐项检查结果。
type BidQuoteValidation struct {
	QuoteID  int64                      `json:"quote_id,omitempty"`
	Lot      string                     `json:"lot"`
	Version  int                        `json:"version"`
	Total    string                     `json:"declared_total"`
	Checks   []BidQuoteValidationCheck  `json:"checks"`
	OK       bool                       `json:"ok"`
}

// BidQuoteItem 是报价行。
type BidQuoteItem struct {
	ID            int64  `json:"id"`
	TenantID      string `json:"tenant_id"`
	QuoteID       int64  `json:"quote_id"`
	LineNo        int    `json:"line_no"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	UnitPrice     string `json:"unit_price"`
	Quantity      string `json:"quantity"`
	LineAmount    string `json:"line_amount"`
	RequirementID *int64 `json:"requirement_item_id,omitempty"`
	Model         string `json:"model,omitempty"`
}

// BidQuote 是一次报价版本。
type BidQuote struct {
	ID             int64          `json:"id"`
	TenantID       string         `json:"tenant_id"`
	TaskID         int64          `json:"task_id"`
	Lot            string         `json:"lot"`
	Version        int            `json:"version"`
	DeclaredTotal  string         `json:"declared_total"`
	Ceiling        *string        `json:"ceiling,omitempty"`
	Currency       string         `json:"currency"`
	Status         string         `json:"status"`
	OriginalPath   string         `json:"original_path,omitempty"`
	OriginalSHA256 string         `json:"original_sha256,omitempty"`
	Note           string         `json:"note,omitempty"`
	ConfirmedBy    string         `json:"confirmed_by,omitempty"`
	ConfirmedAt    *time.Time     `json:"confirmed_at,omitempty"`
	CreatedBy      string         `json:"created_by,omitempty"`
	UpdatedBy      string         `json:"updated_by,omitempty"`
	ApprovedBy     string         `json:"approved_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Items          []*BidQuoteItem `json:"items,omitempty"`
}

// BidQuoteItemInput 是写入报价行的金额字符串。
type BidQuoteItemInput struct {
	LineNo        int    `json:"line_no"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	UnitPrice     string `json:"unit_price"`
	Quantity      string `json:"quantity"`
	LineAmount    string `json:"line_amount"`
	RequirementID *int64 `json:"requirement_item_id"`
	Model         string `json:"model"`
}

// BidQuoteInput 是一次报价版本输入。
type BidQuoteInput struct {
	Lot            string             `json:"lot"`
	DeclaredTotal  string             `json:"declared_total"`
	Ceiling        *string            `json:"ceiling"`
	Currency       string             `json:"currency"`
	OriginalPath   string             `json:"original_path"`
	OriginalSHA256 string             `json:"original_sha256"`
	Note           string             `json:"note"`
	Items          []BidQuoteItemInput `json:"items"`
}

const quoteCols = `q.id, q.tenant_id, q.task_id, q.lot, q.version, q.declared_total::text, CASE WHEN q.ceiling IS NULL THEN NULL ELSE q.ceiling::text END,
	q.currency, q.status, q.original_path, q.original_sha256, q.note, q.confirmed_by, q.confirmed_at, q.created_by, q.updated_by,
	q.approved_by, q.created_at, q.updated_at`

func scanQuote(r rowScanner) (*BidQuote, error) {
	var (
		q          BidQuote
		ceiling    sql.NullString
		confirmed  sql.NullTime
	)
	err := r.Scan(&q.ID, &q.TenantID, &q.TaskID, &q.Lot, &q.Version, &q.DeclaredTotal, &ceiling, &q.Currency, &q.Status,
		&q.OriginalPath, &q.OriginalSHA256, &q.Note, &q.ConfirmedBy, &confirmed, &q.CreatedBy, &q.UpdatedBy, &q.ApprovedBy,
		&q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if ceiling.Valid {
		q.Ceiling = &ceiling.String
	}
	q.ConfirmedAt = nullTimePtr(confirmed)
	return &q, nil
}

func scanQuoteItem(r rowScanner) (*BidQuoteItem, error) {
	var (
		it  BidQuoteItem
		req sql.NullInt64
	)
	err := r.Scan(&it.ID, &it.TenantID, &it.QuoteID, &it.LineNo, &it.Code, &it.Name, &it.UnitPrice, &it.Quantity, &it.LineAmount,
		&req, &it.Model)
	if err != nil {
		return nil, err
	}
	if req.Valid {
		v := req.Int64
		it.RequirementID = &v
	}
	return &it, nil
}

// parseAmount 把十进制金额解析为 0.0001 定点整数；拒绝负数和超过 4 位小数。
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	if s == "" {
		return 0, errors.New("amount is empty")
	}
	if strings.HasPrefix(s, "-") {
		return 0, errors.New("negative amount")
	}
	parts := strings.SplitN(s, ".", 2)
	whole := parts[0]
	if whole == "" {
		whole = "0"
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 4 {
		return 0, fmt.Errorf("amount %q has more than 4 decimal places", s)
	}
	for len(frac) < 4 {
		frac += "0"
	}
	w, ok := new(big.Int).SetString(whole, 10)
	if !ok {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	f, ok := new(big.Int).SetString(frac, 10)
	if !ok {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	v := new(big.Int).Mul(w, big.NewInt(quoteScale))
	v.Add(v, f)
	if !v.IsInt64() {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	return v.Int64(), nil
}

func formatAmount(v int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%04d", sign, v/quoteScale, v%quoteScale)
}

func mulAmount(a, b int64) (int64, error) {
	prod := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	q, r := new(big.Int).QuoRem(prod, big.NewInt(quoteScale), new(big.Int))
	if r.Int64()*2 >= quoteScale {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("line amount out of range")
	}
	return q.Int64(), nil
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ValidateBidQuote 校验一个报价版本；不存在返回 ErrNotFound。
func (d *DB) ValidateBidQuote(ctx context.Context, tenant string, taskID, quoteID int64) (*BidQuoteValidation, error) {
	return validateBidQuoteQ(ctx, d, tenant, taskID, quoteID)
}

func validateBidQuoteQ(ctx context.Context, dbq queryer, tenant string, taskID, quoteID int64) (*BidQuoteValidation, error) {
	tenant = tenantOr(tenant)
	q, err := scanQuote(dbq.QueryRowContext(ctx, `SELECT `+quoteCols+` FROM bid_quotes q WHERE q.tenant_id=$1 AND q.task_id=$2 AND q.id=$3`,
		tenant, taskID, quoteID))
	if err != nil {
		return nil, err
	}
	rows, err := dbq.QueryContext(ctx, `SELECT id, tenant_id, quote_id, line_no, code, name, unit_price::text, quantity::text,
		line_amount::text, requirement_id, model FROM bid_quote_items WHERE tenant_id=$1 AND quote_id=$2 ORDER BY line_no`, tenant, quoteID)
	if err != nil {
		return nil, err
	}
	var items []*BidQuoteItem
	for rows.Next() {
		it, err := scanQuoteItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	declared, err := parseAmount(q.DeclaredTotal)
	if err != nil {
		return nil, fmt.Errorf("%w: declared_total: %v", ErrQuoteInconsistent, err)
	}
	out := &BidQuoteValidation{QuoteID: q.ID, Lot: q.Lot, Version: q.Version, Total: formatAmount(declared), Checks: []BidQuoteValidationCheck{}}
	pass := func(rule, detail string) {
		out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: rule, Status: "pass", Detail: detail})
	}
	fail := func(rule, detail string) {
		out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: rule, Status: "fail", Detail: detail})
	}
	var sum int64
	for _, it := range items {
		price, err := parseAmount(it.UnitPrice)
		if err != nil {
			fail(fmt.Sprintf("line_%d", it.LineNo), "unit_price: "+err.Error())
			continue
		}
		qty, err := parseAmount(it.Quantity)
		if err != nil {
			fail(fmt.Sprintf("line_%d", it.LineNo), "quantity: "+err.Error())
			continue
		}
		line, err := parseAmount(it.LineAmount)
		if err != nil {
			fail(fmt.Sprintf("line_%d", it.LineNo), "line_amount: "+err.Error())
			continue
		}
		calc, err := mulAmount(price, qty)
		if err != nil {
			fail(fmt.Sprintf("line_%d", it.LineNo), err.Error())
			continue
		}
		if calc != line {
			fail(fmt.Sprintf("line_%d", it.LineNo), fmt.Sprintf("line_amount=%s but unit_price*quantity=%s", formatAmount(line), formatAmount(calc)))
		} else {
			pass(fmt.Sprintf("line_%d", it.LineNo), formatAmount(line))
		}
		sum += line
	}
	if absInt64(sum-declared) > 100 {
		fail("declared_total", fmt.Sprintf("declared_total=%s but sum(line_amount)=%s (tolerance 0.01)", formatAmount(declared), formatAmount(sum)))
	} else {
		pass("declared_total", formatAmount(declared))
	}
	if q.Ceiling != nil {
		ceiling, err := parseAmount(*q.Ceiling)
		if err != nil {
			fail("ceiling", "ceiling: "+err.Error())
		} else if declared > ceiling {
			fail("ceiling", fmt.Sprintf("declared_total=%s exceeds ceiling=%s", formatAmount(declared), formatAmount(ceiling)))
		} else {
			pass("ceiling", formatAmount(ceiling))
		}
	}
	if err := checkPriceCommitmentsQ(ctx, dbq, tenant, taskID, declared, out); err != nil {
		return nil, err
	}
	out.OK = true
	for _, c := range out.Checks {
		if c.Status == "fail" {
			out.OK = false
		}
	}
	return out, nil
}

func checkPriceCommitmentsQ(ctx context.Context, dbq queryer, tenant string, taskID int64, declared int64, out *BidQuoteValidation) error {
	rows, err := dbq.QueryContext(ctx, `SELECT code, committed_value::float8, status FROM commitment_ledger
		WHERE tenant_id=$1 AND task_id=$2 AND metric='price' ORDER BY code`, tenant, taskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var code, status string
		var value sql.NullFloat64
		if err := rows.Scan(&code, &value, &status); err != nil {
			return err
		}
		if status != CommitmentConfirmed && status != CommitmentSignedOff {
			out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: "price_commitment", Status: "fail",
				Detail: fmt.Sprintf("%s status=%s; price commitment must be confirmed", code, status)})
			continue
		}
		if !value.Valid {
			out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: "price_commitment", Status: "fail", Detail: code + " has no committed value"})
			continue
		}
		if absInt64(int64(value.Float64*float64(quoteScale)+0.5)-declared) > 100 {
			out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: "price_commitment", Status: "fail",
				Detail: fmt.Sprintf("%s committed=%g but quote declared_total=%s", code, value.Float64, formatAmount(declared))})
		} else {
			out.Checks = append(out.Checks, BidQuoteValidationCheck{Rule: "price_commitment", Status: "pass", Detail: code})
		}
	}
	return rows.Err()
}

// CreateBidQuote 新建一个 draft 报价版本（版本号自动递增）。
func (d *DB) CreateBidQuote(ctx context.Context, tenant string, taskID int64, in BidQuoteInput, actor string) (*BidQuote, error) {
	tenant = tenantOr(tenant)
	if strings.TrimSpace(in.DeclaredTotal) == "" {
		return nil, errors.New("d2: declared_total is required")
	}
	declared, err := parseAmount(in.DeclaredTotal)
	if err != nil {
		return nil, err
	}
	var ceiling any
	if in.Ceiling != nil && strings.TrimSpace(*in.Ceiling) != "" {
		c, err := parseAmount(*in.Ceiling)
		if err != nil {
			return nil, err
		}
		ceiling = formatAmount(c)
	}
	currency := strings.TrimSpace(in.Currency)
	if currency == "" {
		currency = "CNY"
	}
	var out *BidQuote
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM bid_quotes WHERE tenant_id=$1 AND task_id=$2 AND lot=$3`,
			tenant, taskID, strings.TrimSpace(in.Lot)).Scan(&version); err != nil {
			return err
		}
		q, err := scanQuote(tx.QueryRowContext(ctx, `WITH ins AS (
			INSERT INTO bid_quotes(tenant_id, task_id, lot, version, declared_total, ceiling, currency,
				status, original_path, original_sha256, note, created_by, updated_by)
			VALUES ($1,$2,$3,$4,$5::numeric,$6::numeric,$7,'draft',$8,$9,$10,COALESCE(NULLIF($11,''),''),COALESCE(NULLIF($11,''),''))
			RETURNING *) SELECT `+quoteCols+` FROM ins q`,
			tenant, taskID, strings.TrimSpace(in.Lot), version, formatAmount(declared), ceiling, currency, in.OriginalPath,
			in.OriginalSHA256, strings.TrimSpace(in.Note), actor))
		if err != nil {
			return err
		}
		for _, it := range in.Items {
			price, err := parseAmount(it.UnitPrice)
			if err != nil {
				return fmt.Errorf("line %d unit_price: %w", it.LineNo, err)
			}
			qty, err := parseAmount(it.Quantity)
			if err != nil {
				return fmt.Errorf("line %d quantity: %w", it.LineNo, err)
			}
			line, err := parseAmount(it.LineAmount)
			if err != nil {
				return fmt.Errorf("line %d line_amount: %w", it.LineNo, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO bid_quote_items(tenant_id, quote_id, line_no, code, name, unit_price, quantity,
				line_amount, requirement_id, model, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9,$10,COALESCE(NULLIF($11,''),''),COALESCE(NULLIF($11,''),''))`,
				tenant, q.ID, it.LineNo, it.Code, it.Name, formatAmount(price), formatAmount(qty), formatAmount(line), it.RequirementID,
				it.Model, actor); err != nil {
				return err
			}
		}
		out = q
		return nil
	})
	return out, err
}

// ConfirmBidQuote 重新校验并确认报价；不一致返回 ErrQuoteInconsistent，旧 confirmed 版本失去导出资格。
func (d *DB) ConfirmBidQuote(ctx context.Context, tenant string, taskID, quoteID int64, actor string) (*BidQuote, *BidQuoteValidation, error) {
	tenant = tenantOr(tenant)
	val, err := d.ValidateBidQuote(ctx, tenant, taskID, quoteID)
	if err != nil {
		return nil, nil, err
	}
	if !val.OK {
		return nil, val, fmt.Errorf("%w: quote %d has failed checks", ErrQuoteInconsistent, quoteID)
	}
	var out *BidQuote
	err = d.InTx(ctx, func(tx *sql.Tx) error {
		q, err := scanQuote(tx.QueryRowContext(ctx, `SELECT `+quoteCols+` FROM bid_quotes q
			WHERE q.tenant_id=$1 AND q.task_id=$2 AND q.id=$3 FOR UPDATE`, tenant, taskID, quoteID))
		if err != nil {
			return err
		}
		if q.Status == "rejected" {
			return fmt.Errorf("%w: quote %d is rejected", ErrQuoteInconsistent, quoteID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bid_quotes SET status='rejected', note=note || ' superseded by v' || $4, updated_by=$5
			WHERE tenant_id=$1 AND task_id=$2 AND lot=$3 AND status='confirmed' AND id<>$6`,
			tenant, taskID, q.Lot, strconv.Itoa(q.Version+1), actor, quoteID); err != nil {
			return err
		}
		out, err = scanQuote(tx.QueryRowContext(ctx, `WITH upd AS (
			UPDATE bid_quotes SET status='confirmed', confirmed_by=COALESCE(NULLIF($4::text,''),''),
				confirmed_at=now(), approved_by=COALESCE(NULLIF($4::text,''),''), updated_by=COALESCE(NULLIF($4::text,''),'')
			WHERE tenant_id=$1 AND task_id=$2 AND id=$3 RETURNING *)
			SELECT `+quoteCols+` FROM upd q`, tenant, taskID, quoteID, actor))
		return err
	})
	if err != nil {
		return nil, val, err
	}
	return out, val, nil
}

// ListBidQuotes 列出任务报价版本（含行）。
func (d *DB) ListBidQuotes(ctx context.Context, tenant string, taskID int64) ([]*BidQuote, error) {
	tenant = tenantOr(tenant)
	rows, err := d.QueryContext(ctx, `SELECT `+quoteCols+` FROM bid_quotes q WHERE q.tenant_id=$1 AND q.task_id=$2 ORDER BY lot, version DESC`,
		tenant, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BidQuote
	for rows.Next() {
		q, err := scanQuote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, q := range out {
		items, err := d.ListBidQuoteItems(ctx, tenant, q.ID)
		if err != nil {
			return nil, err
		}
		q.Items = items
	}
	return out, nil
}

// ListBidQuoteItems 列出报价行。
func (d *DB) ListBidQuoteItems(ctx context.Context, tenant string, quoteID int64) ([]*BidQuoteItem, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, tenant_id, quote_id, line_no, code, name, unit_price::text, quantity::text,
		line_amount::text, requirement_id, model FROM bid_quote_items WHERE tenant_id=$1 AND quote_id=$2 ORDER BY line_no`,
		tenantOr(tenant), quoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BidQuoteItem
	for rows.Next() {
		it, err := scanQuoteItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
