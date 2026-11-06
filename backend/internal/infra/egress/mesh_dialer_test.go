package egress

import (
	"testing"
)

func TestIsMeshAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"10.144.144.3", true},
		{"10.144.144.3:10882", true},
		{"10.144.0.1", true},
		{"10.144.255.254:443", true},
		{"10.145.1.1", false},
		{"192.168.1.1", false},
		{"127.0.0.1", false},
		{"127.0.0.1:11080", false},
		{"1.1.1.1", false},
		{"grok.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := IsMeshAddr(tt.addr)
			if got != tt.want {
				t.Errorf("IsMeshAddr(%q) = %v; want %v", tt.addr, got, tt.want)
			}
		})
	}
}
