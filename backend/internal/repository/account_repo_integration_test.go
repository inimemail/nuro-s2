//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

type AccountRepoSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *accountRepository
}

type schedulerCacheRecorder struct {
	setAccounts     []*service.Account
	setAccountHasTx []bool
	deleteIDs       []int64
	accounts        map[int64]*service.Account
}

func (s *schedulerCacheRecorder) GetSnapshot(ctx context.Context, bucket service.SchedulerBucket) ([]*service.Account, bool, error) {
	return nil, false, nil
}

func (s *schedulerCacheRecorder) SetSnapshot(ctx context.Context, bucket service.SchedulerBucket, accounts []service.Account) error {
	return nil
}

func (s *schedulerCacheRecorder) GetAccount(ctx context.Context, accountID int64) (*service.Account, error) {
	if s.accounts == nil {
		return nil, nil
	}
	return s.accounts[accountID], nil
}

func (s *schedulerCacheRecorder) SetAccount(ctx context.Context, account *service.Account) error {
	s.setAccounts = append(s.setAccounts, account)
	s.setAccountHasTx = append(s.setAccountHasTx, dbent.TxFromContext(ctx) != nil)
	if s.accounts == nil {
		s.accounts = make(map[int64]*service.Account)
	}
	if account != nil {
		s.accounts[account.ID] = account
	}
	return nil
}

func (s *schedulerCacheRecorder) DeleteAccount(ctx context.Context, accountID int64) error {
	s.deleteIDs = append(s.deleteIDs, accountID)
	if s.accounts != nil {
		delete(s.accounts, accountID)
	}
	return nil
}

func (s *schedulerCacheRecorder) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}

func (s *schedulerCacheRecorder) TryLockBucket(ctx context.Context, bucket service.SchedulerBucket, ttl time.Duration) (bool, error) {
	return true, nil
}

func (s *schedulerCacheRecorder) UnlockBucket(ctx context.Context, bucket service.SchedulerBucket) error {
	return nil
}

func (s *schedulerCacheRecorder) ListBuckets(ctx context.Context) ([]service.SchedulerBucket, error) {
	return nil, nil
}

func (s *schedulerCacheRecorder) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (s *schedulerCacheRecorder) SetOutboxWatermark(ctx context.Context, id int64) error {
	return nil
}

func (s *AccountRepoSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.repo = newAccountRepositoryWithSQL(s.client, tx, nil)
}

func TestAccountRepoSuite(t *testing.T) {
	suite.Run(t, new(AccountRepoSuite))
}

func TestUpdateAccountWithGroupConfigCommitsFinalStateAndRefreshesOnce(t *testing.T) {
	ctx := context.Background()
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, cache)
	suffix := time.Now().UnixNano()
	group1Limit := 1.0
	group2Limit := 2.5
	group1 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("atomic-old-%d", suffix), Platform: service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &group1Limit,
	})
	group2 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("atomic-new-%d", suffix), Platform: service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &group2Limit,
	})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("atomic-account-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", group1.ID, group2.ID)
	})

	requireNoError := func(err error) {
		if err != nil {
			t.Fatalf("fixture setup: %v", err)
		}
	}
	requireNoError(repo.BindGroups(ctx, account.ID, []int64{group1.ID}))
	requireNoError(repo.UpdateUpstreamBillingGuardGroupLimits(ctx, account.ID, map[int64]float64{group1.ID: 1}))
	_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
	cache.setAccounts = nil

	account, err := repo.GetByID(ctx, account.ID)
	requireNoError(err)
	account.Name = "atomic-committed"
	groupIDs := []int64{group2.ID}
	limits := map[int64]float64{group2.ID: 2.5}
	requireNoError(repo.UpdateAccountWithGroupConfig(ctx, account, &groupIDs, &limits, false))

	updated, err := repo.GetByID(ctx, account.ID)
	requireNoError(err)
	if updated.Name != "atomic-committed" {
		t.Fatalf("expected committed name, got %q", updated.Name)
	}
	if len(updated.AccountGroups) != 1 || updated.AccountGroups[0].GroupID != group2.ID {
		t.Fatalf("expected only group %d, got %+v", group2.ID, updated.AccountGroups)
	}
	if got := updated.AccountGroups[0].UpstreamBillingGuardMaxMultiplier; got == nil || *got != 2.5 {
		t.Fatalf("expected committed guard limit 2.5, got %v", got)
	}
	if len(cache.setAccounts) != 1 {
		t.Fatalf("expected one post-commit scheduler refresh, got %d", len(cache.setAccounts))
	}
	if len(cache.setAccounts[0].AccountGroups) != 1 {
		t.Fatalf("scheduler cache received incomplete group state: %+v", cache.setAccounts[0].AccountGroups)
	}
	if got := cache.setAccounts[0].AccountGroups[0].UpstreamBillingGuardMaxMultiplier; got == nil || *got != 2.5 {
		t.Fatalf("scheduler cache did not receive final guard state: %v", got)
	}
	var outboxCount int
	requireNoError(integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&outboxCount))
	if outboxCount == 0 {
		t.Fatal("expected committed scheduler outbox events")
	}
}

func TestUpdateAccountWithGroupConfigRollsBackEverySideEffect(t *testing.T) {
	ctx := context.Background()
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, cache)
	suffix := time.Now().UnixNano()
	group1Limit := 1.0
	group1 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("rollback-old-%d", suffix), Platform: service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &group1Limit,
	})
	group2 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("rollback-new-%d", suffix), Platform: service.PlatformOpenAI,
	})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("rollback-account-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true},
	})
	originalName := account.Name
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", group1.ID, group2.ID)
	})

	if err := repo.BindGroups(ctx, account.ID, []int64{group1.ID}); err != nil {
		t.Fatalf("bind fixture group: %v", err)
	}
	if err := repo.UpdateUpstreamBillingGuardGroupLimits(ctx, account.ID, map[int64]float64{group1.ID: 1}); err != nil {
		t.Fatalf("set fixture guard: %v", err)
	}
	_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
	cache.setAccounts = nil

	account, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("load fixture account: %v", err)
	}
	account.Name = "must-rollback"
	groupIDs := []int64{group2.ID}
	// group1 is deliberately absent after rebinding, forcing the last stage to fail.
	limits := map[int64]float64{group1.ID: 2}
	if err := repo.UpdateAccountWithGroupConfig(ctx, account, &groupIDs, &limits, false); err == nil {
		t.Fatal("expected missing guard binding to roll back the transaction")
	}

	updated, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload rolled-back account: %v", err)
	}
	if updated.Name != originalName {
		t.Fatalf("account row was partially committed: got %q want %q", updated.Name, originalName)
	}
	if len(updated.AccountGroups) != 1 || updated.AccountGroups[0].GroupID != group1.ID {
		t.Fatalf("group bindings were partially committed: %+v", updated.AccountGroups)
	}
	if got := updated.AccountGroups[0].UpstreamBillingGuardMaxMultiplier; got == nil || *got != 1 {
		t.Fatalf("guard limit was partially committed: %v", got)
	}
	if len(cache.setAccounts) != 0 {
		t.Fatalf("rolled-back state leaked to scheduler cache: %d writes", len(cache.setAccounts))
	}
	var outboxCount int
	if err := integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count scheduler outbox: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("rolled-back state leaked to scheduler outbox: %d events", outboxCount)
	}
}

func TestUpdatePreservesProbeSnapshotWrittenWhileAccountRowIsBlocked(t *testing.T) {
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	suffix := time.Now().UnixNano()
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("probe-race-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			service.UpstreamBillingProbeEnabledExtraKey: true,
			service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "old"},
		},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
	})

	stale, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("load stale account: %v", err)
	}
	stale.Name = "updated-with-blocked-probe"

	lockTx, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback() }()
	var blockerPID int
	if err := lockTx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatalf("read blocker pid: %v", err)
	}
	if _, err := lockTx.ExecContext(ctx, `
		UPDATE accounts
		SET extra = jsonb_set(extra, '{upstream_billing_probe}', '{"status":"new"}'::jsonb)
		WHERE id = $1
	`, account.ID); err != nil {
		t.Fatalf("stage concurrent probe snapshot: %v", err)
	}

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- repo.Update(context.Background(), stale)
	}()

	blocked := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := integrationDB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE $1 = ANY(pg_blocking_pids(pid))
			)
		`, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("inspect blocked account update: %v", err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		_ = lockTx.Rollback()
		t.Fatal("account update never blocked behind the concurrent probe write")
	}
	if err := lockTx.Commit(); err != nil {
		t.Fatalf("commit concurrent probe snapshot: %v", err)
	}

	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("update account after probe commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("account update did not resume after probe commit")
	}

	updated, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload account after concurrent update: %v", err)
	}
	snapshot, ok := updated.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)
	if !ok || snapshot["status"] != "new" {
		t.Fatalf("concurrent probe snapshot was overwritten: %#v", updated.Extra[service.UpstreamBillingProbeExtraKey])
	}
}

func TestUpdateAccountWithGroupConfigOuterRollbackIncludesShadowProxy(t *testing.T) {
	ctx := context.Background()
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, cache)
	suffix := time.Now().UnixNano()
	proxy1 := mustCreateProxy(t, integrationEntClient, &service.Proxy{Name: fmt.Sprintf("proxy-old-%d", suffix)})
	proxy2 := mustCreateProxy(t, integrationEntClient, &service.Proxy{Name: fmt.Sprintf("proxy-new-%d", suffix)})
	group1 := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("shadow-old-%d", suffix)})
	group2 := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("shadow-new-%d", suffix)})
	parent := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:        fmt.Sprintf("shadow-parent-%d", suffix),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		ProxyID:     &proxy1.ID,
		Extra:       map[string]any{},
		Status:      service.StatusActive,
		Schedulable: true,
	})
	shadow := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:            fmt.Sprintf("shadow-child-%d", suffix),
		Platform:        service.PlatformOpenAI,
		Type:            service.AccountTypeOAuth,
		ProxyID:         &proxy1.ID,
		ParentAccountID: &parent.ID,
		QuotaDimension:  service.QuotaDimensionSpark,
		Extra:           map[string]any{},
		Status:          service.StatusActive,
		Schedulable:     true,
	})
	originalName := parent.Name
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id IN ($1, $2)", parent.ID, shadow.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id IN ($1, $2)", parent.ID, shadow.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", group1.ID, group2.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM proxies WHERE id IN ($1, $2)", proxy1.ID, proxy2.ID)
	})
	if err := repo.BindGroups(ctx, parent.ID, []int64{group1.ID}); err != nil {
		t.Fatalf("bind parent fixture group: %v", err)
	}
	_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id IN ($1, $2)", parent.ID, shadow.ID)
	cache.setAccounts = nil

	parent, err := repo.GetByID(ctx, parent.ID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Name = "parent-must-rollback"
	parent.ProxyID = &proxy2.ID
	groupIDs := []int64{group2.ID}
	tx, err := integrationEntClient.Tx(ctx)
	if err != nil {
		t.Fatalf("begin outer rollback transaction: %v", err)
	}
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := repo.UpdateAccountWithGroupConfig(txCtx, parent, &groupIDs, nil, true); err != nil {
		_ = tx.Rollback()
		t.Fatalf("update parent and shadow in outer transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback parent and shadow transaction: %v", err)
	}

	rolledBackParent, err := repo.GetByID(ctx, parent.ID)
	if err != nil {
		t.Fatalf("reload rolled-back parent: %v", err)
	}
	rolledBackShadow, err := repo.GetByID(ctx, shadow.ID)
	if err != nil {
		t.Fatalf("reload rolled-back shadow: %v", err)
	}
	if rolledBackParent.Name != originalName || rolledBackParent.ProxyID == nil || *rolledBackParent.ProxyID != proxy1.ID {
		t.Fatalf("parent escaped rollback: name=%q proxy=%v", rolledBackParent.Name, rolledBackParent.ProxyID)
	}
	if len(rolledBackParent.GroupIDs) != 1 || rolledBackParent.GroupIDs[0] != group1.ID {
		t.Fatalf("parent groups escaped rollback: %v", rolledBackParent.GroupIDs)
	}
	if rolledBackShadow.ProxyID == nil || *rolledBackShadow.ProxyID != proxy1.ID {
		t.Fatalf("shadow proxy escaped rollback: %v", rolledBackShadow.ProxyID)
	}
	if len(cache.setAccounts) != 0 {
		t.Fatalf("outer rollback leaked scheduler cache writes: %d", len(cache.setAccounts))
	}
	var outboxCount int
	if err := integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id IN ($1, $2)", parent.ID, shadow.ID).Scan(&outboxCount); err != nil {
		t.Fatalf("count rolled-back outbox events: %v", err)
	}
	if outboxCount != 0 {
		t.Fatalf("outer rollback leaked scheduler outbox events: %d", outboxCount)
	}
}

func TestUpdateAccountWithGroupConfigOuterCommitRefreshesFinalParentAndShadow(t *testing.T) {
	ctx := context.Background()
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, cache)
	suffix := time.Now().UnixNano()
	proxy1 := mustCreateProxy(t, integrationEntClient, &service.Proxy{Name: fmt.Sprintf("commit-proxy-old-%d", suffix)})
	proxy2 := mustCreateProxy(t, integrationEntClient, &service.Proxy{Name: fmt.Sprintf("commit-proxy-new-%d", suffix)})
	group1Limit := 1.0
	group2Limit := 2.5
	group1 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("commit-group-old-%d", suffix), Platform: service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &group1Limit,
	})
	group2 := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("commit-group-new-%d", suffix), Platform: service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &group2Limit,
	})
	parent := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:        fmt.Sprintf("commit-parent-%d", suffix),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		ProxyID:     &proxy1.ID,
		Extra:       map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true},
		Status:      service.StatusActive,
		Schedulable: true,
	})
	shadow := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:            fmt.Sprintf("commit-shadow-%d", suffix),
		Platform:        service.PlatformOpenAI,
		Type:            service.AccountTypeAPIKey,
		ProxyID:         &proxy1.ID,
		ParentAccountID: &parent.ID,
		QuotaDimension:  service.QuotaDimensionSpark,
		Extra:           map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true},
		Status:          service.StatusActive,
		Schedulable:     true,
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id IN ($1, $2)", parent.ID, shadow.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id IN ($1, $2)", parent.ID, shadow.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", group1.ID, group2.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM proxies WHERE id IN ($1, $2)", proxy1.ID, proxy2.ID)
	})
	if err := repo.BindGroups(ctx, parent.ID, []int64{group1.ID}); err != nil {
		t.Fatalf("bind parent fixture group: %v", err)
	}
	if err := repo.UpdateUpstreamBillingGuardGroupLimits(ctx, parent.ID, map[int64]float64{group1.ID: 1}); err != nil {
		t.Fatalf("set parent fixture guard: %v", err)
	}
	_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id IN ($1, $2)", parent.ID, shadow.ID)
	cache.setAccounts = nil
	cache.setAccountHasTx = nil
	cache.accounts = nil

	parent, err := repo.GetByID(ctx, parent.ID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Name = "parent-committed"
	parent.ProxyID = &proxy2.ID
	groupIDs := []int64{group2.ID}
	limits := map[int64]float64{group2.ID: 2.5}
	tx, err := integrationEntClient.Tx(ctx)
	if err != nil {
		t.Fatalf("begin outer commit transaction: %v", err)
	}
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := repo.UpdateAccountWithGroupConfig(txCtx, parent, &groupIDs, &limits, true); err != nil {
		_ = tx.Rollback()
		t.Fatalf("update parent and shadow in outer transaction: %v", err)
	}
	if len(cache.setAccounts) != 0 {
		_ = tx.Rollback()
		t.Fatalf("uncommitted state leaked to scheduler cache: %d writes", len(cache.setAccounts))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit parent and shadow transaction: %v", err)
	}

	if len(cache.setAccounts) != 2 {
		t.Fatalf("expected one post-commit refresh per parent and shadow, got %d", len(cache.setAccounts))
	}
	for i, hasTx := range cache.setAccountHasTx {
		if hasTx {
			t.Fatalf("post-commit scheduler refresh %d retained a committed transaction in its context", i)
		}
	}
	cachedParent := cache.accounts[parent.ID]
	if cachedParent == nil {
		t.Fatal("parent was not refreshed after outer commit")
	}
	if cachedParent.Name != "parent-committed" || cachedParent.ProxyID == nil || *cachedParent.ProxyID != proxy2.ID {
		t.Fatalf("parent cache did not receive committed row state: name=%q proxy=%v", cachedParent.Name, cachedParent.ProxyID)
	}
	if len(cachedParent.AccountGroups) != 1 || cachedParent.AccountGroups[0].GroupID != group2.ID {
		t.Fatalf("parent cache did not receive committed groups: %+v", cachedParent.AccountGroups)
	}
	if got := cachedParent.AccountGroups[0].UpstreamBillingGuardMaxMultiplier; got == nil || *got != 2.5 {
		t.Fatalf("parent cache did not receive committed guard limit: %v", got)
	}
	cachedShadow := cache.accounts[shadow.ID]
	if cachedShadow == nil || cachedShadow.ProxyID == nil || *cachedShadow.ProxyID != proxy2.ID {
		t.Fatalf("shadow cache did not receive committed proxy: %+v", cachedShadow)
	}
}

func TestBindGroupsConcurrentReplacementsRemainLastWriterWins(t *testing.T) {
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	suffix := time.Now().UnixNano()
	oldGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("bind-race-old-%d", suffix)})
	firstGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("bind-race-first-%d", suffix)})
	secondGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("bind-race-second-%d", suffix)})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("bind-race-account-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2, $3)", oldGroup.ID, firstGroup.ID, secondGroup.ID)
	})
	if err := repo.BindGroups(ctx, account.ID, []int64{oldGroup.ID}); err != nil {
		t.Fatalf("bind initial group: %v", err)
	}

	blocker, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin account-group blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatalf("read account-group blocker pid: %v", err)
	}
	var lockedGroupID int64
	if err := blocker.QueryRowContext(ctx, `
		SELECT group_id
		FROM account_groups
		WHERE account_id = $1 AND group_id = $2
		FOR UPDATE
	`, account.ID, oldGroup.ID).Scan(&lockedGroupID); err != nil {
		t.Fatalf("lock initial account-group row: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- repo.BindGroups(context.Background(), account.ID, []int64{firstGroup.ID})
	}()
	if !waitForPostgresBlocker(t, blockerPID, 1, 5*time.Second) {
		_ = blocker.Rollback()
		t.Fatal("first group replacement did not block on the fixture row")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- repo.BindGroups(context.Background(), account.ID, []int64{secondGroup.ID})
	}()
	if !waitForPostgresBlockedSessions(t, 2, 5*time.Second) {
		_ = blocker.Rollback()
		t.Fatal("concurrent group replacements did not both reach their serialization locks")
	}
	if err := blocker.Commit(); err != nil {
		t.Fatalf("release account-group blocker: %v", err)
	}

	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s group replacement failed: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s group replacement did not finish", name)
		}
	}

	updated, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload concurrently rebound account: %v", err)
	}
	if len(updated.GroupIDs) != 1 || updated.GroupIDs[0] != secondGroup.ID {
		t.Fatalf("concurrent replacements left merged or stale bindings: got %v want [%d]", updated.GroupIDs, secondGroup.ID)
	}
}

func TestBindGroupsAndGroupDeleteAvoidGroupBindingDeadlock(t *testing.T) {
	ctx := context.Background()
	accountRepo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	groupRepo := newGroupRepositoryWithSQL(integrationEntClient, integrationDB)
	suffix := time.Now().UnixNano()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("bind-delete-race-%d", suffix)})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("bind-delete-account-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1 OR group_id = $2", account.ID, group.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id = $1", group.ID)
	})
	if err := accountRepo.BindGroups(ctx, account.ID, []int64{group.ID}); err != nil {
		t.Fatalf("bind fixture group: %v", err)
	}

	blocker, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin binding blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatalf("read binding blocker pid: %v", err)
	}
	var lockedGroupID int64
	if err := blocker.QueryRowContext(ctx, `
		SELECT group_id
		FROM account_groups
		WHERE account_id = $1 AND group_id = $2
		FOR UPDATE
	`, account.ID, group.ID).Scan(&lockedGroupID); err != nil {
		t.Fatalf("lock binding row: %v", err)
	}

	bindDone := make(chan error, 1)
	go func() {
		bindDone <- accountRepo.BindGroups(context.Background(), account.ID, []int64{group.ID})
	}()
	if !waitForPostgresBlocker(t, blockerPID, 1, 5*time.Second) {
		_ = blocker.Rollback()
		t.Fatal("binding replacement did not block on the fixture row")
	}

	deleteDone := make(chan error, 1)
	go func() {
		_, deleteErr := groupRepo.DeleteCascade(context.Background(), group.ID)
		deleteDone <- deleteErr
	}()
	if !waitForPostgresBlockedSessions(t, 2, 5*time.Second) {
		_ = blocker.Rollback()
		t.Fatal("binding replacement and group delete did not reach the expected locks")
	}
	if err := blocker.Commit(); err != nil {
		t.Fatalf("release binding blocker: %v", err)
	}

	for name, done := range map[string]<-chan error{"bind": bindDone, "delete": deleteDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s operation failed after lock serialization: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s operation did not finish", name)
		}
	}

	updated, err := accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		t.Fatalf("reload account after group delete: %v", err)
	}
	if len(updated.GroupIDs) != 0 {
		t.Fatalf("deleted group remained bound to account: %v", updated.GroupIDs)
	}
	var activeGroupCount int
	if err := integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups WHERE id = $1 AND deleted_at IS NULL", group.ID).Scan(&activeGroupCount); err != nil {
		t.Fatalf("verify group soft delete: %v", err)
	}
	if activeGroupCount != 0 {
		t.Fatalf("group was not deleted: active rows=%d", activeGroupCount)
	}
}

func TestDeleteBuildsSchedulerPayloadFromBindingsAfterAccountLock(t *testing.T) {
	ctx := context.Background()
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	suffix := time.Now().UnixNano()
	oldGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("delete-payload-old-%d", suffix)})
	newGroup := mustCreateGroup(t, integrationEntClient, &service.Group{Name: fmt.Sprintf("delete-payload-new-%d", suffix)})
	account := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name:     fmt.Sprintf("delete-payload-account-%d", suffix),
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{},
	})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id IN ($1, $2)", oldGroup.ID, newGroup.ID)
	})
	if err := repo.BindGroups(ctx, account.ID, []int64{oldGroup.ID}); err != nil {
		t.Fatalf("bind initial delete payload group: %v", err)
	}
	_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)

	blocker, err := integrationDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin account delete blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	if err := blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatalf("read account delete blocker pid: %v", err)
	}
	var lockedAccountID int64
	if err := blocker.QueryRowContext(ctx, `
		SELECT id
		FROM accounts
		WHERE id = $1
		FOR UPDATE
	`, account.ID).Scan(&lockedAccountID); err != nil {
		t.Fatalf("lock account before delete: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- repo.Delete(context.Background(), account.ID)
	}()
	if !waitForPostgresBlocker(t, blockerPID, 1, 5*time.Second) {
		_ = blocker.Rollback()
		t.Fatal("account delete did not block on the account row")
	}
	if _, err := blocker.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", account.ID); err != nil {
		t.Fatalf("replace binding while delete waits: %v", err)
	}
	if _, err := blocker.ExecContext(ctx, `
		INSERT INTO account_groups (account_id, group_id, priority, created_at)
		VALUES ($1, $2, 0, NOW())
	`, account.ID, newGroup.ID); err != nil {
		t.Fatalf("insert replacement binding while delete waits: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatalf("commit replacement binding: %v", err)
	}
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("delete account after binding replacement: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("account delete did not finish")
	}

	var payloadUsesNewGroup bool
	if err := integrationDB.QueryRowContext(ctx, `
		SELECT payload->'group_ids' = to_jsonb(ARRAY[$2]::bigint[])
		FROM scheduler_outbox
		WHERE event_type = $1 AND account_id = $3
		ORDER BY id DESC
		LIMIT 1
	`, service.SchedulerOutboxEventAccountChanged, newGroup.ID, account.ID).Scan(&payloadUsesNewGroup); err != nil {
		t.Fatalf("read account delete scheduler payload: %v", err)
	}
	if !payloadUsesNewGroup {
		t.Fatalf("account delete scheduler payload did not use post-lock group %d", newGroup.ID)
	}
}

func waitForPostgresBlocker(t *testing.T, blockerPID, minimum int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var blocked int
		if err := integrationDB.QueryRowContext(context.Background(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))
		`, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("inspect postgres blocker: %v", err)
		}
		if blocked >= minimum {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForPostgresBlockedSessions(t *testing.T, minimum int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var blocked int
		if err := integrationDB.QueryRowContext(context.Background(), `
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
				AND pid <> pg_backend_pid()
				AND cardinality(pg_blocking_pids(pid)) > 0
		`).Scan(&blocked); err != nil {
			t.Fatalf("inspect blocked postgres sessions: %v", err)
		}
		if blocked >= minimum {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --- Create / GetByID / Update / Delete ---

func (s *AccountRepoSuite) TestCreate() {
	account := &service.Account{
		Name:        "test-create",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{},
		Extra:       map[string]any{},
		Concurrency: 3,
		Priority:    50,
		Schedulable: true,
	}

	err := s.repo.Create(s.ctx, account)
	s.Require().NoError(err, "Create")
	s.Require().NotZero(account.ID, "expected ID to be set")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("test-create", got.Name)
}

func (s *AccountRepoSuite) TestGetByID_NotFound() {
	_, err := s.repo.GetByID(s.ctx, 999999)
	s.Require().Error(err, "expected error for non-existent ID")
}

func (s *AccountRepoSuite) TestUpdate() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "original"})

	account.Name = "updated"
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID after update")
	s.Require().Equal("updated", got.Name)
}

func (s *AccountRepoSuite) TestUpdatePreservesConcurrentUpstreamBillingProbeSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "probe-snapshot-update-race",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			service.UpstreamBillingProbeEnabledExtraKey: true,
			service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "old"},
		},
	})
	stale, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	_, err = s.repo.sql.ExecContext(s.ctx, `
		UPDATE accounts
		SET extra = jsonb_set(extra, '{upstream_billing_probe}', '{"status":"new"}'::jsonb)
		WHERE id = $1
	`, account.ID)
	s.Require().NoError(err)

	stale.Name = "updated-with-current-probe"
	s.Require().NoError(s.repo.Update(s.ctx, stale))
	updated, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	snapshot, ok := updated.Extra[service.UpstreamBillingProbeExtraKey].(map[string]any)
	s.Require().True(ok)
	s.Require().Equal("new", snapshot["status"])
}

func (s *AccountRepoSuite) TestUpdate_ClearsBillingGuardWhenAccountIdentityLeavesAPIKey() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "guard-identity-transition",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
	})
	observed := 3.0
	_, err := s.repo.sql.ExecContext(s.ctx, `
		UPDATE accounts
		SET upstream_billing_guard_enabled = TRUE,
		    upstream_billing_guard_max_multiplier = 2.0,
		    upstream_billing_guard_blocked = TRUE,
		    upstream_billing_guard_observed_multiplier = $2,
		    upstream_billing_guard_evaluated_at = NOW()
		WHERE id = $1
	`, account.ID, observed)
	s.Require().NoError(err)

	account, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	account.Type = service.AccountTypeOAuth
	s.Require().NoError(s.repo.Update(s.ctx, account))

	updated, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().False(updated.UpstreamBillingGuardEnabled)
	s.Require().False(updated.UpstreamBillingGuardBlocked)
	s.Require().Nil(updated.UpstreamBillingGuardObservedMultiplier)
	s.Require().Nil(updated.UpstreamBillingGuardEvaluatedAt)
	s.Require().Equal(2.0, updated.UpstreamBillingGuardMaxMultiplier)
}

func (s *AccountRepoSuite) TestUpdate_SyncSchedulerSnapshotOnDisabled() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "sync-update", Status: service.StatusActive, Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	account.Status = service.StatusDisabled
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusDisabled, cacheRecorder.setAccounts[0].Status)
}

func (s *AccountRepoSuite) TestUpdate_SyncSchedulerSnapshotOnCredentialsChange() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "sync-credentials-update",
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5": "gpt-5.1",
			},
		},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	account.Credentials = map[string]any{
		"model_mapping": map[string]any{
			"gpt-5": "gpt-5.2",
		},
	}
	err := s.repo.Update(s.ctx, account)
	s.Require().NoError(err, "Update")

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	mapping, ok := cacheRecorder.setAccounts[0].Credentials["model_mapping"].(map[string]any)
	s.Require().True(ok)
	s.Require().Equal("gpt-5.2", mapping["gpt-5"])
}

func (s *AccountRepoSuite) TestDelete() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "to-delete"})

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete")

	_, err = s.repo.GetByID(s.ctx, account.ID)
	s.Require().Error(err, "expected error after delete")
}

func (s *AccountRepoSuite) TestDelete_RemovesSchedulerAccountSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "to-delete-cache"})
	cacheRecorder := &schedulerCacheRecorder{
		accounts: map[int64]*service.Account{
			account.ID: {
				ID:          account.ID,
				Name:        account.Name,
				Status:      service.StatusActive,
				Schedulable: true,
			},
		},
	}
	s.repo.schedulerCache = cacheRecorder

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete")

	s.Require().Equal([]int64{account.ID}, cacheRecorder.deleteIDs)
	s.Require().NotContains(cacheRecorder.accounts, account.ID)
}

func (s *AccountRepoSuite) TestDelete_WithGroupBindings() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-del"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-del"})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)

	err := s.repo.Delete(s.ctx, account.ID)
	s.Require().NoError(err, "Delete should cascade remove bindings")

	count, err := s.client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(account.ID)).Count(s.ctx)
	s.Require().NoError(err)
	s.Require().Zero(count, "expected bindings to be removed")
}

// --- List / ListWithFilters ---

func (s *AccountRepoSuite) TestList() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc1"})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc2"})

	accounts, page, err := s.repo.List(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10})
	s.Require().NoError(err, "List")
	s.Require().Len(accounts, 2)
	s.Require().Equal(int64(2), page.Total)
}

func (s *AccountRepoSuite) TestListWithFilters() {
	tests := []struct {
		name        string
		setup       func(client *dbent.Client)
		platform    string
		accType     string
		status      string
		search      string
		groupID     int64
		privacyMode string
		poolMode    string
		wantCount   int
		validate    func(accounts []service.Account)
	}{
		{
			name: "filter_by_platform",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic})
				mustCreateAccount(s.T(), client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI})
			},
			platform:  service.PlatformOpenAI,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.PlatformOpenAI, accounts[0].Platform)
			},
		},
		{
			name: "filter_by_type",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "t1", Type: service.AccountTypeOAuth})
				mustCreateAccount(s.T(), client, &service.Account{Name: "t2", Type: service.AccountTypeAPIKey})
			},
			accType:   service.AccountTypeAPIKey,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.AccountTypeAPIKey, accounts[0].Type)
			},
		},
		{
			name: "filter_by_status",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "s1", Status: service.StatusActive})
				mustCreateAccount(s.T(), client, &service.Account{Name: "s2", Status: service.StatusDisabled})
			},
			status:    service.StatusDisabled,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal(service.StatusDisabled, accounts[0].Status)
			},
		},
		{
			name: "filter_by_status_active_excludes_runtime_blocked_accounts",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "active-normal", Status: service.StatusActive})
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err := client.Account.UpdateOneID(rateLimited.ID).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    service.StatusActive,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-normal", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_unschedulable_excludes_rate_limited_and_temp_unschedulable",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "active-normal", Status: service.StatusActive, Schedulable: true})
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err := client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err = client.Account.UpdateOneID(rateLimited.ID).
					SetSchedulable(false).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetSchedulable(false).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "unschedulable",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-unsched", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_rate_limited_excludes_temp_unschedulable",
			setup: func(client *dbent.Client) {
				rateLimited := mustCreateAccount(s.T(), client, &service.Account{Name: "active-rate-limited", Status: service.StatusActive})
				err := client.Account.UpdateOneID(rateLimited.ID).
					SetRateLimitResetAt(time.Now().Add(10 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(tempUnsched.ID).
					SetRateLimitResetAt(time.Now().Add(20 * time.Minute)).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "rate_limited",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-rate-limited", accounts[0].Name)
			},
		},
		{
			name: "filter_by_status_temp_unschedulable_excludes_manually_unschedulable",
			setup: func(client *dbent.Client) {
				tempUnsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-temp-unsched", Status: service.StatusActive, Schedulable: true})
				err := client.Account.UpdateOneID(tempUnsched.ID).
					SetTempUnschedulableUntil(time.Now().Add(15 * time.Minute)).
					Exec(context.Background())
				s.Require().NoError(err)
				unsched := mustCreateAccount(s.T(), client, &service.Account{Name: "active-unsched", Status: service.StatusActive})
				err = client.Account.UpdateOneID(unsched.ID).
					SetSchedulable(false).
					Exec(context.Background())
				s.Require().NoError(err)
			},
			status:    "temp_unschedulable",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("active-temp-unsched", accounts[0].Name)
			},
		},
		{
			name: "filter_by_search",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "alpha-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "beta-account"})
			},
			search:    "alpha",
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Contains(accounts[0].Name, "alpha")
			},
		},
		{
			name: "filter_by_ungrouped",
			setup: func(client *dbent.Client) {
				group := mustCreateGroup(s.T(), client, &service.Group{Name: "g-ungrouped"})
				grouped := mustCreateAccount(s.T(), client, &service.Account{Name: "grouped-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "ungrouped-account"})
				mustBindAccountToGroup(s.T(), client, grouped.ID, group.ID, 1)
			},
			groupID:   service.AccountListGroupUngrouped,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("ungrouped-account", accounts[0].Name)
				s.Require().Empty(accounts[0].GroupIDs)
			},
		},
		{
			name: "filter_by_privacy_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-ok", Extra: map[string]any{"privacy_mode": service.PrivacyModeTrainingOff}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-fail", Extra: map[string]any{"privacy_mode": service.PrivacyModeFailed}})
			},
			privacyMode: service.PrivacyModeTrainingOff,
			wantCount:   1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("privacy-ok", accounts[0].Name)
			},
		},
		{
			name: "filter_by_privacy_mode_unset",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-unset", Extra: nil})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-empty", Extra: map[string]any{"privacy_mode": ""}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "privacy-set", Extra: map[string]any{"privacy_mode": service.PrivacyModeTrainingOff}})
			},
			privacyMode: service.AccountPrivacyModeUnsetFilter,
			wantCount:   2,
			validate: func(accounts []service.Account) {
				names := []string{accounts[0].Name, accounts[1].Name}
				s.ElementsMatch([]string{"privacy-unset", "privacy-empty"}, names)
			},
		},
		{
			name: "filter_by_non_pool_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "plain-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "pool-account", Credentials: map[string]any{"pool_mode": true}})
			},
			poolMode:  service.AccountPoolModeNonPoolFilter,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("plain-account", accounts[0].Name)
			},
		},
		{
			name: "filter_by_pool_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "plain-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "pool-account", Credentials: map[string]any{"pool_mode": true}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "image-pool-account", Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true}})
			},
			poolMode:  service.AccountPoolModePoolFilter,
			wantCount: 2,
			validate: func(accounts []service.Account) {
				names := []string{accounts[0].Name, accounts[1].Name}
				s.ElementsMatch([]string{"pool-account", "image-pool-account"}, names)
			},
		},
		{
			name: "filter_by_image_pool_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "plain-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "pool-account", Credentials: map[string]any{"pool_mode": true}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "image-pool-account", Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true}})
			},
			poolMode:  service.AccountPoolModeImagePoolFilter,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("image-pool-account", accounts[0].Name)
			},
		},
		{
			name: "filter_by_text_pool_mode",
			setup: func(client *dbent.Client) {
				mustCreateAccount(s.T(), client, &service.Account{Name: "plain-account"})
				mustCreateAccount(s.T(), client, &service.Account{Name: "pool-account", Credentials: map[string]any{"pool_mode": true}})
				mustCreateAccount(s.T(), client, &service.Account{Name: "image-pool-account", Credentials: map[string]any{"pool_mode": true, "image_pool_mode": true}})
			},
			poolMode:  service.AccountPoolModeTextPoolFilter,
			wantCount: 1,
			validate: func(accounts []service.Account) {
				s.Require().Equal("pool-account", accounts[0].Name)
			},
		},
	}

	for _, tt := range tests {
		s.Run(tt.name, func() {
			// 每个 case 重新获取隔离资源
			tx := testEntTx(s.T())
			client := tx.Client()
			repo := newAccountRepositoryWithSQL(client, tx, nil)
			ctx := context.Background()

			tt.setup(client)

			accounts, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, tt.platform, tt.accType, tt.status, tt.search, tt.groupID, tt.privacyMode, tt.poolMode)
			s.Require().NoError(err)
			s.Require().Len(accounts, tt.wantCount)
			if tt.validate != nil {
				tt.validate(accounts)
			}
		})
	}
}

// --- ListByGroup / ListActive / ListByPlatform ---

func (s *AccountRepoSuite) TestListByGroup() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-list"})
	acc1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Status: service.StatusActive})
	acc2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Status: service.StatusActive})
	mustBindAccountToGroup(s.T(), s.client, acc1.ID, group.ID, 2)
	mustBindAccountToGroup(s.T(), s.client, acc2.ID, group.ID, 1)

	accounts, err := s.repo.ListByGroup(s.ctx, group.ID)
	s.Require().NoError(err, "ListByGroup")
	s.Require().Len(accounts, 2)
	// Should be ordered by priority
	s.Require().Equal(acc2.ID, accounts[0].ID, "expected acc2 first (priority=1)")
}

func (s *AccountRepoSuite) TestListActive() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "active1", Status: service.StatusActive})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "inactive1", Status: service.StatusDisabled})

	accounts, err := s.repo.ListActive(s.ctx)
	s.Require().NoError(err, "ListActive")
	s.Require().Len(accounts, 1)
	s.Require().Equal("active1", accounts[0].Name)
}

func (s *AccountRepoSuite) TestListByPlatform() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "p1", Platform: service.PlatformAnthropic, Status: service.StatusActive})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "p2", Platform: service.PlatformOpenAI, Status: service.StatusActive})

	accounts, err := s.repo.ListByPlatform(s.ctx, service.PlatformAnthropic)
	s.Require().NoError(err, "ListByPlatform")
	s.Require().Len(accounts, 1)
	s.Require().Equal(service.PlatformAnthropic, accounts[0].Platform)
}

// --- Preload and VirtualFields ---

func (s *AccountRepoSuite) TestPreload_And_VirtualFields() {
	proxy := mustCreateProxy(s.T(), s.client, &service.Proxy{Name: "p1"})
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g1"})

	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:    "acc1",
		ProxyID: &proxy.ID,
	})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().NotNil(got.Proxy, "expected Proxy preload")
	s.Require().Equal(proxy.ID, got.Proxy.ID)
	s.Require().Len(got.GroupIDs, 1, "expected GroupIDs to be populated")
	s.Require().Equal(group.ID, got.GroupIDs[0])
	s.Require().Len(got.Groups, 1, "expected Groups to be populated")
	s.Require().Equal(group.ID, got.Groups[0].ID)

	accounts, page, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10}, "", "", "", "acc", 0, "", "")
	s.Require().NoError(err, "ListWithFilters")
	s.Require().Equal(int64(1), page.Total)
	s.Require().Len(accounts, 1)
	s.Require().NotNil(accounts[0].Proxy, "expected Proxy preload in list")
	s.Require().Equal(proxy.ID, accounts[0].Proxy.ID)
	s.Require().Len(accounts[0].GroupIDs, 1, "expected GroupIDs in list")
	s.Require().Equal(group.ID, accounts[0].GroupIDs[0])
}

// --- GroupBinding / AddToGroup / RemoveFromGroup / BindGroups / GetGroups ---

func (s *AccountRepoSuite) TestGroupBinding_And_BindGroups() {
	g1 := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g1"})
	g2 := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g2"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc"})

	s.Require().NoError(s.repo.AddToGroup(s.ctx, account.ID, g1.ID, 10), "AddToGroup")
	groups, err := s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups")
	s.Require().Len(groups, 1, "expected 1 group")
	s.Require().Equal(g1.ID, groups[0].ID)

	s.Require().NoError(s.repo.RemoveFromGroup(s.ctx, account.ID, g1.ID), "RemoveFromGroup")
	groups, err = s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups after remove")
	s.Require().Empty(groups, "expected 0 groups after remove")

	s.Require().NoError(s.repo.BindGroups(s.ctx, account.ID, []int64{g1.ID, g2.ID}), "BindGroups")
	groups, err = s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err, "GetGroups after bind")
	s.Require().Len(groups, 2, "expected 2 groups after bind")
}

func (s *AccountRepoSuite) TestRemoveFromGroup_SyncsSchedulerAccountSnapshot() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-remove-sync"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-remove-sync"})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.RemoveFromGroup(s.ctx, account.ID, group.ID))

	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Empty(cacheRecorder.setAccounts[0].GroupIDs)
}

func (s *AccountRepoSuite) TestBindGroups_EmptyList() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-empty"})
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-empty"})
	mustBindAccountToGroup(s.T(), s.client, account.ID, group.ID, 1)
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.BindGroups(s.ctx, account.ID, []int64{}), "BindGroups empty")

	groups, err := s.repo.GetGroups(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Empty(groups, "expected 0 groups after binding empty list")
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Empty(cacheRecorder.setAccounts[0].GroupIDs)

	var outboxCount int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1", []any{service.SchedulerOutboxEventAccountGroupsChanged}, &outboxCount)
	s.Require().NoError(err)
	s.Require().Equal(1, outboxCount)
}

// --- Schedulable ---

func (s *AccountRepoSuite) TestListSchedulable() {
	now := time.Now()
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sched"})

	okAcc := mustCreateAccount(s.T(), s.client, &service.Account{Name: "ok", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, okAcc.ID, group.ID, 1)

	future := now.Add(10 * time.Minute)
	overloaded := mustCreateAccount(s.T(), s.client, &service.Account{Name: "over", Schedulable: true, OverloadUntil: &future})
	mustBindAccountToGroup(s.T(), s.client, overloaded.ID, group.ID, 1)

	sched, err := s.repo.ListSchedulable(s.ctx)
	s.Require().NoError(err, "ListSchedulable")
	ids := idsOfAccounts(sched)
	s.Require().Contains(ids, okAcc.ID)
	s.Require().NotContains(ids, overloaded.ID)
}

func (s *AccountRepoSuite) TestListSchedulableCapacityUsesBindingGuardAndAllowsPendingProbe() {
	lowLimit := 1.0
	highLimit := 3.0
	lowGroup := mustCreateGroup(s.T(), s.client, &service.Group{
		Name:                              "guard-low",
		Platform:                          service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &lowLimit,
	})
	highGroup := mustCreateGroup(s.T(), s.client, &service.Group{
		Name:                              "guard-high",
		Platform:                          service.PlatformOpenAI,
		UpstreamBillingGuardMaxMultiplier: &highLimit,
	})
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:                        "group-guard-capacity",
		Platform:                    service.PlatformOpenAI,
		Type:                        service.AccountTypeAPIKey,
		Extra:                       map[string]any{service.UpstreamBillingProbeEnabledExtraKey: true},
		UpstreamBillingGuardEnabled: true,
	})
	s.Require().NoError(s.repo.BindGroups(s.ctx, account.ID, []int64{lowGroup.ID, highGroup.ID}))

	rows, err := s.repo.ListSchedulableCapacityByGroupIDs(s.ctx, []int64{lowGroup.ID, highGroup.ID})
	s.Require().NoError(err)
	s.Require().Len(rows, 2, "an enabled guard without a first successful probe remains temporarily available")

	_, err = s.repo.sql.ExecContext(s.ctx, `
		UPDATE accounts
		SET upstream_billing_guard_observed_multiplier = 2
		WHERE id = $1
	`, account.ID)
	s.Require().NoError(err)

	rows, err = s.repo.ListSchedulableCapacityByGroupIDs(s.ctx, []int64{lowGroup.ID, highGroup.ID})
	s.Require().NoError(err)
	s.Require().Len(rows, 1)
	s.Require().Equal(highGroup.ID, rows[0].GroupID)
}

func (s *AccountRepoSuite) TestListSchedulableByGroupID_TimeBoundaries_And_StatusUpdates() {
	now := time.Now()
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sched"})

	okAcc := mustCreateAccount(s.T(), s.client, &service.Account{Name: "ok", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, okAcc.ID, group.ID, 1)

	future := now.Add(10 * time.Minute)
	overloaded := mustCreateAccount(s.T(), s.client, &service.Account{Name: "over", Schedulable: true, OverloadUntil: &future})
	mustBindAccountToGroup(s.T(), s.client, overloaded.ID, group.ID, 1)

	rateLimited := mustCreateAccount(s.T(), s.client, &service.Account{Name: "rl", Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, rateLimited.ID, group.ID, 1)
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, rateLimited.ID, now.Add(10*time.Minute)), "SetRateLimited")

	s.Require().NoError(s.repo.SetError(s.ctx, overloaded.ID, "boom"), "SetError")

	sched, err := s.repo.ListSchedulableByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ListSchedulableByGroupID")
	s.Require().Len(sched, 1, "expected only ok account schedulable")
	s.Require().Equal(okAcc.ID, sched[0].ID)

	s.Require().NoError(s.repo.ClearRateLimit(s.ctx, rateLimited.ID), "ClearRateLimit")
	sched2, err := s.repo.ListSchedulableByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ListSchedulableByGroupID after ClearRateLimit")
	s.Require().Len(sched2, 2, "expected 2 schedulable accounts after ClearRateLimit")
}

func (s *AccountRepoSuite) TestListSchedulableByPlatform() {
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic, Schedulable: true})
	mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI, Schedulable: true})

	accounts, err := s.repo.ListSchedulableByPlatform(s.ctx, service.PlatformAnthropic)
	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal(service.PlatformAnthropic, accounts[0].Platform)
}

func (s *AccountRepoSuite) TestListSchedulableByGroupIDAndPlatform() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "g-sp"})
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a1", Platform: service.PlatformAnthropic, Schedulable: true})
	a2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "a2", Platform: service.PlatformOpenAI, Schedulable: true})
	mustBindAccountToGroup(s.T(), s.client, a1.ID, group.ID, 1)
	mustBindAccountToGroup(s.T(), s.client, a2.ID, group.ID, 2)

	accounts, err := s.repo.ListSchedulableByGroupIDAndPlatform(s.ctx, group.ID, service.PlatformAnthropic)
	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal(a1.ID, accounts[0].ID)
}

func (s *AccountRepoSuite) TestSetSchedulable() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-sched", Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.SetSchedulable(s.ctx, account.ID, false))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().False(got.Schedulable)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
}

func (s *AccountRepoSuite) TestBulkUpdate_SyncSchedulerSnapshotOnDisabled() {
	account1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-1", Status: service.StatusActive, Schedulable: true})
	account2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-2", Status: service.StatusActive, Schedulable: true})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	disabled := service.StatusDisabled
	rows, err := s.repo.BulkUpdate(s.ctx, []int64{account1.ID, account2.ID}, service.AccountBulkUpdate{
		Status: &disabled,
	})
	s.Require().NoError(err)
	s.Require().Equal(int64(2), rows)

	s.Require().Len(cacheRecorder.setAccounts, 2)
	ids := map[int64]struct{}{}
	for _, acc := range cacheRecorder.setAccounts {
		ids[acc.ID] = struct{}{}
	}
	s.Require().Contains(ids, account1.ID)
	s.Require().Contains(ids, account2.ID)
}

// --- SetOverloaded / SetRateLimited / ClearRateLimit ---

func (s *AccountRepoSuite) TestSetOverloaded() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-over"})
	until := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, until))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.OverloadUntil)
	s.Require().WithinDuration(until, *got.OverloadUntil, time.Second)
}

func (s *AccountRepoSuite) TestSetRateLimited() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-rl"})
	resetAt := time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC)

	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, resetAt))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.RateLimitedAt)
	s.Require().NotNil(got.RateLimitResetAt)
	s.Require().WithinDuration(resetAt, *got.RateLimitResetAt, time.Second)
}

func (s *AccountRepoSuite) TestClearRateLimit() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-clear"})
	until := time.Now().Add(1 * time.Hour)
	s.Require().NoError(s.repo.SetOverloaded(s.ctx, account.ID, until))
	s.Require().NoError(s.repo.SetRateLimited(s.ctx, account.ID, until))

	s.Require().NoError(s.repo.ClearRateLimit(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.RateLimitedAt)
	s.Require().Nil(got.RateLimitResetAt)
	s.Require().Nil(got.OverloadUntil)
}

func (s *AccountRepoSuite) TestTempUnschedulableFieldsLoadedByGetByIDAndGetByIDs() {
	acc1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-temp-1"})
	acc2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-temp-2"})

	until := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Second)
	reason := `{"rule":"429","matched_keyword":"too many requests"}`
	s.Require().NoError(s.repo.SetTempUnschedulable(s.ctx, acc1.ID, until, reason))

	gotByID, err := s.repo.GetByID(s.ctx, acc1.ID)
	s.Require().NoError(err)
	s.Require().NotNil(gotByID.TempUnschedulableUntil)
	s.Require().WithinDuration(until, *gotByID.TempUnschedulableUntil, time.Second)
	s.Require().Equal(reason, gotByID.TempUnschedulableReason)

	gotByIDs, err := s.repo.GetByIDs(s.ctx, []int64{acc2.ID, acc1.ID})
	s.Require().NoError(err)
	s.Require().Len(gotByIDs, 2)
	s.Require().Equal(acc2.ID, gotByIDs[0].ID)
	s.Require().Nil(gotByIDs[0].TempUnschedulableUntil)
	s.Require().Equal("", gotByIDs[0].TempUnschedulableReason)
	s.Require().Equal(acc1.ID, gotByIDs[1].ID)
	s.Require().NotNil(gotByIDs[1].TempUnschedulableUntil)
	s.Require().WithinDuration(until, *gotByIDs[1].TempUnschedulableUntil, time.Second)
	s.Require().Equal(reason, gotByIDs[1].TempUnschedulableReason)

	s.Require().NoError(s.repo.ClearTempUnschedulable(s.ctx, acc1.ID))
	cleared, err := s.repo.GetByID(s.ctx, acc1.ID)
	s.Require().NoError(err)
	s.Require().Nil(cleared.TempUnschedulableUntil)
	s.Require().Equal("", cleared.TempUnschedulableReason)
}

// --- UpdateLastUsed ---

func (s *AccountRepoSuite) TestUpdateLastUsed() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-used"})
	s.Require().Nil(account.LastUsedAt)

	s.Require().NoError(s.repo.UpdateLastUsed(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.LastUsedAt)
}

// --- SetError ---

func (s *AccountRepoSuite) TestSetError() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-err", Status: service.StatusActive, Schedulable: true})

	s.Require().NoError(s.repo.SetError(s.ctx, account.ID, "something went wrong"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusError, got.Status)
	s.Require().Equal("something went wrong", got.ErrorMessage)
	s.Require().False(got.Schedulable)
}

func (s *AccountRepoSuite) TestUpdateErrorStatusUnschedulesAccount() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-update-err", Status: service.StatusActive, Schedulable: true})
	account.Status = service.StatusError
	account.ErrorMessage = "token revoked"
	account.Schedulable = true

	s.Require().NoError(s.repo.Update(s.ctx, account))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusError, got.Status)
	s.Require().Equal("token revoked", got.ErrorMessage)
	s.Require().False(got.Schedulable)
}

func (s *AccountRepoSuite) TestClearError_SyncSchedulerSnapshotOnRecovery() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:         "acc-clear-err",
		Status:       service.StatusError,
		ErrorMessage: "temporary error",
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	s.Require().NoError(s.repo.ClearError(s.ctx, account.ID))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusActive, got.Status)
	s.Require().Empty(got.ErrorMessage)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusActive, cacheRecorder.setAccounts[0].Status)
}

// --- UpdateSessionWindow ---

func (s *AccountRepoSuite) TestUpdateSessionWindow() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-win"})
	start := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	end := time.Date(2025, 6, 15, 15, 0, 0, 0, time.UTC)

	s.Require().NoError(s.repo.UpdateSessionWindow(s.ctx, account.ID, &start, &end, "active"))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().NotNil(got.SessionWindowStart)
	s.Require().NotNil(got.SessionWindowEnd)
	s.Require().Equal("active", got.SessionWindowStatus)
}

// --- UpdateExtra ---

func (s *AccountRepoSuite) TestUpdateExtra_MergesFields() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "acc-extra",
		Extra: map[string]any{"a": "1"},
	})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{"b": "2"}), "UpdateExtra")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("1", got.Extra["a"])
	s.Require().Equal("2", got.Extra["b"])
}

func (s *AccountRepoSuite) TestUpdateExtra_EmptyUpdates() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-extra-empty"})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{}))
}

func (s *AccountRepoSuite) TestUpdateExtra_NilExtra() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-nil-extra", Extra: nil})
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{"key": "val"}))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("val", got.Extra["key"])
}

func (s *AccountRepoSuite) TestUpdateExtra_SchedulerNeutralSkipsOutboxAndSyncsFreshSnapshot() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-neutral",
		Platform: service.PlatformOpenAI,
		Extra:    map[string]any{"codex_usage_updated_at": "old"},
	})
	cacheRecorder := &schedulerCacheRecorder{
		accounts: map[int64]*service.Account{
			account.ID: {
				ID:       account.ID,
				Platform: account.Platform,
				Status:   service.StatusDisabled,
				Extra: map[string]any{
					"codex_usage_updated_at": "old",
				},
			},
		},
	}
	s.repo.schedulerCache = cacheRecorder

	updates := map[string]any{
		"codex_usage_updated_at":     "2026-03-11T10:00:00Z",
		"codex_5h_used_percent":      88.5,
		"session_window_utilization": 0.42,
	}
	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, updates))

	got, err := s.repo.GetByID(s.ctx, account.ID)
	s.Require().NoError(err)
	s.Require().Equal("2026-03-11T10:00:00Z", got.Extra["codex_usage_updated_at"])
	s.Require().Equal(88.5, got.Extra["codex_5h_used_percent"])
	s.Require().Equal(0.42, got.Extra["session_window_utilization"])

	var outboxCount int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &outboxCount))
	s.Require().Zero(outboxCount)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().NotNil(cacheRecorder.accounts[account.ID])
	s.Require().Equal(service.StatusActive, cacheRecorder.accounts[account.ID].Status)
	s.Require().Equal("2026-03-11T10:00:00Z", cacheRecorder.accounts[account.ID].Extra["codex_usage_updated_at"])
}

func (s *AccountRepoSuite) TestUpdateExtra_ExhaustedCodexSnapshotSyncsSchedulerCache() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-codex-exhausted",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra:    map[string]any{},
	})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"codex_7d_used_percent":        100.0,
		"codex_7d_reset_at":            "2026-03-12T13:00:00Z",
		"codex_7d_reset_after_seconds": 86400,
	}))

	var count int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count)
	s.Require().NoError(err)
	s.Require().Equal(0, count)
	s.Require().Len(cacheRecorder.setAccounts, 1)
	s.Require().Equal(account.ID, cacheRecorder.setAccounts[0].ID)
	s.Require().Equal(service.StatusActive, cacheRecorder.setAccounts[0].Status)
	s.Require().Equal(100.0, cacheRecorder.setAccounts[0].Extra["codex_7d_used_percent"])
}

func (s *AccountRepoSuite) TestUpdateExtra_SchedulerRelevantStillEnqueuesOutbox() {
	account := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "acc-extra-mixed",
		Platform: service.PlatformAntigravity,
		Extra:    map[string]any{},
	})
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)

	s.Require().NoError(s.repo.UpdateExtra(s.ctx, account.ID, map[string]any{
		"mixed_scheduling":       true,
		"codex_usage_updated_at": "2026-03-11T10:00:00Z",
	}))

	var count int
	err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox", nil, &count)
	s.Require().NoError(err)
	s.Require().Equal(1, count)
}

// --- GetByCRSAccountID ---

func (s *AccountRepoSuite) TestGetByCRSAccountID() {
	crsID := "crs-12345"
	mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "acc-crs",
		Extra: map[string]any{"crs_account_id": crsID},
	})

	got, err := s.repo.GetByCRSAccountID(s.ctx, crsID)
	s.Require().NoError(err)
	s.Require().NotNil(got)
	s.Require().Equal("acc-crs", got.Name)
}

func (s *AccountRepoSuite) TestGetByCRSAccountID_NotFound() {
	got, err := s.repo.GetByCRSAccountID(s.ctx, "non-existent")
	s.Require().NoError(err)
	s.Require().Nil(got)
}

func (s *AccountRepoSuite) TestGetByCRSAccountID_EmptyString() {
	got, err := s.repo.GetByCRSAccountID(s.ctx, "")
	s.Require().NoError(err)
	s.Require().Nil(got)
}

// --- BulkUpdate ---

func (s *AccountRepoSuite) TestBulkUpdate() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk1", Priority: 1})
	a2 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk2", Priority: 1})
	cacheRecorder := &schedulerCacheRecorder{}
	s.repo.schedulerCache = cacheRecorder

	newPriority := 99
	affected, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID, a2.ID}, service.AccountBulkUpdate{
		Priority: &newPriority,
	})
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(affected, int64(1), "expected at least one affected row")

	got1, _ := s.repo.GetByID(s.ctx, a1.ID)
	got2, _ := s.repo.GetByID(s.ctx, a2.ID)
	s.Require().Equal(99, got1.Priority)
	s.Require().Equal(99, got2.Priority)
	s.Require().Len(cacheRecorder.setAccounts, 2)
	s.Require().Equal(99, cacheRecorder.accounts[a1.ID].Priority)
	s.Require().Equal(99, cacheRecorder.accounts[a2.ID].Priority)
}

func (s *AccountRepoSuite) TestBulkUpdate_MergeCredentials() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:        "bulk-cred",
		Credentials: map[string]any{"existing": "value"},
	})

	_, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{
		Credentials: map[string]any{"new_key": "new_value"},
	})
	s.Require().NoError(err)

	got, _ := s.repo.GetByID(s.ctx, a1.ID)
	s.Require().Equal("value", got.Credentials["existing"])
	s.Require().Equal("new_value", got.Credentials["new_key"])
}

func (s *AccountRepoSuite) TestBulkUpdate_MergeExtra() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:  "bulk-extra",
		Extra: map[string]any{"existing": "val"},
	})

	_, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{
		Extra: map[string]any{"new_key": "new_val"},
	})
	s.Require().NoError(err)

	got, _ := s.repo.GetByID(s.ctx, a1.ID)
	s.Require().Equal("val", got.Extra["existing"])
	s.Require().Equal("new_val", got.Extra["new_key"])
}

func (s *AccountRepoSuite) TestBulkUpdate_EmptyIDs() {
	affected, err := s.repo.BulkUpdate(s.ctx, []int64{}, service.AccountBulkUpdate{})
	s.Require().NoError(err)
	s.Require().Zero(affected)
}

func (s *AccountRepoSuite) TestBulkUpdate_EmptyUpdates() {
	a1 := mustCreateAccount(s.T(), s.client, &service.Account{Name: "bulk-empty"})

	affected, err := s.repo.BulkUpdate(s.ctx, []int64{a1.ID}, service.AccountBulkUpdate{})
	s.Require().NoError(err)
	s.Require().Zero(affected)
}

func idsOfAccounts(accounts []service.Account) []int64 {
	out := make([]int64, 0, len(accounts))
	for i := range accounts {
		out = append(out, accounts[i].ID)
	}
	return out
}
