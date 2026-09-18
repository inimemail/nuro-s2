package repository

import (
	"context"
	"sort"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountDuplicateRepository = (*accountRepository)(nil)

// CreateWithAccountGroups commits the account, binding settings and scheduler
// event together. A failed binding must never leave a credential-only orphan.
func (r *accountRepository) CreateWithAccountGroups(ctx context.Context, account *service.Account, groups []service.AccountGroup) error {
	if account == nil {
		return service.ErrAccountNilInput
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	groupIDs := make([]int64, 0, len(groups))
	for _, group := range groups {
		groupIDs = append(groupIDs, group.GroupID)
	}
	// Lock live groups in a stable order, matching BindGroups. This also
	// prevents a concurrent group deletion from leaving an incomplete copy.
	lockIDs := append([]int64(nil), groupIDs...)
	sort.Slice(lockIDs, func(i, j int) bool { return lockIDs[i] < lockIDs[j] })
	if len(lockIDs) > 0 {
		live, err := client.Group.Query().Where(dbgroup.IDIn(lockIDs...)).Order(dbent.Asc(dbgroup.FieldID)).Select(dbgroup.FieldID, dbgroup.FieldPlatform).ForUpdate().All(ctx)
		if err != nil {
			return err
		}
		if len(live) != len(lockIDs) {
			return service.ErrGroupNotFound
		}
		for _, group := range live {
			if group.Platform == service.PlatformComposite {
				return infraerrors.BadRequest("ACCOUNT_GROUP_COMPOSITE_UNSUPPORTED", "accounts cannot be bound directly to composite groups")
			}
		}
	}
	if err := createAccountRecord(ctx, client, account); err != nil {
		return err
	}
	if err := client.Account.Update().Where(dbaccount.IDEQ(account.ID)).
		SetUpstreamBillingGuardEnabled(account.UpstreamBillingGuardEnabled).
		SetUpstreamBillingGuardMaxMultiplier(account.UpstreamBillingGuardMaxMultiplier).Exec(ctx); err != nil {
		return err
	}
	for _, group := range groups {
		if _, err := client.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(group.GroupID).SetPriority(group.Priority).
			SetNillableUpstreamBillingGuardMaxMultiplier(group.UpstreamBillingGuardOverrideMaxMultiplier).
			SetNillableUpstreamBillingGuardMinMultiplier(group.UpstreamBillingGuardOverrideMinMultiplier).Save(ctx); err != nil {
			return err
		}
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	account.GroupIDs = groupIDs
	account.AccountGroups = append([]service.AccountGroup(nil), groups...)
	for i := range account.AccountGroups {
		account.AccountGroups[i].AccountID = account.ID
	}
	return nil
}
