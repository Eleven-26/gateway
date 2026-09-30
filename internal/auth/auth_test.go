package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gwlab/internal/config"
)

// TestVerifyJWTExpiryPolicy 盯住 exp 策略（审计 P0-5）：
// 修之前 `if c.Exp > 0 && now > c.Exp` 会把「没有 exp」当成永不过期（实测返回 200）。
func TestVerifyJWTExpiryPolicy(t *testing.T) {
	a := New("test-secret", "test-key")
	now := time.Now().Unix()

	cases := []struct {
		name   string
		claims Claims
		want   error // nil 表示应当通过
	}{
		{"正常令牌", Claims{Sub: "u", Iat: now, Exp: now + 60}, nil},
		{"没有 exp：必须拒", Claims{Sub: "u"}, ErrNoExpiry},
		{"已过期", Claims{Sub: "u", Iat: now - 3600, Exp: now - 1}, ErrExpired},
		{"寿命超过 24h", Claims{Sub: "u", Iat: now, Exp: now + int64((25 * time.Hour).Seconds())}, ErrTooLongLived},
	}
	for _, tc := range cases {
		_, err := a.VerifyJWT(a.SignJWT(tc.claims))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: 期望通过，实际 %v", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: 期望 %v，实际 %v", tc.name, tc.want, err)
		}
	}
}

// TestVerifyJWTNotYetValid：nbf 未生效的令牌要拒。
func TestVerifyJWTNotYetValid(t *testing.T) {
	a := New("test-secret", "test-key")
	now := time.Now().Unix()

	_, err := a.VerifyJWT(a.SignJWT(Claims{Sub: "u", Iat: now, Nbf: now + 300, Exp: now + 600}))
	if err == nil || !strings.Contains(err.Error(), "尚未生效") {
		t.Fatalf("nbf 未到时应当拒绝，实际 %v", err)
	}
}

// TestAuthenticateAPIKey：API Key 走恒定时间比较，错误 key 必须 401 而不是 403。
func TestAuthenticateAPIKey(t *testing.T) {
	a := New("test-secret", "ak_test")
	policy := config.AuthPolicy{Required: true, Scheme: "apikey"}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "ak_wrong")
	if _, err := a.Authenticate(req, policy); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("错误 API Key 应返回 ErrBadSignature，实际 %v", err)
	}
	if got := Status(ErrBadSignature); got != http.StatusUnauthorized {
		t.Errorf("Status(ErrBadSignature) = %d，期望 401", got)
	}

	ok := httptest.NewRequest(http.MethodGet, "/", nil)
	ok.Header.Set("X-API-Key", "ak_test")
	id, err := a.Authenticate(ok, policy)
	if err != nil {
		t.Fatalf("正确 API Key 应当通过，实际 %v", err)
	}
	if id.Subject != "apikey:ak_test" {
		t.Errorf("身份 = %q，期望 apikey:ak_test", id.Subject)
	}
}

// TestAuthenticateBearerSchemeCaseInsensitive 盯住 auth-scheme 大小写不敏感（审计 P2-1）：
// RFC 7235 §2.1 规定 scheme 大小写不敏感，但原实现是 strings.CutPrefix(raw, "Bearer ")，
// 逐字节比较，于是实测 `bearer <合法token>` 被答成 401「凭证格式错误」。
func TestAuthenticateBearerSchemeCaseInsensitive(t *testing.T) {
	a := New("test-secret", "test-key")
	policy := config.AuthPolicy{Required: true, Scheme: "jwt"}
	now := time.Now().Unix()
	token := a.SignJWT(Claims{Sub: "u", Iat: now, Exp: now + 60})

	cases := []struct {
		name   string
		header string
		want   error // nil 表示应当通过
	}{
		{"标准写法 Bearer", "Bearer " + token, nil},
		{"小写 bearer 必须通过", "bearer " + token, nil},
		{"全大写 BEARER 必须通过", "BEARER " + token, nil},
		{"混合大小写 BeArEr 必须通过", "BeArEr " + token, nil},
		{"别的 scheme（Token）必须被拒", "Token " + token, ErrBadFormat},
		{"裸 token（无 scheme）必须被拒", token, ErrBadFormat},
		{"用 Tab 分隔也必须被拒（只接受空格）", "Bearer\t" + token, ErrBadFormat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", tc.header)
			id, err := a.Authenticate(req, policy)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("期望通过，实际 %v", err)
			case tc.want == nil && (id == nil || id.Subject != "u"):
				t.Fatalf("身份 = %+v，期望 Subject=u", id)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("期望 %v，实际 %v", tc.want, err)
			}
		})
	}
}
