package repository

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// These copies exist only for a scoped create transaction. Legacy/JWT paths
// retain their repositories, publication timing and external onboarding flow.
func (r *accountRepository) ScopedAdminCreateRepositories(tx *dbent.Tx) (service.AccountRepository, service.GroupRepository) {
	return &accountRepository{client: tx.Client(), sql: tx.Client()}, &groupRepository{client: tx.Client(), sql: tx.Client()}
}
func (r *accountRepository) ScopedAdminPublish(ctx context.Context, id int64, deleted bool) {
	if deleted {
		r.deleteSchedulerAccountSnapshot(ctx, id)
	} else {
		r.syncSchedulerAccountSnapshot(ctx, id)
	}
}
