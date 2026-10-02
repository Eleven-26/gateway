package proxy

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gwlab/internal/config"
)

// writeServerCA 把 httptest TLS 服务器的自签证书落成 PEM，供上游 CA 配置使用。
func writeServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("httptest TLS 服务器没有暴露证书")
	}
	if _, err := x509.ParseCertificate(cert.Raw); err != nil {
		t.Fatalf("证书无法解析: %v", err)
	}
	path := filepath.Join(t.TempDir(), "upstream-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serveOnce(t *testing.T, p *Proxy) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/secure", nil))
	return rec.Code, rec.Body.String()
}

// TestUpstreamTLSScheme 覆盖上游 TLS 的核心：
//   - scheme=https + 配了上游 CA → 握手成功、能正常转发；
//   - scheme=https 但没给 CA（用系统根证书）→ 证书校验失败，回 502；
//   - InsecureSkipVerify 时跳过校验 → 又能通（证明开关真的生效）。
//
// 这三种情形正好对应「配对了 / 忘了配 / 图省事关掉校验」，是上游 TLS 最常见的三种状态。
func TestUpstreamTLSScheme(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure-ok"))
	}))
	defer up.Close()
	addr := up.Listener.Addr().String()
	caFile := writeServerCA(t, up)

	t.Run("配了上游 CA 就能通", func(t *testing.T) {
		p := New(&config.Upstream{Addr: addr, Scheme: "https", TLS: &config.TLSConfig{CAFile: caFile}}, "")
		code, body := serveOnce(t, p)
		if code != 200 || !strings.Contains(body, "secure-ok") {
			t.Fatalf("期望 200 secure-ok，实际 code=%d body=%s", code, body)
		}
	})

	t.Run("没配 CA 时证书校验必须失败", func(t *testing.T) {
		p := New(&config.Upstream{Addr: addr, Scheme: "https"}, "")
		code, body := serveOnce(t, p)
		if code != http.StatusBadGateway {
			t.Fatalf("自签证书 + 系统根证书应当握手失败（502），实际 code=%d body=%s", code, body)
		}
	})

	t.Run("InsecureSkipVerify 时跳过校验", func(t *testing.T) {
		p := New(&config.Upstream{Addr: addr, Scheme: "https", TLS: &config.TLSConfig{InsecureSkipVerify: true}}, "")
		code, body := serveOnce(t, p)
		if code != 200 || !strings.Contains(body, "secure-ok") {
			t.Fatalf("跳过校验时应当能通，实际 code=%d body=%s", code, body)
		}
	})
}

// TestSchemeDefaultsToHTTP：不写 scheme 时仍是明文 HTTP（不能因为引入 TLS 而改变默认行为）。
func TestSchemeDefaultsToHTTP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain-ok"))
	}))
	defer up.Close()

	p := New(&config.Upstream{Addr: up.Listener.Addr().String()}, "")
	if code, body := serveOnce(t, p); code != 200 || !strings.Contains(body, "plain-ok") {
		t.Fatalf("默认 scheme 应当是 http，实际 code=%d body=%s", code, body)
	}
}
