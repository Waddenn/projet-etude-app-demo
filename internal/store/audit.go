package store

import (
	"context"
	"net"
	"time"

	"github.com/Waddenn/projet-etude-app-demo/internal/model"
)

func (s *Store) WriteAudit(ctx context.Context, e model.AuditEntry) error {
	defer s.timed(ctx, "audit_write", time.Now())
	var ipText any
	if e.IP != "" {
		if ip := net.ParseIP(e.IP); ip != nil {
			ipText = ip.String()
		}
	}
	_, err := s.Pool.Exec(ctx, `
        INSERT INTO audit_log
            (user_sub, user_email, action, resource_type, resource_id, ip, user_agent, trace_id, result)
        VALUES ($1,$2,$3,$4,$5,$6::inet,$7,$8,$9)
    `, e.UserSub, nullable(e.UserEmail), e.Action,
		nullable(e.ResourceType), nullable(e.ResourceID),
		ipText, nullable(e.UserAgent), nullable(e.TraceID), string(e.Result))
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]model.AuditEntry, error) {
	defer s.timed(ctx, "audit_list", time.Now())
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
        SELECT id, ts, user_sub, COALESCE(user_email,''), action,
               COALESCE(resource_type,''), COALESCE(resource_id,''),
               COALESCE(host(ip),''), COALESCE(user_agent,''),
               COALESCE(trace_id,''), result
          FROM audit_log
         ORDER BY ts DESC
         LIMIT $1
    `, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.AuditEntry, 0, limit)
	for rows.Next() {
		var e model.AuditEntry
		var resStr string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.UserSub, &e.UserEmail,
			&e.Action, &e.ResourceType, &e.ResourceID, &e.IP,
			&e.UserAgent, &e.TraceID, &resStr); err != nil {
			return nil, err
		}
		e.Result = model.AuditResult(resStr)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeAuditOlderThan supprime les entrées plus vieilles que d. Utilisé par
// le CronJob de rétention (90 j par défaut → exigence RGPD).
func (s *Store) PurgeAuditOlderThan(ctx context.Context, d time.Duration) (int64, error) {
	defer s.timed(ctx, "audit_purge", time.Now())
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM audit_log WHERE ts < now() - $1::interval`, d.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
