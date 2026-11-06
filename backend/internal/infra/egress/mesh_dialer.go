package egress

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// defaultEasyTierSocks5Proxy is the local user-space SOCKS5 endpoint exposed by
// EasyTier when running in no-tun mode (e.g., inside container environments like Bohrium).
const defaultEasyTierSocks5Proxy = "127.0.0.1:11080"

var (
	meshDialerMu sync.RWMutex
	meshDialer   ContextAndDirectDialer
)

// ContextAndDirectDialer is an interface combining proxy.Dialer and proxy.ContextDialer.
type ContextAndDirectDialer interface {
	xproxy.Dialer
	xproxy.ContextDialer
}

// IsMeshAddr checks if the given host or host:port belongs to the EasyTier mesh
// private IPv4 range (10.144.0.0/16).
func IsMeshAddr(hostOrAddr string) bool {
	host := strings.TrimSpace(hostOrAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	ipv4 := ip.To4()
	if ipv4 == nil {
		return false
	}
	return ipv4[0] == 10 && ipv4[1] == 144
}

// EasyTierProxyEndpoint returns the configured or default EasyTier SOCKS5 proxy endpoint.
func EasyTierProxyEndpoint() string {
	if ep := strings.TrimSpace(os.Getenv("EASYTIER_PROXY")); ep != "" {
		return ep
	}
	return defaultEasyTierSocks5Proxy
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

// NewProxyDialerWithMeshSupport creates a dialer for an egress proxy URL.
// If the proxy target host is an EasyTier mesh IP, it automatically chains through
// the local EasyTier user-space proxy.
func NewProxyDialerWithMeshSupport(proxyURL *url.URL, direct *net.Dialer) (xproxy.Dialer, error) {
	if direct == nil {
		direct = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	var forward xproxy.Dialer = direct
	if IsMeshAddr(proxyURL.Host) {
		forward = NewMeshAwareDialer(direct)
	}
	return xproxy.FromURL(proxyURL, forward)
}
