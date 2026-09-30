package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSClientConfig 把声明式的 TLS 参数翻成 crypto/tls 的配置（审计 C2）。
//
// 放在 config 包里而不是各调用方各写一份：proxy（HTTP 上游）与 transcode（gRPC 上游）
// 需要**完全一致**的 TLS 语义，各写一份迟早会漂移（比如一边记得加 SNI、另一边忘了）。
//
// 调用方拿到 error 时该怎么做：`Validate()` 已经在启动期把「文件读不到 / 证书私钥不匹配」
// 这类错误拦住并让进程起不来，所以运行期（热重载后文件被删之类）极小概率出错；
// 出错时调用方应把它当作**连接失败**处理（返回一个校验失败的配置，让握手直接报 x509 错），
// 而不是静默降级成明文或跳过校验 —— 那才是真正危险的失败模式。
func (u *Upstream) TLSClientConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12, // 1.0/1.1 已废弃，明确拒绝
	}
	if u.TLS == nil {
		return cfg, nil
	}
	cfg.ServerName = u.TLS.ServerName
	cfg.InsecureSkipVerify = u.TLS.InsecureSkipVerify

	if u.TLS.CAFile != "" {
		pemBytes, err := os.ReadFile(u.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读 CA 文件 %s 失败: %w", u.TLS.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("CA 文件 %s 里没有可用的 PEM 证书", u.TLS.CAFile)
		}
		cfg.RootCAs = pool
	}

	if u.TLS.ClientCertFile != "" || u.TLS.ClientKeyFile != "" {
		if u.TLS.ClientCertFile == "" || u.TLS.ClientKeyFile == "" {
			return nil, fmt.Errorf("mTLS 必须同时给 client_cert_file 与 client_key_file")
		}
		cert, err := tls.LoadX509KeyPair(u.TLS.ClientCertFile, u.TLS.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载 mTLS 客户端证书失败（%s / %s）: %w",
				u.TLS.ClientCertFile, u.TLS.ClientKeyFile, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// SchemeOrDefault 返回生效的 scheme（空当 http）。
func (u *Upstream) SchemeOrDefault() string {
	if u.Scheme == "" {
		return "http"
	}
	return u.Scheme
}
