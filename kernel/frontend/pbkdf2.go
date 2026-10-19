// Package frontend 是 R23 前台服务：面向人的登录 UI + 个人记忆工作台 + 社区浏览。
// 独立部署（cmd/lightmem-frontend），只通过 REST API / Authenticator 对接后台，
// 不改后台 Schema（红线）。凭据与会话存前台自有库。
package frontend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// pbkdf2SHA256 是 PBKDF2-HMAC-SHA256 的 stdlib 实现（RFC 2898）。
// go.mod 红线禁止新增第三方依赖（x/crypto 不在 require 中），
// 故用 stdlib 手写；该实现与 RFC 2898 完全一致，可与任何标准库互通。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, numBlocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, hLen)
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := 0; j < hLen; j++ {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}
