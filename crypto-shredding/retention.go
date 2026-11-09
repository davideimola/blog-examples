package main

import (
	"context"
	"fmt"
	"time"
)

// Expire shreds every month older than keep months before now, for every
// tenant: it destroys the month's key versions, then drops the month's
// partition and key rows from the live database. The backups still hold
// those rows, encrypted under versions that no longer exist.
//
// Months inside the retention window are never touched, which is how data
// that must stay for a while is kept: the job that holds the destroy
// permission refuses to use it early.
func (s *Store) Expire(ctx context.Context, now time.Time, keep int) ([]time.Time, error) {
	cutoff := monthOf(now).AddDate(0, -keep, 0)
	rows, err := s.db.Query(ctx,
		`SELECT month, kms_version FROM tenant_deks WHERE month < $1 ORDER BY month`, cutoff)
	if err != nil {
		return nil, err
	}
	type row struct {
		month   time.Time
		version string
	}
	var expired []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.month, &r.version); err != nil {
			return nil, err
		}
		expired = append(expired, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var months []time.Time
	for _, r := range expired {
		if err := s.kms.Destroy(ctx, r.version); err != nil {
			return nil, err
		}
		if len(months) == 0 || !months[len(months)-1].Equal(r.month) {
			months = append(months, r.month)
		}
	}
	for _, m := range months {
		if _, err := s.db.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, partitionName(m))); err != nil {
			return nil, err
		}
		if _, err := s.db.Exec(ctx, `DELETE FROM tenant_deks WHERE month = $1`, m); err != nil {
			return nil, err
		}
	}
	s.forget(func(id dekID) bool { return id.month.Before(cutoff) })
	return months, nil
}

// Offboard shreds everything a tenant ever wrote, then cleans up the live
// database. The cleanup is housekeeping: destroying the versions is what
// reaches the backups.
func (s *Store) Offboard(ctx context.Context, tenant string) (int, error) {
	rows, err := s.db.Query(ctx, `SELECT kms_version FROM tenant_deks WHERE tenant_id = $1`, tenant)
	if err != nil {
		return 0, err
	}
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, v := range versions {
		if err := s.kms.Destroy(ctx, v); err != nil {
			return 0, err
		}
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM session_events WHERE tenant_id = $1`, tenant); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM tenant_deks WHERE tenant_id = $1`, tenant); err != nil {
		return 0, err
	}
	s.forget(func(id dekID) bool { return id.tenant == tenant })
	return len(versions), nil
}
