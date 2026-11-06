package egress

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/easytier"
	xproxy "golang.org/x/net/proxy"
)

var (
	meshDialerMu sync.RWMutex
	meshDialer   ContextAndDirectDialer
)

// ContextAndDirectDialer is an interface combining proxy.Dialer and proxy.ContextDialer.
type ContextAndDirectDialer interface {
	xproxy.Dialer
	xproxy.ContextDialer
}

// IsMeshAddr reports whether the host or host:port belongs to the EasyTier
// mesh private IPv4 range. Consensus lives in internal/pkg/easytier.
func IsMeshAddr(hostOrAddr string) bool {
	return easytier.IsMeshAddr(hostOrAddr)
}

// EasyTierProxyEndpoint returns the configured or default EasyTier SOCKS5 proxy
// endpoint.
func EasyTierProxyEndpoint() string {
	return easytier.ProxyEndpoint()
}

// getOrCreateMeshDialer returns a cached SOCKS5 dialer targeting the local EasyTier proxy.
func getOrCreateMeshDialer() (ContextAndDirectDialer, error) {
	meshDialerMu.RLock()
	d := meshDialer
	meshDialerMu.RUnlock()
	if d != nil {
		return d, nil
	}

	meshDialerMu.Lock()
	defer meshDialerMu.Unlock()
	if meshDialer != nil {
		return meshDialer, nil
	}

	ep := EasyTierProxyEndpoint()
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	socksDialer, err := xproxy.SOCKS5("tcp", ep, nil, baseDialer)
	if err != nil {
		return nil, fmt.Errorf("创建 EasyTier mesh SOCKS5 代理失败 (%s): %w", ep, err)
	}

	meshDialer = &meshContextDialerWrapper{d: socksDialer}
	return meshDialer, nil
}

type meshContextDialerWrapper struct {
	d xproxy.Dialer
}

func (w *meshContextDialerWrapper) Dial(network, addr string) (net.Conn, error) {
	return w.d.Dial(network, addr)
}

func (w *meshContextDialerWrapper) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := w.d.(xproxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := w.d.Dial(network, addr)
		ch <- result{conn: c, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

// NewMeshAwareDialer returns a dialer that automatically routes connections
// destined for EasyTier mesh IPs (10.144.0.0/16) through the local EasyTier SOCKS5 proxy,
// and uses direct connection for all other destinations.
func NewMeshAwareDialer(direct *net.Dialer) ContextAndDirectDialer {
	if direct == nil {
		direct = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return &meshAwareDialer{direct: direct}
}

type meshAwareDialer struct {
	direct *net.Dialer
}

func (m *meshAwareDialer) Dial(network, addr string) (net.Conn, error) {
	return m.DialContext(context.Background(), network, addr)
}

func (m *meshAwareDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if (network == "tcp" || network == "tcp4" || network == "tcp6") && IsMeshAddr(addr) {
		// Attempt to route through EasyTier SOCKS5 proxy
		if mesh, err := getOrCreateMeshDialer(); err == nil {
			conn, err := mesh.DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			// If mesh proxy fails, fallback to direct in case running on a machine with native TUN
		}
	}
	return m.direct.DialContext(ctx, network, addr)
}

// isHTTPProxyScheme reports whether the scheme is an HTTP CONNECT proxy.
func isHTTPProxyScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https":
		return true
	}
	return false
}

// NewProxyDialerWithMeshSupport creates a dialer for a SOCKS proxy URL.
// If the proxy target host is an EasyTier mesh IP, it automatically chains through
// the local EasyTier user-space proxy.
//
// HTTP(S) proxies are not supported here: x/net/proxy only understands SOCKS.
// Callers must let net/http (buildclient/pinnedclient) or meshHTTPConnectDialer
// (tls-client) perform the CONNECT handshake instead.
func NewProxyDialerWithMeshSupport(proxyURL *url.URL, direct *net.Dialer) (xproxy.Dialer, error) {
	if proxyURL == nil {
		return nil, fmt.Errorf("代理地址不能为空")
	}
	if isHTTPProxyScheme(proxyURL.Scheme) {
		return nil, fmt.Errorf("HTTP(S) 代理不能通过 x/net/proxy 建立，请使用 CONNECT 拨号器")
	}
	if direct == nil {
		direct = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	var forward xproxy.Dialer = direct
	if IsMeshAddr(proxyURL.Host) {
		forward = NewMeshAwareDialer(direct)
	}
	return xproxy.FromURL(proxyURL, forward)
}
