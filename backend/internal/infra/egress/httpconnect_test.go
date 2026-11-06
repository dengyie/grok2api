package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"
)

// connectTestProxy accepts CONNECT requests, optionally checks credentials,
// then pipes bytes to the requested target.
type connectTestProxy struct {
	listener net.Listener
	auth     string
	status   string
	targets  chan string
	done     chan struct{}
}

func newConnectTestProxy(t *testing.T, auth, status string) *connectTestProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &connectTestProxy{
		listener: listener,
		auth:     auth,
		status:   status,
		targets:  make(chan string, 1),
		done:     make(chan struct{}),
	}
	go proxy.serve(t)
	return proxy
}

func (p *connectTestProxy) URL() string {
	prefix := "http://"
	if p.auth != "" {
		prefix = "http://" + p.auth + "@"
	}
	return prefix + p.listener.Addr().String()
}

func (p *connectTestProxy) Close() {
	_ = p.listener.Close()
	<-p.done
}

func (p *connectTestProxy) serve(t *testing.T) {
	defer close(p.done)
	connection, err := p.listener.Accept()
	if err != nil {
		return
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	protocol := textproto.NewReader(reader)
	requestLine, err := protocol.ReadLine()
	if err != nil {
		t.Errorf("read CONNECT request line: %v", err)
		return
	}
	fields := strings.Fields(requestLine)
	if len(fields) < 2 || fields[0] != "CONNECT" {
		t.Errorf("request line = %q", requestLine)
		return
	}
	target := fields[1]
	headers, err := protocol.ReadMIMEHeader()
	if err != nil {
		t.Errorf("read CONNECT headers: %v", err)
		return
	}
	if headers.Get("Host") != target {
		t.Errorf("CONNECT Host = %q, want %q", headers.Get("Host"), target)
	}
	if p.auth != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.auth))
		if headers.Get("Proxy-Authorization") != want {
			t.Errorf("Proxy-Authorization = %q, want %q", headers.Get("Proxy-Authorization"), want)
		}
	}
	p.targets <- target
	if p.status != "200" {
		_, _ = io.WriteString(connection, "HTTP/1.1 "+p.status+" Denied\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		t.Errorf("dial target: %v", err)
		return
	}
	defer upstream.Close()
	_ = connection.SetDeadline(time.Time{})
	_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection established\r\n\r\n")
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(upstream, reader)
		copyDone <- copyErr
	}()
	_, downstreamErr := io.Copy(connection, upstream)
	upstreamErr := <-copyDone
	if !isBenignTunnelCopyError(downstreamErr) {
		t.Errorf("downstream copy: %v", downstreamErr)
	}
	if !isBenignTunnelCopyError(upstreamErr) {
		t.Errorf("upstream copy: %v", upstreamErr)
	}
}

// isBenignTunnelCopyError ignores the teardown races that are expected when the
// test client closes its side right after reading the response.
func isBenignTunnelCopyError(err error) bool {
	if err == nil {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"closed network connection", "connection reset by peer", "broken pipe"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func TestMeshHTTPConnectDialerTunnelsTLS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Connection", "close")
		_, _ = io.WriteString(writer, "tunneled")
	}))
	defer target.Close()

	proxy := newConnectTestProxy(t, "user:secret", "200")
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}

	dialer := newMeshHTTPConnectDialer(proxyURL, nil)
	targetAddr := strings.TrimPrefix(target.URL, "https://")
	conn, err := dialer.DialContext(context.Background(), "tcp", targetAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: "+targetAddr+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "tunneled" {
		t.Fatalf("body = %q err = %v", body, err)
	}
	select {
	case requested := <-proxy.targets:
		if requested != targetAddr {
			t.Fatalf("CONNECT target = %q, want %q", requested, targetAddr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not receive CONNECT")
	}
}

func TestMeshHTTPConnectDialerReportsProxyRejection(t *testing.T) {
	proxy := newConnectTestProxy(t, "", "407")
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	dialer := newMeshHTTPConnectDialer(proxyURL, nil)
	_, err = dialer.DialContext(context.Background(), "tcp", "grok.com:443")
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("error = %v, want CONNECT rejection with 407", err)
	}
	select {
	case <-proxy.targets:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not receive CONNECT")
	}
}

func TestParseConnectStatusLine(t *testing.T) {
	tests := []struct {
		line    string
		code    int
		wantErr bool
	}{
		{line: "HTTP/1.1 200 Connection established", code: 200},
		{line: "HTTP/1.0 407 Proxy Authentication Required", code: 407},
		{line: "garbage", wantErr: true},
		{line: "HTTP/1.1 abc", wantErr: true},
	}
	for _, test := range tests {
		code, err := parseConnectStatusLine(test.line)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseConnectStatusLine(%q) accepted invalid line", test.line)
			}
			continue
		}
		if err != nil || code != test.code {
			t.Errorf("parseConnectStatusLine(%q) = %d, %v; want %d", test.line, code, err, test.code)
		}
	}
}

func TestMeshHTTPConnectDialerDefaultsProxyPort(t *testing.T) {
	cases := map[string]string{
		"http://10.144.144.3":        "10.144.144.3:80",
		"https://10.144.144.3":       "10.144.144.3:443",
		"http://10.144.144.3:10801":  "10.144.144.3:10801",
		"https://10.144.144.3:11080": "10.144.144.3:11080",
	}
	for raw, want := range cases {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := newMeshHTTPConnectDialer(parsed, nil).proxyAddr(); got != want {
			t.Errorf("proxyAddr(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestMeshHTTPConnectDialerRejectsNonTCP(t *testing.T) {
	parsed, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newMeshHTTPConnectDialer(parsed, nil).DialContext(context.Background(), "udp", "grok.com:53"); err == nil {
		t.Fatal("udp dial was accepted")
	}
}
