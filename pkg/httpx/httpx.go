// Package httpx 提供带 *.localhost → 127.0.0.1 拨号映射的 HTTP 客户端构造。
// 背景：Go 的解析器在部分环境（如 Windows）不解析 *.localhost 子域名，而浏览器/
// curl 可以（系统级特殊处理）；kind ingress 又按 Host 头路由。因此拨号时将
// *.localhost 的地址改写为 127.0.0.1、Host 头原样保留，即可正常走 ingress。
package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewClient 构造内置 *.localhost 拨号映射的 HTTP 客户端。
func NewClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if rewritten, ok := rewriteLocalhost(addr); ok {
				addr = rewritten
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// rewriteLocalhost 将 *.localhost 地址改写为 127.0.0.1（端口不变）。
func rewriteLocalhost(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	if host == "localhost" || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return net.JoinHostPort("127.0.0.1", port), true
	}
	return "", false
}
