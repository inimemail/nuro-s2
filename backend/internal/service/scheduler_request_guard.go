package service

import "context"

// Scheduler request guards avoid starting snapshot reads for already-cancelled
// requests and discard results when cancellation wins the read race. The
// snapshot service still exclusively owns publication and cache semantics.
func listSchedulableAccountsForRequest(
	ctx context.Context,
	snapshot *SchedulerSnapshotService,
	groupID *int64,
	platform string,
	forcePlatform bool,
) ([]Account, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	accounts, useMixed, err := snapshot.ListSchedulableAccounts(ctx, groupID, platform, forcePlatform)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, useMixed, ctxErr
	}
	return accounts, useMixed, err
}

func getSchedulerAccountForRequest(ctx context.Context, snapshot *SchedulerSnapshotService, accountID int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	account, err := snapshot.GetAccount(ctx, accountID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return account, err
}
