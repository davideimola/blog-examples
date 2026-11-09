package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The cost of encryption on the hot path: one AES-256-GCM operation on a
// 4 KiB event, about the size of a tool result.
func BenchmarkSeal4KiB(b *testing.B) {
	key, event := make([]byte, 32), make([]byte, 4096)
	rand.Read(key)
	rand.Read(event)
	b.SetBytes(int64(len(event)))
	for b.Loop() {
		if _, err := seal(key, event, []byte("acme\x00s-1\x001")); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpen4KiB(b *testing.B) {
	key, event := make([]byte, 32), make([]byte, 4096)
	rand.Read(key)
	rand.Read(event)
	aad := []byte("acme\x00s-1\x001")
	sealed, _ := seal(key, event, aad)
	b.SetBytes(int64(len(event)))
	for b.Loop() {
		if _, err := open(key, sealed, aad); err != nil {
			b.Fatal(err)
		}
	}
}

// testStore needs a Postgres: TEST_DATABASE_URL, e.g. the one from
// compose.yaml. The schema is reset.
func testStore(t *testing.T, cacheTTL time.Duration) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	keys, err := OpenLocalKMS(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db, keys, cacheTTL), db
}

func appendSession(t *testing.T, s *Store, tenant string, at time.Time, n int) {
	t.Helper()
	for i := range n {
		e := Event{Tenant: tenant, Session: "s", Seq: i + 1, At: at, Content: []byte("DB_PASSWORD=hunter2")}
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

// With the cache, reading a 50-event session costs one KMS call. Without
// it, one per event.
func TestKMSCallsPerRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want int64
	}{
		{"cached", 5 * time.Minute, 1},
		{"uncached", 0, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testStore(t, tc.ttl)
			appendSession(t, s, "acme", time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), 50)
			s.forget(func(dekID) bool { return true })
			s.KMSCalls.Store(0)
			if _, err := s.Events(context.Background(), "acme", "s"); err != nil {
				t.Fatal(err)
			}
			if got := s.KMSCalls.Load(); got != tc.want {
				t.Errorf("KMS calls = %d, want %d", got, tc.want)
			}
		})
	}
}

// A copy of the rows taken before offboarding (a backup) does not decrypt
// once the tenant's key versions are destroyed, and the other tenant's rows
// in the same copy still do.
func TestOffboardShredsCopies(t *testing.T) {
	ctx := context.Background()
	s, db := testStore(t, time.Minute)
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	appendSession(t, s, "acme", at, 1)
	appendSession(t, s, "globex", at, 1)

	backup := map[string][]byte{}
	for _, tenant := range []string{"acme", "globex"} {
		var version string
		var wrapped []byte
		if err := db.QueryRow(ctx, `SELECT kms_version, wrapped_dek FROM tenant_deks WHERE tenant_id = $1`, tenant).Scan(&version, &wrapped); err != nil {
			t.Fatal(err)
		}
		backup[tenant] = wrapped
		backup[tenant+"/version"] = []byte(version)
	}

	if _, err := s.Offboard(ctx, "globex"); err != nil {
		t.Fatal(err)
	}

	_, err := s.kms.Unwrap(ctx, string(backup["globex/version"]), backup["globex"], dekAAD("globex", at))
	if !errors.Is(err, ErrDestroyed) {
		t.Errorf("globex key from the backup: err = %v, want ErrDestroyed", err)
	}
	if _, err := s.kms.Unwrap(ctx, string(backup["acme/version"]), backup["acme"], dekAAD("acme", at)); err != nil {
		t.Errorf("acme key from the backup: %v", err)
	}
	events, err := s.Events(ctx, "acme", "s")
	if err != nil || len(events) != 1 || !bytes.Equal(events[0].Content, []byte("DB_PASSWORD=hunter2")) {
		t.Errorf("acme still readable: events = %v, err = %v", events, err)
	}
}
