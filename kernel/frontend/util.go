package frontend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// newID 生成 32 位十六进制随机 ID（无 "-" 前缀，用于 session_log.id 等）。
func newID() string {
	b := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

// newSecret 生成 base64url 的随机机密（CSRF token、cookie 签名等）。
func newSecret() string {
	b := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

// sha256hex 计算 SHA-256 十六进制（用于 cookie 值哈希、CSRF 校验）。
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// writeJSON 输出 JSON 响应。
func writeJSON(w io.Writer, status int, v any) {
	_ = json.NewEncoder(w).Encode(v)
}