// Package auth 实现 ② 认证鉴权：JWT(HS256) 与 API Key。
//
// JWT 不引库：结构就是 base64url(header).base64url(payload).base64url(HMAC)。
// 自己写一遍能看清三件事——
//
//	① 签名覆盖的是「前两段的原始字符串」，不是解析后的 JSON（否则顺序一变签名就失效）；
//	② 验签必须用 hmac.Equal（恒定时间比较），不能用 ==（会被时序攻击）；
//	③ exp/nbf 是业务校验，签名过了不代表没过期；且 **exp 必须存在** ——
//	   「没有 exp 就不过期」等于签一次永久通行（审计 P0-5 修掉了这一点）。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gwlab/internal/config"
)

var (
	ErrNoCredential = errors.New("缺少凭证")
	ErrBadFormat    = errors.New("凭证格式错误")
	ErrBadSignature = errors.New("签名不合法")
	ErrExpired      = errors.New("凭证已过期")
	ErrNoExpiry     = errors.New("凭证没有 exp（必须带过期时间）")
	ErrTooLongLived = errors.New("凭证寿命超过上限")
	ErrForbidden    = errors.New("权限不足")
)

// maxTokenLifetime 令牌寿命上限（exp - iat）。签名有效但寿命过长的令牌一律拒绝，
// 避免「签一次永久通行」——没有这个上限时，密钥泄露或内部误签发就没有止损点。
const maxTokenLifetime = 24 * time.Hour

// Claims 是网关关心的 JWT 载荷字段。
type Claims struct {
	Sub  string   `json:"sub"`
	Role []string `json:"role"`
	Exp  int64    `json:"exp"` // 必需：无 exp 视为非法（审计 P0-5）
	Iat  int64    `json:"iat"`
	Nbf  int64    `json:"nbf"`
}

// Identity 鉴权通过后放进请求上下文，供上游与日志使用。
type Identity struct {
	Subject string
	Roles   []string
	Scheme  string
}

// Authenticator 持有验签所需的密钥。零值不可用，请用 New 构造。
type Authenticator struct {
	jwtSecret string
	apiKey    string
}

// New 构造一个鉴权器。
func New(jwtSecret, apiKey string) *Authenticator {
	return &Authenticator{jwtSecret: jwtSecret, apiKey: apiKey}
}

// Authenticate 按路由策略鉴权。
func (a *Authenticator) Authenticate(r *http.Request, p config.AuthPolicy) (*Identity, error) {
	if !p.Required {
		return &Identity{Subject: "anonymous", Scheme: "none"}, nil
	}
	switch p.Scheme {
	case "jwt":
		raw := r.Header.Get("Authorization")
		if raw == "" {
			return nil, ErrNoCredential
		}
		// auth-scheme 大小写不敏感（RFC 7235 §2.1）：原实现 strings.CutPrefix(raw, "Bearer ")
		// 是逐字节比较，于是 `bearer xxx` / `BEARER xxx` 被判成「凭证格式错误」而 401
		//（实测踩过，审计 P2-1）。这里只对 scheme 做 EqualFold，分隔符仍严格要求空格，
		// 所以 `Token xxx` 这类别的 scheme 依旧被拒。
		scheme, token, ok := strings.Cut(raw, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return nil, ErrBadFormat
		}
		claims, err := a.VerifyJWT(strings.TrimSpace(token))
		if err != nil {
			return nil, err
		}
		if len(p.Roles) > 0 && !anyRole(claims.Role, p.Roles) {
			return nil, ErrForbidden
		}
		return &Identity{Subject: claims.Sub, Roles: claims.Role, Scheme: "jwt"}, nil

	case "apikey":
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}
		if key == "" {
			return nil, ErrNoCredential
		}
		if !hmac.Equal([]byte(key), []byte(a.apiKey)) { // 同样是恒定时间比较
			return nil, ErrBadSignature
		}
		subject := key
		if len(subject) > 8 {
			subject = subject[:8]
		}
		return &Identity{Subject: "apikey:" + subject, Scheme: "apikey"}, nil
	}
	return nil, ErrBadFormat
}

// VerifyJWT 校验 HS256 签名并返回 claims。
func (a *Authenticator) VerifyJWT(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrBadFormat
	}
	// ① 签名对象是「前两段的字面量拼接」
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(a.jwtSecret))
	mac.Write([]byte(signingInput))
	want := mac.Sum(nil)
	got, err := b64decode(parts[2])
	if err != nil {
		return nil, ErrBadFormat
	}
	// ② 恒定时间比较
	if !hmac.Equal(want, got) {
		return nil, ErrBadSignature
	}
	// ③ 再校验业务声明
	var c Claims
	payload, err := b64decode(parts[1])
	if err != nil {
		return nil, ErrBadFormat
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrBadFormat
	}
	now := time.Now().Unix()
	// ⚠️ exp 必须存在：原实现是 `if c.Exp > 0 && now > c.Exp`，于是 exp 缺失的令牌
	// 被当成「永不过期」接受（实测 200）—— 等于给了一把不能吊销的永久钥匙（审计 P0-5）。
	if c.Exp == 0 {
		return nil, ErrNoExpiry
	}
	if now > c.Exp {
		return nil, ErrExpired
	}
	if c.Nbf > 0 && now < c.Nbf {
		return nil, fmt.Errorf("凭证尚未生效")
	}
	// 寿命上限只在 iat 存在时可判（JWT 本身不强制 iat）；缺失时无法判断，故只在有 iat 时执行。
	if c.Iat > 0 && time.Duration(c.Exp-c.Iat)*time.Second > maxTokenLifetime {
		return nil, ErrTooLongLived
	}
	return &c, nil
}

// SignJWT 造一个 token —— 只给演示/测试用，网关自己不会签发。
// 未给 iat 时自动补当前时间，这样签出来的令牌也受寿命上限约束。
func (a *Authenticator) SignJWT(c Claims) string {
	if c.Iat == 0 {
		c.Iat = time.Now().Unix()
	}
	h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	p, _ := json.Marshal(c)
	head := base64.RawURLEncoding.EncodeToString(h)
	body := base64.RawURLEncoding.EncodeToString(p)
	mac := hmac.New(sha256.New, []byte(a.jwtSecret))
	mac.Write([]byte(head + "." + body))
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Status 把鉴权错误映射成状态码 —— 401 与 403 的区别要分明：
// 没凭证/凭证无效 → 401（你可以再试）；有身份但权限不够 → 403（别再试了）。
func Status(err error) int {
	if errors.Is(err, ErrForbidden) {
		return http.StatusForbidden
	}
	return http.StatusUnauthorized
}

// Code 给出与 Status 配套的机器可读错误码。
func Code(err error) string {
	switch {
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrNoExpiry):
		return "token_no_expiry"
	case errors.Is(err, ErrTooLongLived):
		return "token_too_long_lived"
	}
	return "unauthorized"
}

// ---- 身份在请求上下文中的传递 ----

type ctxKey struct{}

var identityKey ctxKey

// WithIdentity 把鉴权结果写入上下文。
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// FromContext 取回鉴权结果，未鉴权时返回 nil。
func FromContext(ctx context.Context) *Identity {
	if v, ok := ctx.Value(identityKey).(*Identity); ok {
		return v
	}
	return nil
}

func b64decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func anyRole(got, want []string) bool {
	for _, g := range got {
		for _, w := range want {
			if strings.EqualFold(g, w) {
				return true
			}
		}
	}
	return false
}
