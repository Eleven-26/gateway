package auth

import (
	"testing"
	"time"
)

// FuzzVerifyJWT 要求：任意字符串都不能让验签 panic。
// 能通过验签的令牌只可能是"签名正确"的那些（HMAC 用固定密钥），
// 所以这里不断言具体错误，只守住"不崩"和"通过则 claims 可用"。
func FuzzVerifyJWT(f *testing.F) {
	a := New("fuzz-secret", "fuzz-key")
	now := time.Now().Unix()

	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("a.b.c")
	f.Add("Bearer x.y.z")
	f.Add(a.SignJWT(Claims{Sub: "u", Iat: now, Exp: now + 60}))
	f.Add(a.SignJWT(Claims{Sub: "u"}))                                 // 无 exp：现在必须被拒
	f.Add(a.SignJWT(Claims{Sub: "u", Iat: now, Exp: now + 86400*365})) // 寿命超限
	f.Add("eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.e30.")                  // alg=none + 空签名段

	f.Fuzz(func(t *testing.T, token string) {
		claims, err := a.VerifyJWT(token)
		if err != nil {
			return
		}
		if claims == nil {
			t.Fatal("err 为 nil 但 claims 也是 nil")
		}
		if claims.Exp == 0 {
			t.Fatal("无 exp 的令牌不应该通过验签")
		}
	})
}
