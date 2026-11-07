package web

import (
	"context"
	"encoding/base64"
	"errors"
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
