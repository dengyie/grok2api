package web

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// videoFeedbackRepository models one bound egress node and records the health
// transitions the video path reports for it.
type videoFeedbackRepository struct {
	egressRepositoryStub
	mu           sync.Mutex
	node         egressdomain.Node
	failureCount int
	cooldownSet  bool
}

func (r *videoFeedbackRepository) GetEgressNode(_ context.Context, id uint64) (egressdomain.Node, error) {
	if id != r.node.ID {
		return egressdomain.Node{}, errors.New("not found")
	}
	return r.node, nil
}

func (r *videoFeedbackRepository) UpdateEgressNodeHealth(_ context.Context, _ uint64, _ float64, failureCount int, cooldownUntil *time.Time, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failureCount = failureCount
	r.cooldownSet = cooldownUntil != nil
	return nil
}

func (r *videoFeedbackRepository) UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error {
	return nil
}

func (r *videoFeedbackRepository) UpdateEgressNodeLastError(context.Context, uint64, string) error {
	return nil
}

// A create POST that dies during dialing must be reported to the egress
// manager, otherwise the dead node never cools down and every sticky account
// bound to it keeps paying the dial timeout instead of failing over.
func TestGenerateVideoReportsDeadEgressNode(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	// Reserve then release a port so the SOCKS endpoint is certainly closed.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := listener.Addr().String()
	_ = listener.Close()

	encryptedProxy, err := cipher.Encrypt("socks5://" + deadAddr)
	if err != nil {
		t.Fatal(err)
	}
	repository := &videoFeedbackRepository{node: egressdomain.Node{
		ID: 7, Name: "dead-node", Scope: egressdomain.ScopeWeb, Enabled: true,
		Health: 1, EncryptedProxyURL: encryptedProxy,
	}}
	adapter := NewAdapter(Config{
		BaseURL: "https://grok.com", StatsigMode: "manual", StatsigManualValue: "test",
		VideoTimeoutSeconds: 3,
	}, infraegress.NewManager(repository, cipher), cipher, nil, nil)

	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{
			ID: 1, Provider: account.ProviderWeb, WebTier: account.WebTierBasic,
			EncryptedAccessToken: token, EgressNodeID: 7,
		},
		Prompt: "dead egress probe", Duration: 6,
	})
	if err == nil {
		t.Fatal("video create through a dead egress node unexpectedly succeeded")
	}
	repository.mu.Lock()
	failureCount, cooldownSet := repository.failureCount, repository.cooldownSet
	repository.mu.Unlock()
	if failureCount < 1 || !cooldownSet {
		t.Fatalf("dead egress node was not cooled: failureCount=%d cooldown=%v", failureCount, cooldownSet)
	}
}

// stallingProxy is a minimal SOCKS5 server (no auth) that accepts every CONNECT
// and then never forwards a byte. A client talking to it has provably completed
// dialing — the connection and the SOCKS handshake both succeeded — and only
// stalls waiting for the peer's response, which is how a slow but healthy egress
// looks to the create path.
func stallingProxy(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				// version + methods.
				head := make([]byte, 2)
				if _, err := io.ReadFull(conn, head); err != nil {
					return
				}
				if _, err := io.ReadFull(conn, make([]byte, int(head[1]))); err != nil {
					return
				}
				if _, err := conn.Write([]byte{5, 0}); err != nil {
					return
				}
				// version + connect + reserved + address type.
				request := make([]byte, 4)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				switch request[3] {
				case 1:
					var addr [4]byte
					if _, err := io.ReadFull(conn, addr[:]); err != nil {
						return
					}
				case 3:
					var length [1]byte
					if _, err := io.ReadFull(conn, length[:]); err != nil {
						return
					}
					if _, err := io.ReadFull(conn, make([]byte, int(length[0]))); err != nil {
						return
					}
				case 4:
					var addr [16]byte
					if _, err := io.ReadFull(conn, addr[:]); err != nil {
						return
					}
				default:
					return
				}
				if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
					return
				}
				// Command succeeded; the dial is now complete as far as the client knows.
				if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				// Swallow the request and never answer: the client can only time out
				// waiting for response headers.
				_, _ = io.Copy(io.Discard, conn)
			}(conn)
		}
	}()
	return "socks5://" + listener.Addr().String()
}

// A create that got past dialing and merely timed out waiting for the peer says
// nothing about the proxy. Cooling the node for that would trade a working exit
// for one slow upstream response and drain the pool under load, so this path must
// stay silent while a genuine dead egress (see TestGenerateVideoReportsDeadEgressNode)
// still reports.
func TestGenerateVideoSlowUpstreamDoesNotCoolEgressNode(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt(stallingProxy(t))
	if err != nil {
		t.Fatal(err)
	}
	repository := &videoFeedbackRepository{node: egressdomain.Node{
		ID: 8, Name: "slow-node", Scope: egressdomain.ScopeWeb, Enabled: true,
		Health: 1, EncryptedProxyURL: encryptedProxy,
	}}
	adapter := NewAdapter(Config{
		BaseURL: "http://example.invalid", StatsigMode: "manual", StatsigManualValue: "test",
		VideoTimeoutSeconds: 2,
	}, infraegress.NewManager(repository, cipher), cipher, nil, nil)

	startedAt := time.Now()
	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{
			ID: 2, Provider: account.ProviderWeb, WebTier: account.WebTierBasic,
			EncryptedAccessToken: token, EgressNodeID: 8,
		},
		Prompt: "slow upstream probe", Duration: 6,
	})
	repository.mu.Lock()
	failureCount, cooldownSet := repository.failureCount, repository.cooldownSet
	repository.mu.Unlock()

	stalledAtHeaders := err != nil && time.Since(startedAt) >= time.Second
	if !stalledAtHeaders {
		t.Fatalf("create did not stall at response headers as intended: err=%v elapsed=%s", err, time.Since(startedAt))
	}
	if provider.VideoCreateFailureStage(err) != provider.VideoStageSubmitted {
		t.Fatalf("precondition failed: a connected-but-slow create must be indeterminate, not a create failure: %v", err)
	}
	if failureCount != 0 || cooldownSet {
		t.Fatalf("slow upstream cooled a healthy node: failureCount=%d cooldown=%v", failureCount, cooldownSet)
	}
}
