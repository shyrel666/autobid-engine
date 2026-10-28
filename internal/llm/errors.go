package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APIError 是 Provider 适配层统一的 HTTP 错误表示；无状态码（StatusCode=0）表示传输层失败。
type APIError struct {
	Provider   string
	StatusCode int
	Type       string // 上游错误类型，如 rate_limit_error / overloaded_error / billing_error
	Message    string
	RequestID  string
	RetryAfter time.Duration
	Err        error
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s: status %d", e.Provider, e.StatusCode)
	if e.Type != "" {
		s += " " + e.Type
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

func (e *APIError) Unwrap() error { return e.Err }

func asAPIError(err error) (*APIError, bool) {
	var ae *APIError
	ok := errors.As(err, &ae)
	return ae, ok
}

func isCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

var quotaMarkers = []string{
	"quota", "credit", "billing", "payment", "insufficient_quota", "insufficient balance",
	"余额", "欠费", "额度",
}

// IsQuotaExhausted 是严格的配额耗尽分类器（§5.4）：普通 429、认证失败、网络错误或 5xx 都不算，
// 只有 402 / billing_error，或错误正文明确指向 quota/credits/billing/payment 才推进任务 Profile 链。
func IsQuotaExhausted(err error) bool {
	ae, ok := asAPIError(err)
	if !ok {
		return false
	}
	if ae.StatusCode == 402 || ae.Type == "billing_error" {
		return true
	}
	if ae.StatusCode == 401 || ae.StatusCode >= 500 || ae.StatusCode == 0 {
		return false
	}
	msg := strings.ToLower(ae.Message + " " + ae.Type)
	for _, m := range quotaMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// IsContextTooLong 报告请求是否因上下文过长被拒绝。
func IsContextTooLong(err error) bool {
	ae, ok := asAPIError(err)
	if !ok {
		return false
	}
	if ae.StatusCode == 413 {
		return true
	}
	msg := strings.ToLower(ae.Message)
	return ae.StatusCode == 400 && (strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "context window") || strings.Contains(msg, "context length"))
}

// IsRetryable 判定同 Provider 提交前重试是否有意义（§5.3）：
// 配额耗尽、上下文过长、400/401/403/404/422 与取消不重试，其余（429、5xx、529 过载、传输错误）视为瞬态。
func IsRetryable(err error) bool {
	if err == nil || isCancel(err) || IsQuotaExhausted(err) || IsContextTooLong(err) {
		return false
	}
	ae, ok := asAPIError(err)
	if !ok {
		return true // 未分类的传输/解析错误按瞬态处理
	}
	switch ae.StatusCode {
	case 400, 401, 403, 404, 422:
		return false
	}
	return true
}

// ShouldFailover 判定 llmpool 是否可换下一个成员（§5.6）：取消与 400 不切；
// 401/402/403/404/408/429、5xx 与无状态码的传输错误可切。
func ShouldFailover(err error) bool {
	if err == nil || isCancel(err) {
		return false
	}
	ae, ok := asAPIError(err)
	if !ok {
		return true
	}
	switch {
	case ae.StatusCode == 0:
		return true
	case ae.StatusCode == 401, ae.StatusCode == 402, ae.StatusCode == 403, ae.StatusCode == 404,
		ae.StatusCode == 408, ae.StatusCode == 429, ae.StatusCode >= 500:
		return true
	}
	return false
}

// IsHardFailure 表示配置/权限类故障（401/402/403/404），llmpool 首次即熔断。
func IsHardFailure(err error) bool {
	ae, ok := asAPIError(err)
	if !ok {
		return false
	}
	switch ae.StatusCode {
	case 401, 402, 403, 404:
		return true
	}
	return false
}
