package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *opsRepository) BatchUpsertIngressRejects(ctx context.Context, items []*service.OpsIngressRejectAggregate) error {
	if r == nil || r.db == nil || len(items) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for start := 0; start < len(items); start += 500 {
		end := start + 500
		if end > len(items) {
			end = len(items)
		}
		valid := make([]*service.OpsIngressRejectAggregate, 0, end-start)
		for _, i := range items[start:end] {
			if i != nil && i.RequestCount > 0 {
				valid = append(valid, i)
			}
		}
		if len(valid) == 0 {
			continue
		}
		var q strings.Builder
		q.WriteString(`INSERT INTO ops_ingress_reject_aggregates (bucket_start,reject_reason,route_family,protocol,client_ip,user_id,api_key_id,request_count,first_seen,last_seen) VALUES `)
		args := make([]any, 0, len(valid)*10)
		for i, item := range valid {
			if i > 0 {
				q.WriteByte(',')
			}
			base := len(args)
			fmt.Fprintf(&q, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)", base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10)
			var uid, kid int64
			if item.UserID != nil {
				uid = *item.UserID
			}
			if item.APIKeyID != nil {
				kid = *item.APIKeyID
			}
			args = append(args, item.BucketStart.UTC(), item.RejectReason, item.RouteFamily, item.Protocol, item.ClientIP, uid, kid, item.RequestCount, item.FirstSeen.UTC(), item.LastSeen.UTC())
		}
		q.WriteString(` ON CONFLICT (bucket_start,reject_reason,route_family,protocol,client_ip,user_id,api_key_id) DO UPDATE SET request_count=ops_ingress_reject_aggregates.request_count+EXCLUDED.request_count,first_seen=LEAST(ops_ingress_reject_aggregates.first_seen,EXCLUDED.first_seen),last_seen=GREATEST(ops_ingress_reject_aggregates.last_seen,EXCLUDED.last_seen),updated_at=NOW()`)
		if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *opsRepository) ListIngressRejects(ctx context.Context, f *service.OpsIngressRejectFilter) (*service.OpsIngressRejectList, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil ops repository")
	}
	if f == nil {
		f = &service.OpsIngressRejectFilter{}
	}
	page, size := f.Page, f.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	if size > 200 {
		size = 200
	}
	clauses := []string{"1=1"}
	args := []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if f.StartTime != nil {
		add("bucket_start >= $%d", f.StartTime.UTC())
	}
	if f.EndTime != nil {
		add("bucket_start < $%d", f.EndTime.UTC())
	}
	if v := strings.TrimSpace(f.RejectReason); v != "" {
		add("reject_reason = $%d", v)
	}
	if v := strings.TrimSpace(f.RouteFamily); v != "" {
		add("route_family = $%d", v)
	}
	if v := strings.TrimSpace(f.Protocol); v != "" {
		add("protocol = $%d", v)
	}
	if v := strings.TrimSpace(f.ClientIP); v != "" {
		add("client_ip = $%d::inet", v)
	}
	if f.UserID != nil {
		add("user_id = $%d", *f.UserID)
	}
	if f.APIKeyID != nil {
		add("api_key_id = $%d", *f.APIKeyID)
	}
	where := " WHERE " + strings.Join(clauses, " AND ")
	var total int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ops_ingress_reject_aggregates"+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	args = append(args, size, (page-1)*size)
	q := fmt.Sprintf(`SELECT id,bucket_start,reject_reason,route_family,protocol,host(client_ip),user_id,api_key_id,request_count,first_seen,last_seen FROM ops_ingress_reject_aggregates%s ORDER BY bucket_start DESC,id DESC LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &service.OpsIngressRejectList{Items: make([]*service.OpsIngressRejectAggregate, 0, size), Total: total, Page: page, PageSize: size}
	for rows.Next() {
		item := &service.OpsIngressRejectAggregate{}
		var uid, kid int64
		if err := rows.Scan(&item.ID, &item.BucketStart, &item.RejectReason, &item.RouteFamily, &item.Protocol, &item.ClientIP, &uid, &kid, &item.RequestCount, &item.FirstSeen, &item.LastSeen); err != nil {
			return nil, err
		}
		if uid > 0 {
			item.UserID = &uid
		}
		if kid > 0 {
			item.APIKeyID = &kid
		}
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}
