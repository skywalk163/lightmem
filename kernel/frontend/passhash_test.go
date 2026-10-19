package frontend

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestPBKDF2SHA256 用 PBKDF2-HMAC-SHA256 公开测试向量校验实现正确性。
func TestPBKDF2SHA256(t *testing.T) {
	cases := []struct {
		password string
		salt     string
		iter     int
		dkLen    int
		want     string // hex
	}{
		{"password", "salt", 1, 32, "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32, "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32, "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40, "348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := pbkdf2SHA256([]byte(c.password), []byte(c.salt), c.iter, c.dkLen)
		if hex.EncodeToString(got) != c.want {
			t.Errorf("pbkdf2(pw=%q,salt=%q,iter=%d) = %x, want %s",
				c.password, c.salt, c.iter, got, c.want)
		}
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	old := passwordHashIter
	passwordHashIter = 1000 // 测试加速
	defer func() { passwordHashIter = old }()

	stored, err := hashPassword("s3cret-pass", 0)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !strings.HasPrefix(stored, pbkdf2Prefix+"$") {
		t.Fatalf("stored format = %q", stored)
	}
	if !verifyPassword("s3cret-pass", stored) {
		t.Fatal("verifyPassword: correct password rejected")
	}
	if verifyPassword("wrong", stored) {
		t.Fatal("verifyPassword: wrong password accepted")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	bad := []string{
		"", "plaintext", "pbkdf2-sha256$1$zz$zz", // 非法 hex
		"pbkdf2-sha256$abc$00$00", // 非整数迭代
		"pbkdf2-sha256$0$00$00",   // 迭代 0
		"pbkdf2-sha256$-5$00$00",  // 负迭代
		"pbkdf2-sha256$1000$",     // 缺段
		"bcrypt$10$xx$yy",         // 异前缀
	}
	for _, s := range bad {
		if verifyPassword("whatever", s) {
			t.Errorf("verifyPassword(%q) accepted malformed stored", s)
		}
	}
}

func TestHashPasswordEmptyRejected(t *testing.T) {
	if _, err := hashPassword("", 1000); err == nil {
		t.Fatal("hashPassword('') should error")
	}
}
