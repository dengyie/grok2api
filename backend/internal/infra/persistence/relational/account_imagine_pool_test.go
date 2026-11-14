package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// createImaginePoolAccount 建一个可调度的 Web 账号并写一个产品额度窗口。
func createImaginePoolAccount(t *testing.T, ctx context.Context, accounts *AccountRepository, sourceKey string) uint64 {
	t.Helper()
	credential := createEgressOperationsProviderAccount(t, ctx, accounts, account.ProviderWeb, sourceKey)
	return credential.ID
}

func saveImagineWindow(t *testing.T, ctx context.Context, accounts *AccountRepository, accountID uint64, mode string, remaining int, syncedAt, resetAt time.Time) {
	t.Helper()
	synced := syncedAt
	reset := resetAt
	source := account.QuotaSourceUpstream
	err := accounts.ReplaceQuotaWindows(ctx, accountID, account.WebTierBasic, syncedAt, []account.QuotaWindow{{
		AccountID: accountID, Mode: mode, Remaining: remaining, Total: 10,
		ResetAt: &reset, SyncedAt: &synced, Source: source, UpdatedAt: syncedAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSummarizeWebImaginePoolClassifiesAccounts(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "imagine-pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	now := time.Now().UTC()
	staleAfter := 24 * time.Hour

	// fresh available: video_720p remaining=2, synced 1h ago
	freshID := createImaginePoolAccount(t, ctx, accounts, "pool-fresh")
	saveImagineWindow(t, ctx, accounts, freshID, account.QuotaModeWebVideo720p, 2, now.Add(-time.Hour), now.Add(12*time.Hour))
	// fresh exhausted: remaining=0 with future reset
	exhaustedID := createImaginePoolAccount(t, ctx, accounts, "pool-exhausted")
	saveImagineWindow(t, ctx, accounts, exhaustedID, account.QuotaModeWebVideo720p, 0, now.Add(-time.Hour), now.Add(6*time.Hour))
	// stale snapshot: remaining=1 but synced 3 days ago -> unknown
	staleID := createImaginePoolAccount(t, ctx, accounts, "pool-stale")
	saveImagineWindow(t, ctx, accounts, staleID, account.QuotaModeWebVideo720p, 1, now.Add(-72*time.Hour), now.Add(6*time.Hour))
	// weekly fallback counts as available even without product window
	weeklyID := createImaginePoolAccount(t, ctx, accounts, "pool-weekly")
	saveImagineWindow(t, ctx, accounts, weeklyID, "weekly", 50, now.Add(-2*time.Hour), now.Add(5*24*time.Hour))
	// disabled account with fresh window must not count
	disabledID := createImaginePoolAccount(t, ctx, accounts, "pool-disabled")
	saveImagineWindow(t, ctx, accounts, disabledID, account.QuotaModeWebVideo720p, 5, now.Add(-time.Hour), now.Add(12*time.Hour))
	if err := accounts.Delete(ctx, disabledID); err != nil {
		t.Fatal(err)
	}

	stats, err := accounts.SummarizeWebImaginePool(ctx, staleAfter, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FreshAvailable != 2 {
		t.Fatalf("FreshAvailable = %d, want 2 (fresh + weekly)", stats.FreshAvailable)
	}
	if stats.ExhaustedKnown != 1 {
		t.Fatalf("ExhaustedKnown = %d, want 1", stats.ExhaustedKnown)
	}
	if stats.Unknown != 1 {
		t.Fatalf("Unknown = %d, want 1 (stale snapshot)", stats.Unknown)
	}
	if stats.EarliestReset == nil || !stats.EarliestReset.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("EarliestReset = %v, want exhausted account reset", stats.EarliestReset)
	}
}

func TestSummarizeWebImaginePoolAllExhaustedYieldsFailFast(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "imagine-pool-zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	now := time.Now().UTC()
	one := createImaginePoolAccount(t, ctx, accounts, "pool-zero-1")
	two := createImaginePoolAccount(t, ctx, accounts, "pool-zero-2")
	saveImagineWindow(t, ctx, accounts, one, account.QuotaModeWebVideo720p, 0, now.Add(-time.Hour), now.Add(3*time.Hour))
	saveImagineWindow(t, ctx, accounts, two, account.QuotaModeWebVideo720p, 0, now.Add(-2*time.Hour), now.Add(8*time.Hour))

	stats, err := accounts.SummarizeWebImaginePool(ctx, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FreshAvailable != 0 || stats.Unknown != 0 || stats.ExhaustedKnown != 2 {
		t.Fatalf("stats = %#v, want 0 available / 0 unknown / 2 exhausted", stats)
	}
	if stats.EarliestReset == nil || !stats.EarliestReset.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("EarliestReset = %v, want earliest of exhausted windows", stats.EarliestReset)
	}
}

func TestSummarizeWebImaginePoolPastResetWindowIsUnknown(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "imagine-pool-past.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := NewAccountRepository(database)
	now := time.Now().UTC()
	// remaining=0 with reset_at already past: old cycle, cannot prove exhaustion now
	id := createImaginePoolAccount(t, ctx, accounts, "pool-past")
	saveImagineWindow(t, ctx, accounts, id, account.QuotaModeWebVideo720p, 0, now.Add(-30*time.Hour), now.Add(-time.Hour))

	stats, err := accounts.SummarizeWebImaginePool(ctx, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Unknown != 1 || stats.ExhaustedKnown != 0 {
		t.Fatalf("stats = %#v, want past-reset window treated as unknown", stats)
	}
	if stats.EarliestReset != nil {
		t.Fatalf("EarliestReset = %v, want nil (no trusted exhaustion)", stats.EarliestReset)
	}
}

