package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The opt-in DSN must name the isolated verification database. CI explicitly
// requires this test, so a missing database cannot be reported as a pass.
func TestScopedAdminPostgresTransactions(t *testing.T) {
	dsn := os.Getenv("NURO_CACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated nuro_upgrade_verify database")
	}
	require.Contains(t, dsn, "dbname=nuro_upgrade_verify")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, db.PingContext(ctx))
	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := NewAccountRepository(client, db, nil)
	groups := NewGroupRepository(client, db)
	admin := service.NewAdminService(nil, groups, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil)
	cfg := &config.Config{}
	cfg.Security.ScopedAdminAPIKeysEnabled = true
	s := service.NewScopedAdminService(cfg, db, client, admin, repo)
	stamp := uuid.NewString()
	var userID, groupID, accountID, hiddenID int64
	require.NoError(t, db.QueryRow(`INSERT INTO users(email,password_hash,role,status) VALUES($1,'test','admin','active') RETURNING id`, stamp+"@example.test").Scan(&userID))
	require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, stamp).Scan(&groupID))
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type,credentials,extra) VALUES($1,'openai','apikey','{"api_key":"fixture-secret","model_mapping":{"alias":"gpt-6"}}','{"upstream_cache_creation_optimization_enabled":true,"manual_upstream_multiplier":0.25}') RETURNING id`, stamp).Scan(&accountID))
	require.NoError(t, db.QueryRow(`INSERT INTO accounts(name,platform,type) VALUES($1,'grok','apikey') RETURNING id`, stamp+"-hidden").Scan(&hiddenID))
	require.NoError(t, db.QueryRow(`INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,17) RETURNING account_id`, accountID, groupID).Scan(new(int64)))
	// Existing database triggers normalize defaults at insert time. Protect the
	// persisted baseline, including those defaults, across every scoped write.
	var originalExtra string
	require.NoError(t, db.QueryRow(`SELECT extra::text FROM accounts WHERE id=$1`, accountID).Scan(&originalExtra))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM scoped_admin_operations WHERE key_id IN (SELECT id FROM scoped_admin_api_keys WHERE creator_id=$1)`, userID)
		_, _ = db.Exec(`DELETE FROM scoped_admin_api_keys WHERE creator_id=$1`, userID)
		_, _ = db.Exec(`DELETE FROM account_groups WHERE group_id=$1`, groupID)
		_, _ = db.Exec(`DELETE FROM scheduler_outbox WHERE account_id IN ($1,$2) OR group_id=$3`, accountID, hiddenID, groupID)
		_, _ = db.Exec(`DELETE FROM accounts WHERE id IN ($1,$2) OR name=$3`, accountID, hiddenID, stamp+"-created")
		_, _ = db.Exec(`DELETE FROM groups WHERE id=$1`, groupID)
		_, _ = db.Exec(`DELETE FROM users WHERE id=$1`, userID)
	})
	policy := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: append([]string{}, adminaccess.Permissions...), AccountIDs: []int64{accountID}, GroupIDs: []int64{groupID}, Platforms: []string{"openai"}}}}
	input := service.ScopedAdminKeyInput{Name: "test", Enabled: true, Policy: policy, RPM: 60}
	key, secret, err := s.CreateKey(ctx, userID, input)
	require.NoError(t, err)
	p, err := s.Authenticate(ctx, secret, "127.0.0.1")
	require.NoError(t, err)
	call := func(principal *service.ScopedAdminPrincipal, method, kind string, id int64, body, idem string) (json.RawMessage, error) {
		path := "/api/v1/admin/" + kind
		if id > 0 {
			path += "/:id"
		}
		return s.Resource(ctx, principal, method, path, kind, id, 1, 20, []byte(body), idem)
	}
	t.Run("SQL scope and narrow projection", func(t *testing.T) {
		out, e := call(p, "GET", "accounts", 0, "{}", "")
		require.NoError(t, e)
		require.Contains(t, string(out), `"total": 1`)
		require.Contains(t, string(out), `"alias": "gpt-6"`)
		require.NotContains(t, string(out), "fixture-secret")
		require.NotContains(t, string(out), "manual_upstream_multiplier")
		_, e = call(p, "GET", "accounts", hiddenID, "{}", "")
		require.ErrorIs(t, e, service.ErrScopedAdminNotFound)
	})
	t.Run("managed Grok catalog CAS preserves other fields and credential revision", func(t *testing.T) {
		store, ok := repo.(service.GrokCatalogStore)
		require.True(t, ok)
		before, e := repo.GetByID(ctx, hiddenID)
		require.NoError(t, e)
		now := time.Now().UTC()
		next := service.GrokCatalogObservation{Scope: service.GrokCatalogScope(before), AttemptedAt: now, SucceededAt: &now, Status: "success", Models: []string{"grok-fixture"}}
		saved, e := store.CompareAndSwapGrokCatalog(ctx, hiddenID, nil, before.UpdatedAt, next)
		require.NoError(t, e)
		require.True(t, saved)
		current, e := repo.GetByID(ctx, hiddenID)
		require.NoError(t, e)
		require.True(t, before.UpdatedAt.Equal(current.UpdatedAt), "catalog writes must not advance the configuration revision")
		previous := current.Extra[service.GrokModelCatalogExtraKey]
		_, e = db.Exec(`UPDATE accounts SET credentials='{"api_key":"rotated-fixture"}'::jsonb,extra=extra||'{"preserve_fixture":17}'::jsonb,updated_at=updated_at+interval '1 second' WHERE id=$1`, hiddenID)
		require.NoError(t, e)
		saved, e = store.CompareAndSwapGrokCatalog(ctx, hiddenID, previous, current.UpdatedAt, next)
		require.NoError(t, e)
		require.False(t, saved, "a response obtained before rotation must not be committed")
		rotated, e := repo.GetByID(ctx, hiddenID)
		require.NoError(t, e)
		require.Equal(t, "rotated-fixture", rotated.GetCredential("api_key"))
		require.EqualValues(t, 17, rotated.Extra["preserve_fixture"])
		require.Equal(t, "source_changed", service.BuildGrokCatalogDiagnostics(rotated, now).Status)
	})
	t.Run("group scope platforms and nonmatching dimensions stay in SQL", func(t *testing.T) {
		var grokGroup int64
		require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'grok') RETURNING id`, stamp+"-grok").Scan(&grokGroup))
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM groups WHERE id=$1`, grokGroup) })
		in := service.ScopedAdminKeyInput{Name: "platform scope", Enabled: true, RPM: 60, Policy: adminaccess.Policy{Rules: []adminaccess.Rule{
			{Permissions: []string{"accounts.read", "accounts.models.read", "groups.read"}, ProxyIDs: []int64{1}, Platforms: []string{"grok"}},
			{Permissions: []string{"groups.read", "groups.update"}, GroupIDs: []int64{groupID, grokGroup}, Platforms: []string{"openai"}},
			{Permissions: []string{"accounts.read"}, AccountIDs: []int64{accountID}, Platforms: []string{"openai"}},
		}}}
		_, value, e := s.CreateKey(ctx, userID, in)
		require.NoError(t, e)
		principal, e := s.Authenticate(ctx, value, "127.0.0.1")
		require.NoError(t, e)
		out, e := call(principal, "GET", "accounts", 0, "{}", "")
		require.NoError(t, e, "irrelevant proxy scopes must not leave unused SQL parameters")
		require.Contains(t, string(out), `"total": 1`)
		out, e = call(principal, "GET", "groups", 0, "{}", "")
		require.NoError(t, e)
		require.Contains(t, string(out), `"total": 1`)
		_, e = call(principal, "GET", "groups", grokGroup, "{}", "")
		require.ErrorIs(t, e, service.ErrScopedAdminNotFound)
		_, e = call(principal, "PUT", "groups", grokGroup, `{"name":"must not write"}`, "group-platform")
		require.ErrorIs(t, e, service.ErrScopedAdminNotFound)
		var name string
		require.NoError(t, db.QueryRow(`SELECT name FROM groups WHERE id=$1`, grokGroup).Scan(&name))
		require.Equal(t, stamp+"-grok", name)
		in.Policy.Rules = append(in.Policy.Rules, adminaccess.Rule{Permissions: []string{"groups.read"}, All: true})
		_, value, e = s.CreateKey(ctx, userID, in)
		require.NoError(t, e)
		principal, e = s.Authenticate(ctx, value, "127.0.0.1")
		require.NoError(t, e)
		_, e = call(principal, "GET", "groups", grokGroup, "{}", "")
		require.NoError(t, e, "an unrestricted later rule must not leave unused SQL parameters")
	})
	t.Run("protected fields reject atomically", func(t *testing.T) {
		for i, body := range []string{`{"name":"changed","extra":{}}`, `{"credentials":{"api_key":null}}`, `{"credentials":{"upstream_cache_creation_optimization_enabled":false}}`, `{"name":null}`, `{"platform":"grok"}`, `{"status":"disabled"}`} {
			_, e := call(p, "PUT", "accounts", accountID, body, fmt.Sprintf("invalid-%d", i))
			require.Error(t, e, body)
		}
		var name string
		require.NoError(t, db.QueryRow(`SELECT name FROM accounts WHERE id=$1`, accountID).Scan(&name))
		require.Equal(t, stamp, name)
	})
	t.Run("create fields use destination group scope", func(t *testing.T) {
		for _, fieldPermission := range []string{"accounts.notes.write", "accounts.credentials.write", "accounts.schedule.write", "accounts.network.write"} {
			t.Run(fieldPermission, func(t *testing.T) {
				permissions := []string{}
				for _, permission := range adminaccess.Permissions {
					if permission != fieldPermission {
						permissions = append(permissions, permission)
					}
				}
				in := service.ScopedAdminKeyInput{Name: "destination scope", Enabled: true, RPM: 60, Policy: adminaccess.Policy{Rules: []adminaccess.Rule{
					{Permissions: permissions, GroupIDs: []int64{groupID}, Platforms: []string{"openai"}},
					{Permissions: []string{fieldPermission}, Ungrouped: true, Platforms: []string{"openai"}},
				}}}
				k, value, e := s.CreateKey(ctx, userID, in)
				require.NoError(t, e)
				principal, e := s.Authenticate(ctx, value, "127.0.0.1")
				require.NoError(t, e)
				name := stamp + "-scope-" + fieldPermission
				t.Cleanup(func() {
					_, _ = db.Exec(`DELETE FROM account_groups WHERE account_id IN (SELECT id FROM accounts WHERE name=$1)`, name)
					_, _ = db.Exec(`DELETE FROM scheduler_outbox WHERE account_id IN (SELECT id FROM accounts WHERE name=$1)`, name)
					_, _ = db.Exec(`DELETE FROM accounts WHERE name=$1`, name)
				})
				body := fmt.Sprintf(`{"name":%q,"platform":"openai","type":"apikey","credentials":{"api_key":"fixture-scope"},"add_group_ids":[%d],"concurrency":2,"proxy_mode":"direct"}`, name, groupID)
				_, e = call(principal, "POST", "accounts", 0, body, "destination-scope")
				require.ErrorIs(t, e, service.ErrScopedAdminDenied)
				var accounts, receipts int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM accounts WHERE name=$1`, name).Scan(&accounts))
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_operations WHERE key_id=$1`, k.ID).Scan(&receipts))
				require.Zero(t, accounts)
				require.Zero(t, receipts)
			})
		}
	})
	t.Run("account status matches the existing admin and form vocabulary", func(t *testing.T) {
		for _, status := range []string{"inactive", "active"} {
			_, e := call(p, "PUT", "accounts", accountID, fmt.Sprintf(`{"status":%q}`, status), "status-"+status)
			require.NoError(t, e)
			account, e := repo.GetByID(ctx, accountID)
			require.NoError(t, e)
			require.Equal(t, status, account.Status)
		}
	})
	t.Run("concurrent committed receipt and stable revision identity", func(t *testing.T) {
		var previousOperations, previousEvents int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_operations WHERE key_id=$1`, key.ID).Scan(&previousOperations))
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, accountID).Scan(&previousEvents))
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, e := call(p, "PUT", "accounts", accountID, `{"name":"saved"}`, "same-operation")
				errs <- e
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			require.NoError(t, e)
		}
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_operations WHERE key_id=$1`, key.ID).Scan(&n))
		require.Equal(t, previousOperations+1, n)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, accountID).Scan(&n))
		require.Equal(t, previousEvents+1, n, "replay must not publish a second change")
		_, e := call(p, "PUT", "accounts", accountID, `{"name":"other"}`, "same-operation")
		require.Error(t, e)
		input.Revision = key.Revision
		input.Policy.Rules[0].Permissions = []string{"accounts.update", "accounts.notes.write"}
		require.NoError(t, s.UpdateKey(ctx, key.ID, input))
		_, e = call(p, "PUT", "accounts", accountID, `{"name":"saved"}`, "same-operation")
		require.ErrorIs(t, e, service.ErrScopedAdminDenied, "stale principal rejected")
		fresh, e := s.Authenticate(ctx, secret, "127.0.0.1")
		require.NoError(t, e)
		out, e := call(fresh, "PUT", "accounts", accountID, `{"name":"saved"}`, "same-operation")
		require.NoError(t, e)
		require.JSONEq(t, `{"saved":true}`, string(out), "replay uses current read permissions")
		rotated, e := s.ChangeKey(ctx, key.ID, fresh.Key.Revision, "rotate")
		require.NoError(t, e)
		_, e = s.Authenticate(ctx, secret, "127.0.0.1")
		require.Error(t, e)
		p, e = s.Authenticate(ctx, rotated, "127.0.0.1")
		require.NoError(t, e)
		_, e = call(p, "PUT", "accounts", accountID, `{"name":"saved"}`, "same-operation")
		require.NoError(t, e)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_operations WHERE key_id=$1`, key.ID).Scan(&n))
		require.Equal(t, previousOperations+1, n)
	})
	t.Run("revoke cannot be reenabled", func(t *testing.T) {
		_, e := s.ChangeKey(ctx, key.ID, p.Key.Revision, "revoke")
		require.NoError(t, e)
		_, e = call(p, "PUT", "accounts", accountID, `{"name":"revoked"}`, "after-revoke")
		require.ErrorIs(t, e, service.ErrScopedAdminDenied)
		input.Revision = p.Key.Revision + 1
		require.Error(t, s.UpdateKey(ctx, key.ID, input))
	})
	var extra, credentials string
	var priority int
	require.NoError(t, db.QueryRow(`SELECT extra::text,credentials::text FROM accounts WHERE id=$1`, accountID).Scan(&extra, &credentials))
	require.JSONEq(t, originalExtra, extra)
	require.Contains(t, credentials, "fixture-secret")
	require.NoError(t, db.QueryRow(`SELECT priority FROM account_groups WHERE account_id=$1 AND group_id=$2`, accountID, groupID).Scan(&priority))
	require.Equal(t, 17, priority)

	t.Run("atomic create proxy choice group events and delete replay", func(t *testing.T) {
		var secondGroup, proxyID int64
		require.NoError(t, db.QueryRow(`INSERT INTO groups(name,platform) VALUES($1,'openai') RETURNING id`, stamp+"-second").Scan(&secondGroup))
		require.NoError(t, db.QueryRow(`INSERT INTO proxies(name,protocol,host,port) VALUES($1,'http','127.0.0.1',12345) RETURNING id`, stamp).Scan(&proxyID))
		var createdID int64
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM account_groups WHERE group_id=$1 OR account_id=$2`, secondGroup, createdID)
			_, _ = db.Exec(`DELETE FROM scheduler_outbox WHERE account_id=$1 OR group_id=$2`, createdID, secondGroup)
			_, _ = db.Exec(`DELETE FROM accounts WHERE id=$1`, createdID)
			_, _ = db.Exec(`DELETE FROM groups WHERE id=$1`, secondGroup)
			_, _ = db.Exec(`DELETE FROM proxies WHERE id=$1`, proxyID)
		})
		full := service.ScopedAdminKeyInput{Name: "writer", Enabled: true, RPM: 60, Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
			Permissions: append([]string{}, adminaccess.Permissions...), Ungrouped: true, Platforms: []string{"openai"},
			GroupIDs: []int64{groupID, secondGroup}, ProxyIDs: []int64{proxyID},
		}}}}
		newPrincipal := func() *service.ScopedAdminPrincipal {
			_, value, e := s.CreateKey(ctx, userID, full)
			require.NoError(t, e)
			principal, e := s.Authenticate(ctx, value, "127.0.0.1")
			require.NoError(t, e)
			return principal
		}
		writer, other := newPrincipal(), newPrincipal()
		body := fmt.Sprintf(`{"name":%q,"platform":"openai","type":"apikey","credentials":{"api_key":"fixture-created"},"add_group_ids":[%d],"proxy_mode":"least_used"}`, stamp+"-created", groupID)
		out, e := call(writer, "POST", "accounts", 0, body, "create-once")
		require.NoError(t, e)
		var created struct {
			ID int64 `json:"id"`
		}
		require.NoError(t, json.Unmarshal(out, &created))
		createdID = created.ID
		require.Positive(t, createdID)
		var chosen int64
		require.NoError(t, db.QueryRow(`SELECT proxy_id FROM accounts WHERE id=$1`, createdID).Scan(&chosen))
		require.Equal(t, proxyID, chosen)
		repeated, e := call(writer, "POST", "accounts", 0, body, "create-once")
		require.NoError(t, e)
		require.JSONEq(t, string(out), string(repeated))
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM accounts WHERE name=$1`, stamp+"-created").Scan(&n))
		require.Equal(t, 1, n)

		// The same literal idempotency key belongs to different stable key IDs.
		_, e = call(writer, "PUT", "accounts", createdID, `{"notes":"first"}`, "shared-key")
		require.NoError(t, e)
		_, e = call(other, "PUT", "accounts", createdID, `{"notes":"second"}`, "shared-key")
		require.NoError(t, e)
		var notes string
		require.NoError(t, db.QueryRow(`SELECT notes FROM accounts WHERE id=$1`, createdID).Scan(&notes))
		require.Equal(t, "second", notes)

		// Deliberately nondefault binding metadata must survive unrelated edits.
		_, e = db.Exec(`UPDATE account_groups SET priority=17 WHERE account_id=$1 AND group_id=$2`, createdID, groupID)
		require.NoError(t, e)
		var bindingBefore, bindingAfter string
		require.NoError(t, db.QueryRow(`SELECT row_to_json(ag)::text FROM account_groups ag WHERE account_id=$1 AND group_id=$2`, createdID, groupID).Scan(&bindingBefore))
		_, e = call(writer, "PUT", "accounts", createdID, fmt.Sprintf(`{"add_group_ids":[%d,%d]}`, groupID, secondGroup), "add-groups")
		require.NoError(t, e)
		require.NoError(t, db.QueryRow(`SELECT row_to_json(ag)::text FROM account_groups ag WHERE account_id=$1 AND group_id=$2`, createdID, groupID).Scan(&bindingAfter))
		require.JSONEq(t, bindingBefore, bindingAfter)
		_, e = call(writer, "PUT", "accounts", createdID, fmt.Sprintf(`{"remove_group_ids":[%d]}`, secondGroup), "remove-group")
		require.NoError(t, e)
		var payload string
		require.NoError(t, db.QueryRow(`SELECT payload::text FROM scheduler_outbox WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, createdID).Scan(&payload))
		var affected struct {
			Groups []int64 `json:"group_ids"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &affected))
		require.ElementsMatch(t, []int64{groupID, secondGroup}, affected.Groups)

		// A rejected mutation must not leave an account edit or a receipt.
		_, e = call(writer, "PUT", "accounts", createdID, `{"notes":"must rollback","proxy_mode":"specified","proxy_id":9223372036854775807}`, "rollback-proxy")
		require.Error(t, e)
		require.NoError(t, db.QueryRow(`SELECT notes FROM accounts WHERE id=$1`, createdID).Scan(&notes))
		require.Equal(t, "second", notes)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_operations WHERE key_id=$1 AND namespace='PUT /api/v1/admin/accounts/:id'`, writer.Key.ID).Scan(&n))
		require.Equal(t, 3, n)
		_, e = call(writer, "DELETE", "accounts", createdID, "{}", "delete-once")
		require.NoError(t, e)
		require.NoError(t, db.QueryRow(`SELECT payload::text FROM scheduler_outbox WHERE account_id=$1 ORDER BY id DESC LIMIT 1`, createdID).Scan(&payload))
		require.JSONEq(t, fmt.Sprintf(`{"group_ids":[%d]}`, groupID), payload)
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, createdID).Scan(&n))
		out, e = call(writer, "DELETE", "accounts", createdID, "{}", "delete-once")
		require.NoError(t, e)
		require.JSONEq(t, `{"deleted":true}`, string(out))
		var afterReplay int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, createdID).Scan(&afterReplay))
		require.Equal(t, n, afterReplay)
	})
	t.Run("key listing reaches older keys with stable bounded pages", func(t *testing.T) {
		policyJSON, e := json.Marshal(policy)
		require.NoError(t, e)
		_, e = db.Exec(`INSERT INTO scoped_admin_api_keys(name,key_prefix,key_hash,policy,creator_id)
SELECT 'pagination fixture','fixture',$1||'-'||n::text,$2::jsonb,$3 FROM generate_series(1,205) n`, stamp, string(policyJSON), userID)
		require.NoError(t, e)
		var expected int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM scoped_admin_api_keys WHERE deleted_at IS NULL`).Scan(&expected))
		seen := map[int64]bool{}
		cursor, previous := int64(0), int64(0)
		for {
			page, e := s.ListKeys(ctx, cursor)
			require.NoError(t, e)
			require.LessOrEqual(t, len(page.Items), 200)
			for _, k := range page.Items {
				require.False(t, seen[k.ID], "cursor must not repeat a key")
				if previous != 0 {
					require.Less(t, k.ID, previous)
				}
				seen[k.ID], previous = true, k.ID
			}
			if page.NextBeforeID == 0 {
				break
			}
			require.Len(t, page.Items, 200)
			require.Equal(t, previous, page.NextBeforeID)
			cursor = page.NextBeforeID
		}
		require.Len(t, seen, expected)
		_, e = s.ListKeys(ctx, -1)
		require.Error(t, e)
	})
}
