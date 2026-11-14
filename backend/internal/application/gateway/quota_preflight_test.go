package gateway

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestCandidateQuotaRankStaleSnapshotFallsToUnknown 覆盖 11-14 事故根因：
// 5 天前同步的 remaining=1 快照曾被按 available(2) 优先调度，全池先烧必败账号。
// 快照龄期超过 staleAfter 后必须降为 unknown，排到 fresh 快照之后。
func TestCandidateQuotaRankStaleSnapshotFallsToUnknown(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	window := func(syncedAt *time.Time, remaining int) *account.QuotaWindow {
		return &account.QuotaWindow{
			AccountID: 1, Mode: account.QuotaModeWebVideo720p,
			Remaining: remaining, Source: account.QuotaSourceUpstream,
			ResetAt: &future, SyncedAt: syncedAt,
		}
	}
	candidate := func(window *account.QuotaWindow) account.RoutingCandidate {
		return account.RoutingCandidate{Credential: account.Credential{ID: 1}, QuotaWindow: window}
	}
	fresh := now.Add(-time.Hour)
	stale := now.Add(-72 * time.Hour)
	cases := []struct {
		name       string
		window     *account.QuotaWindow
		staleAfter time.Duration
		want       int
	}{
		{name: "fresh available", window: window(&fresh, 1), staleAfter: 24 * time.Hour, want: quotaRankAvailable},
		{name: "stale available degraded", window: window(&stale, 1), staleAfter: 24 * time.Hour, want: quotaRankUnknown},
		{name: "stale exhausted degraded", window: window(&stale, 0), staleAfter: 24 * time.Hour, want: quotaRankUnknown},
		{name: "exactly at boundary stays fresh", window: window(ptrTime(now.Add(-24 * time.Hour)), 1), staleAfter: 24 * time.Hour, want: quotaRankAvailable},
		{name: "nil synced_at never degrades", window: window(nil, 1), staleAfter: 24 * time.Hour, want: quotaRankAvailable},
		{name: "zero staleAfter disables check", window: window(&stale, 1), staleAfter: 0, want: quotaRankAvailable},
	}
	for _, testCase := range cases {
		if got := candidateQuotaRank(candidate(testCase.window), now, testCase.staleAfter); got != testCase.want {
			t.Fatalf("%s: rank = %d, want %d", testCase.name, got, testCase.want)
		}
	}
}

// TestSelectorQuotaStaleAfterDefaults 覆盖配置读取：零值回退内置默认，热更新生效。
func TestSelectorQuotaStaleAfterDefaults(t *testing.T) {
	selector := NewSelector(nil, nil, nil, nil, time.Hour, time.Second, time.Minute)
	if got := selector.selectorQuotaStaleAfter(); got != quotaSnapshotStaleAfter {
		t.Fatalf("default staleAfter = %v, want %v", got, quotaSnapshotStaleAfter)
	}
	selector.UpdateQuotaStaleAfter(2 * time.Hour)
	if got := selector.selectorQuotaStaleAfter(); got != 2*time.Hour {
		t.Fatalf("updated staleAfter = %v, want 2h", got)
	}
	selector.UpdateQuotaStaleAfter(0)
	if got := selector.selectorQuotaStaleAfter(); got != quotaSnapshotStaleAfter {
		t.Fatalf("zero staleAfter should fall back to default, got %v", got)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

// TestVideoQuotaSnapshotUntrusted 覆盖 lease 层可信度判定：探测 lease、
// 跨周期快照、超龄快照都不可直接烧 video_create。
func TestVideoQuotaSnapshotUntrusted(t *testing.T) {
	now := time.Now().UTC()
	candidate := func(window *account.QuotaWindow) *accountLease {
		selected := account.RoutingCandidate{Credential: account.Credential{ID: 7}, QuotaWindow: window}
		return &accountLease{QuotaMode: account.QuotaModeWebVideo720p, routingCandidate: &selected}
	}
	future := now.Add(time.Hour)
	fresh := now.Add(-time.Hour)

	cases := []struct {
		name  string
		lease *accountLease
		want  bool
	}{
		{
			name:  "probe lease trusted check irrelevant but window stale",
			lease: candidate(&account.QuotaWindow{Source: account.QuotaSourceUpstream, Remaining: 1, ResetAt: &future, SyncedAt: ptrTime(now.Add(-72 * time.Hour))}),
			want:  true,
		},
		{
			name:  "fresh window trusted",
			lease: candidate(&account.QuotaWindow{Source: account.QuotaSourceUpstream, Remaining: 1, ResetAt: &future, SyncedAt: &fresh}),
			want:  false,
		},
		{
			name:  "rolled-over window untrusted",
			lease: candidate(&account.QuotaWindow{Source: account.QuotaSourceUpstream, Remaining: 1, ResetAt: ptrTime(now.Add(-time.Minute)), SyncedAt: &fresh}),
			want:  true,
		},
		{
			name:  "no candidate context trusted (hard filter covers it)",
			lease: &accountLease{QuotaMode: account.QuotaModeWebVideo720p},
			want:  false,
		},
		{
			name:  "empty quota mode skips check",
			lease: candidate(&account.QuotaWindow{Source: account.QuotaSourceUpstream, Remaining: 1, ResetAt: &future, SyncedAt: ptrTime(now.Add(-72 * time.Hour))}),
			want:  true,
		},
	}
	// 修正最后一个用例的期望：QuotaMode 为空时函数直接返回 false。
	cases[len(cases)-1].want = false
	cases[len(cases)-1].lease = func() *accountLease {
		lease := candidate(&account.QuotaWindow{Source: account.QuotaSourceUpstream, Remaining: 1, ResetAt: &future, SyncedAt: ptrTime(now.Add(-72 * time.Hour))})
		lease.QuotaMode = ""
		return lease
	}()

	for _, testCase := range cases {
		if got := videoQuotaSnapshotUntrusted(testCase.lease); got != testCase.want {
			t.Fatalf("%s: untrusted = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}
