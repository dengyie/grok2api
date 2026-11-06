// Package easytier centralizes the EasyTier mesh conventions shared by the
// egress clients and the tunnel proxy: the private IPv4 range used for mesh
// addresses and the local user-space SOCKS5 endpoint exposed in no-tun mode.
package easytier

import (
	"net"
	"os"
	"strings"
)

// DefaultSocks5Proxy is the local user-space SOCKS5 endpoint exposed by
// EasyTier when running in no-tun mode (e.g., inside container environments
// like Bohrium).
const DefaultSocks5Proxy = "127.0.0.1:11080"

// IsMeshAddr reports whether the given host or host:port belongs to the
// EasyTier mesh private IPv4 range (10.144.0.0/16).
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

// ProxyEndpoint returns the configured or default EasyTier SOCKS5 proxy
// endpoint. EASYTIER_PROXY overrides the default.
func ProxyEndpoint() string {
	if endpoint := strings.TrimSpace(os.Getenv("EASYTIER_PROXY")); endpoint != "" {
		return endpoint
	}
	return DefaultSocks5Proxy
}
