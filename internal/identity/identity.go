// Package identity 提供口令哈希（argon2id）、会话令牌与防跨站请求伪造令牌的纯函数。
//
// 明文口令与会话令牌只在内存中短暂存在：存储层只保存 argon2id 编码串与令牌的 SHA-256，
// 本包的错误信息从不包含口令或令牌。
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数（RFC 9106 第二推荐配置的内存档：64 MiB、3 轮、单线程以外取 2 并行）。
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLen / MaxPasswordLen 是口令长度（字符数）范围。
const (
	MinPasswordLen = 10
	MaxPasswordLen = 256
)

var (
	ErrWeakPassword  = errors.New("identity: password must be 10-256 characters and not be the login name")
	ErrBadHash       = errors.New("identity: unrecognised password hash")
	ErrPasswordWrong = errors.New("identity: password does not match")
)

// CheckPassword 校验口令强度：长度与"不等于登录名"。不做字符类别要求（长口令比复杂规则更有效）。
func CheckPassword(login, password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLen || n > MaxPasswordLen {
		return ErrWeakPassword
	}
	if strings.EqualFold(strings.TrimSpace(password), strings.TrimSpace(login)) {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword 返回 PHC 格式的 argon2id 编码串：$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword 以常量时间比较口令与编码串；参数取自编码串本身，便于以后调高参数而不影响旧哈希。
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return ErrBadHash
	}
	var mem, iters uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iters, &threads); err != nil || mem == 0 || iters == 0 || threads == 0 ||
		mem > 1<<20 || iters > 16 {
		return ErrBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return ErrBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, iters, mem, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordWrong
	}
	return nil
}

// dummyHash 用于账号不存在时仍执行一次同等代价的校验，避免以响应时间探测账号是否存在。
var dummyHash, _ = HashPassword("autobid-dummy-password-for-timing")

// BurnVerify 执行一次与真实校验同等代价的无效校验。
func BurnVerify(password string) { _ = VerifyPassword(dummyHash, password) }

// NewSessionToken 生成 32 字节随机会话令牌（base64url，无填充），并返回其 SHA-256。
func NewSessionToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken 返回会话令牌的 SHA-256（存储层只保存它）。
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CSRFToken 由会话令牌派生防跨站请求伪造令牌：HMAC-SHA256(token, "autobid-csrf/v1")。
// 服务端每次按 Cookie 里的会话令牌重算，不落库；页面从 /api/auth/session 取得它，放进 X-Autobid-CSRF 请求头。
func CSRFToken(sessionToken string) string {
	m := hmac.New(sha256.New, []byte(sessionToken))
	m.Write([]byte("autobid-csrf/v1"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// CheckCSRF 以常量时间比较请求头里的令牌与会话派生值。
func CheckCSRF(sessionToken, header string) bool {
	if sessionToken == "" || header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(CSRFToken(sessionToken)), []byte(header)) == 1
}
