// Command crypto-shredding stores agent session events in Postgres encrypted
// with one key per tenant per month, then deletes them by destroying keys:
// a whole tenant (offboard) or every month past retention (expire). A backup
// taken before the deletion stays readable only for what was not shredded.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

func main() {
	kmsFlag := flag.String("kms", "local", "key manager: local or gcp")
	keyRing := flag.String("keyring", "", "Cloud KMS key ring, projects/<p>/locations/<l>/keyRings/<r> (with -kms gcp)")
	localKeys := flag.String("local-keys", "kms/keys.json", "where the local KMS keeps its keys (with -kms local)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: crypto-shredding [flags] reset | write | read | offboard <tenant> | expire [-keep n] [-now YYYY-MM-DD]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/sessions"
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var keys KMS
	switch *kmsFlag {
	case "local":
		keys, err = OpenLocalKMS(*localKeys)
	case "gcp":
		keys, err = OpenCloudKMS(ctx, *keyRing)
	default:
		err = fmt.Errorf("unknown -kms %q", *kmsFlag)
	}
	if err != nil {
		log.Fatal(err)
	}
	store := NewStore(db, keys, 5*time.Minute)

	switch cmd := flag.Arg(0); cmd {
	case "reset":
		_, err = db.Exec(ctx, schema)
	case "write":
		err = write(ctx, store)
	case "read":
		err = read(ctx, store, db)
	case "offboard":
		var n int
		n, err = store.Offboard(ctx, flag.Arg(1))
		fmt.Printf("offboarded %s: destroyed %d key versions\n", flag.Arg(1), n)
	case "expire":
		fs := flag.NewFlagSet("expire", flag.ExitOnError)
		keep := fs.Int("keep", 2, "whole months to retain before the current one")
		nowFlag := fs.String("now", time.Now().Format(time.DateOnly), "today, as YYYY-MM-DD")
		fs.Parse(flag.Args()[1:])
		var now time.Time
		if now, err = time.Parse(time.DateOnly, *nowFlag); err != nil {
			break
		}
		var months []time.Time
		months, err = store.Expire(ctx, now, *keep)
		for _, m := range months {
			fmt.Printf("expired %s for every tenant\n", m.Format("2006-01"))
		}
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("KMS calls: %d\n", store.KMSCalls.Load())
}

// write stores one short session per tenant per month, September to
// November 2026. Each one has a tool call that read a secret, the way the
// sessions in the previous post did.
func write(ctx context.Context, s *Store) error {
	for _, tenant := range []string{"acme", "globex"} {
		for _, month := range []time.Month{time.September, time.October, time.November} {
			at := time.Date(2026, month, 14, 9, 30, 0, 0, time.UTC)
			session := "s-" + at.Format("2006-01")
			turns := []string{
				"user: why does the deploy fail?",
				fmt.Sprintf("tool read_file .env: DB_PASSWORD=%s-%s-hunter2", tenant, at.Format("0102")),
				"model: the password in .env does not match the database.",
			}
			for i, t := range turns {
				e := Event{Tenant: tenant, Session: session, Seq: i + 1, At: at.Add(time.Duration(i) * time.Minute), Content: []byte(t)}
				if err := s.Append(ctx, e); err != nil {
					return err
				}
			}
		}
	}
	fmt.Println("wrote 3 sessions for acme and 3 for globex")
	return nil
}

func read(ctx context.Context, s *Store, db *pgxpool.Pool) error {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT tenant_id, session_id FROM session_events ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	type key struct{ tenant, session string }
	var sessions []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.tenant, &k.session); err != nil {
			return err
		}
		sessions = append(sessions, k)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range sessions {
		events, err := s.Events(ctx, k.tenant, k.session)
		if err != nil {
			return err
		}
		for _, e := range events {
			content := string(e.Content)
			if e.Shredded {
				content = "[shredded]"
			}
			fmt.Printf("%-7s %s #%d  %s\n", e.Tenant, e.Session, e.Seq, content)
		}
	}
	return nil
}
