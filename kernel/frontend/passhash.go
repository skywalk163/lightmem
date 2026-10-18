package frontend

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// passwordHashIter 是 PBKDF2 迭代次数（OWASP 2023 推荐值 ≥210000）。
// 用包级变量而非常量，便于测试用低迭代加速。
var passwordHashIter = 210000

const (
	pbkdf2SaltLen = 16
	pbkdf2KeyLen  = 32
	pbkdf2Prefix  = "pbkdf2-sha256"
)

// hashPassword 用 PBKDF2-HMAC-SHA256 派生密码哈希，返回可自描述存储串：
//
//	pbkdf2-sha256$<iter>$<salt-hex>$<hash-hex>
//
// iter 传 0 时使用默认 passwordHashIter。
func hashPassword(password string, iter int) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	if iter <= 0 {
		iter = passwordHashIter
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	dk := pbkdf2SHA256([]byte(password), salt, iter, pbkdf2KeyLen)
	return fmt.Sprintf("%s$%d$%x$%x", pbkdf2Prefix, iter, salt, dk), nil
}

// verifyPassword 常数时间校验密码与存储串是否匹配。
// 任一解析失败（非本格式 / 非法 hex / 异常迭代）一律返回 false，不区分错误。
func verifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	dk := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(dk, want) == 1
}