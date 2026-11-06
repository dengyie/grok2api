package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// meshHTTPConnectDialer dials the final destination through an HTTP or HTTPS
// CONNECT proxy that lives on the EasyTier mesh.
//
// tls-client refuses to combine WithDialContext with WithProxyUrl ("you must
// handle the proxy connection (CONNECT handshake) yourself inside that
// dialer"), so a browser client behind a mesh HTTP proxy needs this dialer.
// The proxy hop itself is still routed through EasyTier by NewMeshAwareDialer.
type meshHTTPConnectDialer struct {
	proxy    *url.URL
	direct   ContextAndDirectDialer
	timeout  time.Duration
	proxyTLS bool
}

func newMeshHTTPConnectDialer(proxyURL *url.URL, direct *net.Dialer) *meshHTTPConnectDialer {
	if direct == nil {
		direct = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return &meshHTTPConnectDialer{
		proxy:    proxyURL,
		direct:   NewMeshAwareDialer(direct),
		timeout:  10 * time.Second,
		proxyTLS: strings.EqualFold(proxyURL.Scheme, "https"),
	}
}

func (d *meshHTTPConnectDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *meshHTTPConnectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "", "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("mesh HTTP CONNECT 不支持网络类型 %q", network)
	}
	proxyAddr := d.proxyAddr()
	conn, err := d.direct.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接 mesh HTTP 代理 %s 失败: %w", proxyAddr, err)
	}
	deadline := time.Now().Add(d.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	if d.proxyTLS {
		tlsConn := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: d.proxy.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("与 mesh HTTPS 代理 %s 建立 TLS 失败: %w", proxyAddr, err))
		}
		conn = tlsConn
	}

	var request strings.Builder
	request.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
	if user := d.proxy.User; user != nil {
		password, _ := user.Password()
		token := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + password))
		request.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return fail(fmt.Errorf("发送 mesh HTTP CONNECT 请求失败: %w", err))
	}

	reader := bufio.NewReader(conn)
	protocol := textproto.NewReader(reader)
	statusLine, err := protocol.ReadLine()
	if err != nil {
		return fail(fmt.Errorf("读取 mesh HTTP CONNECT 状态失败: %w", err))
	}
	statusCode, err := parseConnectStatusLine(statusLine)
	if err != nil {
		return fail(err)
	}
	if _, err := protocol.ReadMIMEHeader(); err != nil {
		return fail(fmt.Errorf("读取 mesh HTTP CONNECT 响应头失败: %w", err))
	}
	if statusCode != http.StatusOK {
		return fail(fmt.Errorf("mesh HTTP 代理 CONNECT %s 失败: %s", addr, statusLine))
	}
	_ = conn.SetDeadline(time.Time{})
	// The proxy may have pipelined bytes after the response headers (the
	// target's TLS ServerHello in some setups); keep them for the caller.
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func (d *meshHTTPConnectDialer) proxyAddr() string {
	if d.proxy.Port() != "" {
		return d.proxy.Host
	}
	if d.proxyTLS {
		return net.JoinHostPort(d.proxy.Hostname(), "443")
	}
	return net.JoinHostPort(d.proxy.Hostname(), "80")
}

func parseConnectStatusLine(statusLine string) (int, error) {
	fields := strings.Fields(statusLine)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, fmt.Errorf("无效的 mesh HTTP CONNECT 状态行: %q", statusLine)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 599 {
		return 0, fmt.Errorf("无效的 mesh HTTP CONNECT 状态码: %q", statusLine)
	}
	return code, nil
}

// bufferedConn preserves bytes already buffered while reading the CONNECT
// response so the TLS handshake sees the full stream.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
