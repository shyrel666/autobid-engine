package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

func (r RetryRule) Or(f RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = f.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = f.IntervalMS
	}
	return r
}
func (r RetryRule) Validate() error {
	if r.Attempts < -1 || r.Attempts > 20 || r.IntervalMS < 0 || r.IntervalMS > 3600000 {
		return errors.New("retry attempts must be -1..20; interval_ms must be 0..3600000")
	}
	return nil
}

type RetryOverride struct {
	Connect RetryRule `json:"connect"`
	Empty   RetryRule `json:"empty"`
	Stream  RetryRule `json:"stream"`
}
type LLMRetryPolicy struct {
	RetryOverride
	Breaker RetryRule `json:"breaker"`
	Intent  RetryRule `json:"intent"`
}

func (p LLMRetryPolicy) Validate() error {
	for _, r := range []RetryRule{p.Connect, p.Empty, p.Stream, p.Breaker, p.Intent} {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// LLMOptions are persisted as JSONB; embedding keeps the HTTP profile flat.
type LLMOptions struct {
	ThinkingType   string        `json:"thinking_type"`
	Streaming      *bool         `json:"streaming,omitempty"`
	MaxTokensField string        `json:"max_tokens_field"`
	RatePerSecond  float64       `json:"rate_per_second"`
	RatePerMinute  float64       `json:"rate_per_minute"`
	Retry          RetryOverride `json:"retry"`
}

func (p LLMOptions) Streams() bool { return p.Streaming == nil || *p.Streaming }

type LLMSettings struct {
	PoolEnabled  bool           `json:"pool_enabled"`
	BindFallback bool           `json:"bind_fallback"`
	RecordBodies bool           `json:"record_bodies"`
	Retry        LLMRetryPolicy `json:"retry"`
}

func (d *DB) LLMSettings(ctx context.Context) (LLMSettings, error) {
	var s LLMSettings
	var raw []byte
	err := d.QueryRowContext(ctx, `SELECT pool_enabled, bind_fallback, record_bodies, retry_policy FROM llm_settings WHERE tenant_id='default'`).Scan(&s.PoolEnabled, &s.BindFallback, &s.RecordBodies, &raw)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s.Retry)
	return s, err
}
func (d *DB) SaveLLMSettings(ctx context.Context, s LLMSettings, actor string) error {
	if err := s.Retry.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s.Retry)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `UPDATE llm_settings SET pool_enabled=$1,bind_fallback=$2,record_bodies=$3,retry_policy=$4,updated_by=$5,updated_at=now() WHERE tenant_id='default'`, s.PoolEnabled, s.BindFallback, s.RecordBodies, raw, actor)
	return err
}
func (d *DB) ActivateLLMProfile(ctx context.Context, id int64, actor string) error {
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT tenant_id FROM llm_settings WHERE tenant_id='default' FOR UPDATE`); err != nil {
			return err
		}
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT enabled FROM llm_profiles WHERE id=$1 AND tenant_id='default' FOR UPDATE`, id).Scan(&enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !enabled {
			return errors.New("activate an enabled profile")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE llm_profiles SET is_default=false WHERE tenant_id='default' AND is_default`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE llm_profiles SET is_default=true,updated_by=$2 WHERE id=$1`, id, actor)
		return err
	})
}
func (d *DB) AgentLLMBindings(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{"planner": 0, "worker": 0, "judge": 0, "goals": 0, "watchdog": 0}
	rows, err := d.QueryContext(ctx, `SELECT agent_key,profile_id FROM agent_llm_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

func (p *LLMProfile) Validate() error {
	if p.Name == "" || p.Model == "" {
		return errors.New("name and model are required")
	}
	if p.Format != "anthropic" && p.Format != "openai" && p.Format != "openai-responses" {
		return errors.New("format must be anthropic, openai or openai-responses")
	}
	if p.MaxTokens < 0 || p.WindowTokens < 0 || p.WindowTokens > 1000000 || p.RatePerSecond < 0 || p.RatePerMinute < 0 || p.RatePerSecond > 100000 || p.RatePerMinute > 100000 {
		return errors.New("invalid token window or request rate")
	}
	if p.WindowTokens > 0 && p.WindowTokens < 32000 {
		return errors.New("context window must be 0 or 32000..1000000 tokens")
	}
	if p.ThinkingType != "" && p.ThinkingType != "enabled" && p.ThinkingType != "disabled" {
		return errors.New("invalid thinking_type")
	}
	switch p.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("invalid effort")
	}
	switch p.ThinkingDisplay {
	case "", "summarized", "omitted":
	default:
		return errors.New("invalid thinking_display")
	}
	if p.MaxTokensField != "" && (p.Format != "openai" || p.MaxTokensField != "max_completion_tokens") {
		return errors.New("max_tokens_field is only supported by openai")
	}
	for _, r := range []RetryRule{p.Retry.Connect, p.Retry.Empty, p.Retry.Stream} {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
	}
	return nil
}
