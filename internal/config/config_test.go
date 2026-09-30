package config

import (
	"testing"
	"time"
)

// TestMaxServiceTimeout：cmd/gateway 用它推导 http.Server.WriteTimeout，必须容得下最慢的上游。
func TestMaxServiceTimeout(t *testing.T) {
	c := &Config{Services: map[string]*Service{
		"a":      {Timeout: 2 * time.Second},
		"b":      {Timeout: 10 * time.Second},
		"broken": nil, // 配置写错时不能 panic
	}}
	if got := c.MaxServiceTimeout(); got != 10*time.Second {
		t.Errorf("MaxServiceTimeout = %v，期望 10s", got)
	}
	if got := (&Config{}).MaxServiceTimeout(); got != 0 {
		t.Errorf("空配置应返回 0，实际 %v", got)
	}
}
