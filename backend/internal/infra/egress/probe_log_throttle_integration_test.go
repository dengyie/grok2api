package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// probeLogWarnCount counts the WARN lines emitted for one attribute, so a test
// can assert the throttling invariant on what operators actually see rather
// than on the throttle's internal state.
func probeLogWarnCount(output, needle string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "msg=egress_probe_failed") && strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

func probeLogDebugCount(output, needle string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "level=DEBUG") && strings.Contains(line, "msg=egress_probe_failed") && strings.Contains(line, needle) {
			count++
		}
	}
	return count
}

// A host that stays down is probed on a fixed interval, so an unthrottled
// warning re-logs the same failure forever and buries the actual transitions.
// Two probe cycles over the same dead node must produce exactly one warning per
// address family, and the repeats must still be available at debug level.
func TestProbeEgressNodeFailureThrottledAcrossCycles(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{ID: 31, Name: "dead", Scope: domain.ScopeBuild, EncryptedProxyURL: encryptedProxy}
	manager := NewManager(&mutableEgressRepository{node: node}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, _ *http.Request) (*http.Response, error) {
			return nil, errors.New("socks connect tcp 127.0.0.1:11080: connection refused")
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))

	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := manager.ProbeEgressNode(context.Background(), node); err == nil {
			t.Fatalf("cycle %d: expected probe failure", cycle)
		}
	}
	logs := output.String()
	for _, family := range []string{"address_family=ipv4", "address_family=ipv6"} {
		if warns := probeLogWarnCount(logs, family); warns != 1 {
			t.Fatalf("%s: got %d warnings across two cycles, want 1: %s", family, warns, logs)
		}
		if debugs := probeLogDebugCount(logs, family); debugs != 1 {
			t.Fatalf("%s: got %d debug repeats across two cycles, want 1: %s", family, debugs, logs)
		}
	}
}

// Setup failures are keyed separately from address families ("all"), so a node
// that breaks, recovers and breaks again must warn on both breakdowns. Without
// clearing the setup key on success the second breakdown would stay silent for
// the rest of the process lifetime.
func TestProbeEgressNodeSetupFailureWarnsAgainAfterRecovery(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	brokenProxy, err := cipher.Encrypt("")
	if err != nil {
		t.Fatal(err)
	}
	healthyProxy, err := cipher.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	broken := domain.Node{ID: 32, Name: "broken", Scope: domain.ScopeBuild, EncryptedProxyURL: brokenProxy}
	healthy := domain.Node{ID: 32, Name: "broken", Scope: domain.ScopeBuild, EncryptedProxyURL: healthyProxy}
	manager := NewManager(&mutableEgressRepository{node: broken}, cipher)
	manager.newBuildClient = func(string, time.Duration) (requestClient, error) {
		return &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
			payload := `{"ip":"198.51.100.32"}`
			if request.URL.Hostname() == "2606:4700:4700::1111" {
				payload = `{"ip":"2001:db8::32"}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}, nil
		}}, nil
	}
	var output bytes.Buffer
	manager.SetLogger(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if _, err := manager.ProbeEgressNode(context.Background(), broken); err == nil {
		t.Fatal("expected the unconfigured proxy to fail the probe")
	}
	if _, err := manager.ProbeEgressNode(context.Background(), healthy); err != nil {
		t.Fatalf("recovered probe failed: %v", err)
	}
	if _, err := manager.ProbeEgressNode(context.Background(), broken); err == nil {
		t.Fatal("expected the unconfigured proxy to fail the probe again")
	}
	logs := output.String()
	if !strings.Contains(logs, "msg=egress_probe_succeeded") {
		t.Fatalf("recovery was not observed: %s", logs)
	}
	if warns := probeLogWarnCount(logs, "address_family=all"); warns != 2 {
		t.Fatalf("got %d setup warnings across two breakdowns, want 2: %s", warns, logs)
	}
}
