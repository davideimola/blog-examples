package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one turn of an agent session: what the user said, what the model
// answered, what a tool returned.
type Event struct {
	Tenant   string
	Session  string
	Seq      int
	At       time.Time
	Content  []byte
	Shredded bool // the key that encrypted Content no longer exists
}

// Store wraps a Postgres session store with per-tenant, per-month encryption.
type Store struct {
	db  *pgxpool.Pool
	kms KMS

	// Unwrapped data keys stay in memory for cacheTTL so a read costs no KMS
	// call. It is also how long a shredded key can outlive its destruction
	// in a running process.
	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[dekID]cachedDEK

	KMSCalls atomic.Int64
}

type dekID struct {
	tenant string
	month  time.Time
}

type cachedDEK struct {
	key     []byte
	expires time.Time
}

func NewStore(db *pgxpool.Pool, kms KMS, cacheTTL time.Duration) *Store {
	return &Store{db: db, kms: kms, cacheTTL: cacheTTL, cache: map[dekID]cachedDEK{}}
}

func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Append encrypts the event with its tenant's key for the month and stores it.
func (s *Store) Append(ctx context.Context, e Event) error {
	month := monthOf(e.At)
	if err := s.ensurePartition(ctx, month); err != nil {
		return err
	}
	dek, err := s.dek(ctx, e.Tenant, month, true)
	if err != nil {
		return err
	}
	ct, err := seal(dek, e.Content, eventAAD(e))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`INSERT INTO session_events (tenant_id, session_id, seq, created_at, content)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.Tenant, e.Session, e.Seq, e.At, ct)
	return err
}

// Events reads a session back. Events whose key was destroyed come back with
// Shredded set and no content.
func (s *Store) Events(ctx context.Context, tenant, session string) ([]Event, error) {
	rows, err := s.db.Query(ctx,
		`SELECT seq, created_at, content FROM session_events
		 WHERE tenant_id = $1 AND session_id = $2 ORDER BY seq`, tenant, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e := Event{Tenant: tenant, Session: session}
		var ct []byte
		if err := rows.Scan(&e.Seq, &e.At, &ct); err != nil {
			return nil, err
		}
		dek, err := s.dek(ctx, tenant, monthOf(e.At), false)
		switch {
		case errors.Is(err, ErrDestroyed):
			e.Shredded = true
		case err != nil:
			return nil, err
		default:
			if e.Content, err = open(dek, ct, eventAAD(e)); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// eventAAD binds a ciphertext to the row it belongs to: copied into another
// tenant's session, or another position in this one, it no longer decrypts.
func eventAAD(e Event) []byte {
	return fmt.Appendf(nil, "%s\x00%s\x00%d", e.Tenant, e.Session, e.Seq)
}

func dekAAD(tenant string, month time.Time) []byte {
	return fmt.Appendf(nil, "%s\x00%s", tenant, month.Format("2006-01"))
}

// dek returns the tenant's data key for month, creating it when create is
// set and none exists yet.
func (s *Store) dek(ctx context.Context, tenant string, month time.Time, create bool) ([]byte, error) {
	id := dekID{tenant, month}
	s.mu.Lock()
	c, ok := s.cache[id]
	s.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.key, nil
	}

	var version string
	var wrapped []byte
	err := s.db.QueryRow(ctx,
		`SELECT kms_version, wrapped_dek FROM tenant_deks WHERE tenant_id = $1 AND month = $2`,
		tenant, month).Scan(&version, &wrapped)
	var key []byte
	switch {
	case errors.Is(err, pgx.ErrNoRows) && create:
		if key, err = s.newDEK(ctx, tenant, month); err != nil {
			return nil, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		// No key was ever made, or retention deleted it with the month.
		return nil, fmt.Errorf("%s %s: %w", tenant, month.Format("2006-01"), ErrDestroyed)
	case err != nil:
		return nil, err
	default:
		s.KMSCalls.Add(1)
		if key, err = s.kms.Unwrap(ctx, version, wrapped, dekAAD(tenant, month)); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	s.cache[id] = cachedDEK{key: key, expires: time.Now().Add(s.cacheTTL)}
	s.mu.Unlock()
	return key, nil
}

func (s *Store) newDEK(ctx context.Context, tenant string, month time.Time) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	s.KMSCalls.Add(2)
	version, err := s.kms.NewVersion(ctx, tenant)
	if err != nil {
		return nil, err
	}
	wrapped, err := s.kms.Wrap(ctx, version, key, dekAAD(tenant, month))
	if err != nil {
		return nil, err
	}
	tag, err := s.db.Exec(ctx,
		`INSERT INTO tenant_deks (tenant_id, month, kms_version, wrapped_dek)
		 VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		tenant, month, version, wrapped)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// Another writer created the month's key first: use theirs. Ours wraps
		// nothing and stays unused. Destroying it here would need the destroy
		// permission, which only the retention job holds.
		return s.dek(ctx, tenant, month, false)
	}
	return key, nil
}

func partitionName(month time.Time) string {
	return "session_events_" + month.Format("2006_01")
}

func (s *Store) ensurePartition(ctx context.Context, month time.Time) error {
	_, err := s.db.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF session_events FOR VALUES FROM ('%s') TO ('%s')`,
		partitionName(month), month.Format(time.DateOnly), month.AddDate(0, 1, 0).Format(time.DateOnly)))
	return err
}

func (s *Store) forget(match func(dekID) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.cache {
		if match(id) {
			delete(s.cache, id)
		}
	}
}
