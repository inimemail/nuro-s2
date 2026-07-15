package repository

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"

	entsql "entgo.io/ent/dialect/sql"
)

type proxyRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

func NewProxyRepository(client *dbent.Client, sqlDB *sql.DB) service.ProxyRepository {
	return newProxyRepositoryWithSQL(client, sqlDB)
}

func newProxyRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *proxyRepository {
	return &proxyRepository{client: client, sql: sqlq}
}

func (r *proxyRepository) Create(ctx context.Context, proxyIn *service.Proxy) error {
	builder := r.client.Proxy.Create().
		SetName(proxyIn.Name).
		SetProtocol(proxyIn.Protocol).
		SetHost(proxyIn.Host).
		SetPort(proxyIn.Port).
		SetStatus(proxyIn.Status)
	if proxyIn.Username != "" {
		builder.SetUsername(proxyIn.Username)
	}
	if proxyIn.Password != "" {
		builder.SetPassword(proxyIn.Password)
	}
	if proxyIn.ExpiresAt != nil {
		builder.SetExpiresAt(*proxyIn.ExpiresAt)
	}
	if proxyIn.FallbackMode != "" {
		builder.SetFallbackMode(proxyIn.FallbackMode)
	}
	if proxyIn.BackupProxyID != nil {
		builder.SetBackupProxyID(*proxyIn.BackupProxyID)
	}
	builder.SetExpiryWarnDays(proxyIn.ExpiryWarnDays)

	created, err := builder.Save(ctx)
	if err == nil {
		applyProxyEntityToService(proxyIn, created)
	}
	return err
}

func (r *proxyRepository) GetByID(ctx context.Context, id int64) (*service.Proxy, error) {
	m, err := r.client.Proxy.Get(ctx, id)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrProxyNotFound
		}
		return nil, err
	}
	return proxyEntityToService(m), nil
}

func (r *proxyRepository) ListByIDs(ctx context.Context, ids []int64) ([]service.Proxy, error) {
	if len(ids) == 0 {
		return []service.Proxy{}, nil
	}

	proxies, err := r.client.Proxy.Query().
		Where(proxy.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]service.Proxy, 0, len(proxies))
	for i := range proxies {
		out = append(out, *proxyEntityToService(proxies[i]))
	}
	return out, nil
}

func (r *proxyRepository) Update(ctx context.Context, proxyIn *service.Proxy) error {
	builder := r.client.Proxy.UpdateOneID(proxyIn.ID).
		SetName(proxyIn.Name).
		SetProtocol(proxyIn.Protocol).
		SetHost(proxyIn.Host).
		SetPort(proxyIn.Port).
		SetStatus(proxyIn.Status)
	if proxyIn.Username != "" {
		builder.SetUsername(proxyIn.Username)
	} else {
		builder.ClearUsername()
	}
	if proxyIn.Password != "" {
		builder.SetPassword(proxyIn.Password)
	} else {
		builder.ClearPassword()
	}
	if proxyIn.ExpiresAt != nil {
		builder.SetExpiresAt(*proxyIn.ExpiresAt)
	} else {
		builder.ClearExpiresAt()
	}
	if proxyIn.FallbackMode != "" {
		builder.SetFallbackMode(proxyIn.FallbackMode)
	} else {
		builder.SetFallbackMode(service.FallbackModeNone)
	}
	if proxyIn.BackupProxyID != nil {
		builder.SetBackupProxyID(*proxyIn.BackupProxyID)
	} else {
		builder.ClearBackupProxyID()
	}
	builder.SetExpiryWarnDays(proxyIn.ExpiryWarnDays)

	updated, err := builder.Save(ctx)
	if err == nil {
		applyProxyEntityToService(proxyIn, updated)
		return nil
	}
	if dbent.IsNotFound(err) {
		return service.ErrProxyNotFound
	}
	return err
}

func (r *proxyRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.client.Proxy.Delete().Where(proxy.IDEQ(id)).Exec(ctx)
	return err
}

func (r *proxyRepository) List(ctx context.Context, params pagination.PaginationParams) ([]service.Proxy, *pagination.PaginationResult, error) {
	return r.ListWithFilters(ctx, params, "", "", "")
}

// ListWithFilters lists proxies with optional filtering by protocol, status, and search query
func (r *proxyRepository) ListWithFilters(ctx context.Context, params pagination.PaginationParams, protocol, status, search string) ([]service.Proxy, *pagination.PaginationResult, error) {
	q := r.client.Proxy.Query()
	if protocol != "" {
		q = q.Where(proxy.ProtocolEQ(protocol))
	}
	if status != "" {
		q = q.Where(proxy.StatusEQ(status))
	}
	if search != "" {
		q = q.Where(proxy.NameContainsFold(search))
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	proxiesQuery := q.
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range proxyListOrder(params) {
		proxiesQuery = proxiesQuery.Order(order)
	}

	proxies, err := proxiesQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outProxies := make([]service.Proxy, 0, len(proxies))
	for i := range proxies {
		outProxies = append(outProxies, *proxyEntityToService(proxies[i]))
	}

	return outProxies, paginationResultFromTotal(int64(total), params), nil
}

// ListWithFiltersAndAccountCount lists proxies with filters and includes account count per proxy
func (r *proxyRepository) ListWithFiltersAndAccountCount(ctx context.Context, params pagination.PaginationParams, protocol, status, search string) ([]service.ProxyWithAccountCount, *pagination.PaginationResult, error) {
	q := r.client.Proxy.Query()
	if protocol != "" {
		q = q.Where(proxy.ProtocolEQ(protocol))
	}
	if status != "" {
		q = q.Where(proxy.StatusEQ(status))
	}
	if search != "" {
		q = q.Where(proxy.NameContainsFold(search))
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	if strings.EqualFold(strings.TrimSpace(params.SortBy), "account_count") {
		return r.listWithAccountCountSort(ctx, q, params, total)
	}

	proxiesQuery := q.
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range proxyListOrder(params) {
		proxiesQuery = proxiesQuery.Order(order)
	}

	proxies, err := proxiesQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	return r.buildProxyWithAccountCountResult(ctx, proxies, params, int64(total))
}

func (r *proxyRepository) listWithAccountCountSort(ctx context.Context, q *dbent.ProxyQuery, params pagination.PaginationParams, total int) ([]service.ProxyWithAccountCount, *pagination.PaginationResult, error) {
	proxies, err := q.
		Order(dbent.Desc(proxy.FieldID)).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}

	result, _, err := r.buildProxyWithAccountCountResult(ctx, proxies, params, int64(total))
	if err != nil {
		return nil, nil, err
	}

	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].AccountCount == result[j].AccountCount {
			return result[i].ID > result[j].ID
		}
		if sortOrder == pagination.SortOrderAsc {
			return result[i].AccountCount < result[j].AccountCount
		}
		return result[i].AccountCount > result[j].AccountCount
	})

	return paginateSlice(result, params), paginationResultFromTotal(int64(total), params), nil
}

func (r *proxyRepository) buildProxyWithAccountCountResult(ctx context.Context, proxies []*dbent.Proxy, params pagination.PaginationParams, total int64) ([]service.ProxyWithAccountCount, *pagination.PaginationResult, error) {
	counts, err := r.GetAccountCountsForProxies(ctx)
	if err != nil {
		return nil, nil, err
	}

	result := make([]service.ProxyWithAccountCount, 0, len(proxies))
	for i := range proxies {
		proxyOut := proxyEntityToService(proxies[i])
		if proxyOut == nil {
			continue
		}
		result = append(result, service.ProxyWithAccountCount{
			Proxy:        *proxyOut,
			AccountCount: counts[proxyOut.ID],
		})
	}

	return result, paginationResultFromTotal(total, params), nil
}

func proxyListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)

	var field string
	switch sortBy {
	case "name":
		field = proxy.FieldName
	case "protocol":
		field = proxy.FieldProtocol
	case "status":
		field = proxy.FieldStatus
	case "created_at":
		field = proxy.FieldCreatedAt
	case "expiry", "expires_at":
		field = proxy.FieldExpiresAt
	default:
		field = proxy.FieldID
	}

	if sortOrder == pagination.SortOrderAsc {
		return []func(*entsql.Selector){dbent.Asc(field), dbent.Asc(proxy.FieldID)}
	}
	return []func(*entsql.Selector){dbent.Desc(field), dbent.Desc(proxy.FieldID)}
}

func (r *proxyRepository) ListActive(ctx context.Context) ([]service.Proxy, error) {
	proxies, err := r.client.Proxy.Query().
		Where(proxy.StatusEQ(service.StatusActive)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	outProxies := make([]service.Proxy, 0, len(proxies))
	for i := range proxies {
		outProxies = append(outProxies, *proxyEntityToService(proxies[i]))
	}
	return outProxies, nil
}

func (r *proxyRepository) ListAllForFallback(ctx context.Context) ([]service.Proxy, error) {
	proxies, err := r.client.Proxy.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Proxy, 0, len(proxies))
	for i := range proxies {
		out = append(out, *proxyEntityToService(proxies[i]))
	}
	return out, nil
}

func (r *proxyRepository) SweepExpiredProxies(ctx context.Context, now time.Time) (int64, error) {
	allProxies, err := r.ListAllForFallback(ctx)
	if err != nil {
		return 0, err
	}
	proxiesByID := make(map[int64]service.Proxy, len(allProxies))
	for _, p := range allProxies {
		proxiesByID[p.ID] = p
	}

	expired, err := r.client.Proxy.Query().
		Where(
			proxy.ExpiresAtNotNil(),
			proxy.ExpiresAtLTE(now),
			proxy.StatusNEQ(service.StatusExpired),
		).
		All(ctx)
	if err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return 0, err
	}

	var exec sqlExecutor = r.sql
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		exec = tx.Client()
	}

	changedAccountIDs := make([]int64, 0)
	for _, entProxy := range expired {
		p := proxyEntityToService(entProxy)
		if p == nil {
			continue
		}
		targetProxyID, hasFallback := service.ResolveProxyFallbackTarget(*p, proxiesByID, now)
		accountIDs, err := r.sweepOneExpiredProxyOnExec(ctx, exec, p.ID, targetProxyID, hasFallback)
		if err != nil {
			return 0, err
		}
		changedAccountIDs = append(changedAccountIDs, accountIDs...)
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	changedAccountIDs = sortedUniqueAccountIDs(changedAccountIDs)
	if len(changedAccountIDs) > 0 {
		payload := map[string]any{"account_ids": changedAccountIDs}
		if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountBulkChanged, nil, nil, payload); err != nil {
			logger.LegacyPrintf("repository.proxy", "[SchedulerOutbox] enqueue proxy expiry account changes failed: err=%v", err)
		}
	}
	return int64(len(changedAccountIDs)), nil
}

func sortedUniqueAccountIDs(accountIDs []int64) []int64 {
	if len(accountIDs) < 2 {
		return accountIDs
	}
	sort.Slice(accountIDs, func(i, j int) bool { return accountIDs[i] < accountIDs[j] })
	write := 1
	for _, accountID := range accountIDs[1:] {
		if accountID == accountIDs[write-1] {
			continue
		}
		accountIDs[write] = accountID
		write++
	}
	return accountIDs[:write]
}

func (r *proxyRepository) sweepOneExpiredProxyOnExec(
	ctx context.Context,
	exec sqlExecutor,
	proxyID int64,
	targetProxyID *int64,
	hasFallback bool,
) ([]int64, error) {
	_, err := exec.ExecContext(ctx, `
		UPDATE proxies
		SET status = $2,
			updated_at = NOW()
		WHERE id = $1
			AND status <> $2
	`, proxyID, service.StatusExpired)
	if err != nil {
		return nil, err
	}

	if !hasFallback {
		rows, err := exec.QueryContext(ctx, `
			SELECT id
			FROM accounts
			WHERE deleted_at IS NULL
				AND proxy_id = $1
		`, proxyID)
		if err != nil {
			return nil, err
		}
		return scanAccountIDs(rows)
	}

	var rows *sql.Rows
	if targetProxyID != nil {
		rows, err = exec.QueryContext(ctx, `
			UPDATE accounts
			SET proxy_fallback_origin_id = COALESCE(proxy_fallback_origin_id, proxy_id),
				proxy_id = $2,
				updated_at = NOW()
			WHERE deleted_at IS NULL
				AND proxy_id = $1
			RETURNING id
		`, proxyID, *targetProxyID)
	} else {
		rows, err = exec.QueryContext(ctx, `
			UPDATE accounts
			SET proxy_fallback_origin_id = COALESCE(proxy_fallback_origin_id, proxy_id),
				proxy_id = NULL,
				updated_at = NOW()
			WHERE deleted_at IS NULL
				AND proxy_id = $1
			RETURNING id
		`, proxyID)
	}
	if err != nil {
		return nil, err
	}
	return scanAccountIDs(rows)
}

func scanAccountIDs(rows *sql.Rows) ([]int64, error) {
	if rows == nil {
		return nil, errors.New("nil account rows")
	}
	defer rows.Close()

	accountIDs := make([]int64, 0)
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func (r *proxyRepository) CountExpired(ctx context.Context) (int64, error) {
	var count int64
	if err := scanSingleRow(ctx, r.sql, `
		SELECT COUNT(*)
		FROM proxies
		WHERE expires_at IS NOT NULL
			AND expires_at <= NOW()
			AND status <> $1
	`, []any{service.StatusExpired}, &count); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *proxyRepository) CountExpiringSoon(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	if err := scanSingleRow(ctx, r.sql, `
		SELECT COUNT(*)
		FROM proxies
		WHERE expires_at IS NOT NULL
			AND expires_at > $1
			AND status <> $2
			AND expires_at <= $1 + (expiry_warn_days * INTERVAL '1 day')
	`, []any{now, service.StatusExpired}, &count); err != nil {
		return 0, err
	}
	return count, nil
}

// ExistsByHostPortAuth checks if a proxy with the same host, port, username, and password exists
func (r *proxyRepository) ExistsByHostPortAuth(ctx context.Context, host string, port int, username, password string) (bool, error) {
	q := r.client.Proxy.Query().
		Where(proxy.HostEQ(host), proxy.PortEQ(port))

	if username == "" {
		q = q.Where(proxy.Or(proxy.UsernameIsNil(), proxy.UsernameEQ("")))
	} else {
		q = q.Where(proxy.UsernameEQ(username))
	}
	if password == "" {
		q = q.Where(proxy.Or(proxy.PasswordIsNil(), proxy.PasswordEQ("")))
	} else {
		q = q.Where(proxy.PasswordEQ(password))
	}

	count, err := q.Count(ctx)
	return count > 0, err
}

// CountAccountsByProxyID returns the number of accounts using a specific proxy
func (r *proxyRepository) CountAccountsByProxyID(ctx context.Context, proxyID int64) (int64, error) {
	var count int64
	if err := scanSingleRow(ctx, r.sql, "SELECT COUNT(*) FROM accounts WHERE proxy_id = $1 AND deleted_at IS NULL", []any{proxyID}, &count); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *proxyRepository) ListAccountSummariesByProxyID(ctx context.Context, proxyID int64) ([]service.ProxyAccountSummary, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id, name, platform, type, notes
		FROM accounts
		WHERE proxy_id = $1 AND deleted_at IS NULL
		ORDER BY id DESC
	`, proxyID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.ProxyAccountSummary, 0)
	for rows.Next() {
		var (
			id       int64
			name     string
			platform string
			accType  string
			notes    sql.NullString
		)
		if err := rows.Scan(&id, &name, &platform, &accType, &notes); err != nil {
			return nil, err
		}
		var notesPtr *string
		if notes.Valid {
			notesPtr = &notes.String
		}
		out = append(out, service.ProxyAccountSummary{
			ID:       id,
			Name:     name,
			Platform: platform,
			Type:     accType,
			Notes:    notesPtr,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAccountCountsForProxies returns a map of proxy ID to account count for all proxies
func (r *proxyRepository) GetAccountCountsForProxies(ctx context.Context) (counts map[int64]int64, err error) {
	rows, err := r.sql.QueryContext(ctx, "SELECT proxy_id, COUNT(*) AS count FROM accounts WHERE proxy_id IS NOT NULL AND deleted_at IS NULL GROUP BY proxy_id")
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			counts = nil
		}
	}()

	counts = make(map[int64]int64)
	for rows.Next() {
		var proxyID, count int64
		if err = rows.Scan(&proxyID, &count); err != nil {
			return nil, err
		}
		counts[proxyID] = count
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// ListActiveWithAccountCount returns all active proxies with account count, sorted by creation time descending
func (r *proxyRepository) ListActiveWithAccountCount(ctx context.Context) ([]service.ProxyWithAccountCount, error) {
	proxies, err := r.client.Proxy.Query().
		Where(proxy.StatusEQ(service.StatusActive)).
		Order(dbent.Desc(proxy.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// Get account counts
	counts, err := r.GetAccountCountsForProxies(ctx)
	if err != nil {
		return nil, err
	}

	// Build result with account counts
	result := make([]service.ProxyWithAccountCount, 0, len(proxies))
	for i := range proxies {
		proxyOut := proxyEntityToService(proxies[i])
		if proxyOut == nil {
			continue
		}
		result = append(result, service.ProxyWithAccountCount{
			Proxy:        *proxyOut,
			AccountCount: counts[proxyOut.ID],
		})
	}

	return result, nil
}

func proxyEntityToService(m *dbent.Proxy) *service.Proxy {
	if m == nil {
		return nil
	}
	out := &service.Proxy{
		ID:             m.ID,
		Name:           m.Name,
		Protocol:       m.Protocol,
		Host:           m.Host,
		Port:           m.Port,
		Status:         m.Status,
		ExpiresAt:      m.ExpiresAt,
		FallbackMode:   m.FallbackMode,
		BackupProxyID:  m.BackupProxyID,
		ExpiryWarnDays: m.ExpiryWarnDays,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
	if m.Username != nil {
		out.Username = *m.Username
	}
	if m.Password != nil {
		out.Password = *m.Password
	}
	return out
}

func applyProxyEntityToService(dst *service.Proxy, src *dbent.Proxy) {
	if dst == nil || src == nil {
		return
	}
	dst.ID = src.ID
	dst.Name = src.Name
	dst.Protocol = src.Protocol
	dst.Host = src.Host
	dst.Port = src.Port
	dst.Status = src.Status
	dst.ExpiresAt = src.ExpiresAt
	dst.FallbackMode = src.FallbackMode
	dst.BackupProxyID = src.BackupProxyID
	dst.ExpiryWarnDays = src.ExpiryWarnDays
	if src.Username != nil {
		dst.Username = *src.Username
	} else {
		dst.Username = ""
	}
	if src.Password != nil {
		dst.Password = *src.Password
	} else {
		dst.Password = ""
	}
	dst.CreatedAt = src.CreatedAt
	dst.UpdatedAt = src.UpdatedAt
}
